// Package router forwards OpenAI-compatible requests to the best destination
// in the mesh.
package router

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"mime/multipart"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gregbugaj/modelfabric/internal/mesh"
)

// maxJITTTLSeconds is a week: long enough for any real "keep this loaded",
// short enough that the duration arithmetic behind it cannot overflow.
const maxJITTTLSeconds = 7 * 24 * 60 * 60

// maxMultipartBytes bounds a buffered upload (audio transcription and friends).
const maxMultipartBytes = 256 << 20

// HopHeader marks a request that has already been forwarded once. A node that
// receives one serves it from a local engine or fails; it never forwards again.
// This is what keeps the mesh loop-free without any topology knowledge.
const HopHeader = "X-Fabric-Hop"

// TraceHeader carries the id that ties one client request together across the
// nodes it touches. A request forwarded from one front door to another node is
// recorded by both — twice in the merged Activity view — and without this there
// is nothing to say the two rows are the same request.
//
// It is minted at the front door the client reached, and travels with the
// forward. It is deliberately not taken from an arbitrary client, unlike the
// W3C traceparent that rides beside it — see trace.go for why the two are
// trusted differently.
const TraceHeader = "X-Fabric-Trace"

// NodeHeader and EngineHeader name the machine and engine process that served
// a response. Exported so the llm-d proxy can set the same pair after
// resolving Envoy's upstream, rather than having two spellings of them.
const (
	NodeHeader   = "X-Fabric-Node"
	EngineHeader = "X-Fabric-Engine"
)

var ErrNoCandidate = errors.New("no node in the mesh serves this model")

type Router struct {
	m   *mesh.Mesh
	log *slog.Logger
	aff *Placement // nil when prefix affinity is off
	// queue, when set, holds a request here while no engine has a slot it
	// should take. nil sends every request the moment it arrives.
	queue *Queue
	// routed counts, per node, the requests this router sent there and how
	// many of those were a conversation arriving from another engine. A move
	// is where re-reading comes from, and nothing else on the dashboard shows
	// one: in the runs of 2026-10-05 it was worked out from trajectory files.
	routedMu sync.Mutex
	routed   map[string]*routedTo

	// Bodies reports the live body-capture settings. Consulted per request so
	// the dashboard's switch takes effect immediately rather than on restart.
	// nil captures nothing.
	Bodies func() BodyLog

	// OnRoute, if set, receives one event per routed request. It carries
	// routing metadata only, never request or response content.
	OnRoute func(Event)

	// JIT, if set, is asked to load a model no node is serving, and returns
	// once it is routable. attempted is false when JIT does not apply (off,
	// or the model is unknown here), which keeps the plain 404.
	JIT func(ctx context.Context, model string, ttlSeconds int) (attempted bool, err error)

	// Stall bounds how long a request may make no progress at all before it is
	// abandoned. Zero uses DefaultStall; negative disables it.
	//
	// This is a backstop against a request that holds a slot forever, which is
	// not hypothetical: a client timed out without closing its connection, so
	// it stopped reading while the engine kept writing. TCP backpressure then
	// stalled the engine mid-generation, and every party sat still — the client
	// waiting, the engine blocked on a write, and ModelFabric blocked forwarding it.
	// The engine's only slot stayed occupied for ninety minutes, the node kept
	// advertising itself as busy, and two idle machines went unused because a
	// slot is the unit every router counts in.
	Stall time.Duration

	// MaxOutputTokens is filled into a generating request that states no limit
	// of its own. Zero leaves such a request uncapped, which means the context
	// window (see ceiling.go for what that cost).
	MaxOutputTokens int
}

// DefaultStall is deliberately far longer than any request should need. It
// bounds *silence*, not duration, and silence is normal while a prompt is being
// prefilled: 60k tokens on the Apple Silicon node in this fleet is minutes
// before the first token appears. Anything short enough to feel responsive here
// would kill legitimate long prefills, which is a worse failure than the one it
// prevents.
const DefaultStall = 15 * time.Minute

// stallErr names what happened, because the bare error is "context canceled",
// which reads like a client that hung up — the one thing this is not.
func (r *Router) stallErr(c mesh.Candidate, why string) error {
	r.log.Warn("abandoned a stalled request",
		"target", c.Name, "node", c.Node, "after", r.stall(), "why", why)
	return fmt.Errorf("no progress for %s on %s (%s); the slot was released",
		r.stall(), c.Name, why)
}

func (r *Router) stall() time.Duration {
	switch {
	case r.Stall < 0:
		return 0
	case r.Stall == 0:
		return DefaultStall
	default:
		return r.Stall
	}
}

type noJITKey struct{}

// WithoutJIT marks a request as unable to trigger a JIT load. Requests that
// arrive over the tailnet carry it: loading a model is management, and the
// mesh listener exposes none.
func WithoutJIT(ctx context.Context) context.Context {
	return context.WithValue(ctx, noJITKey{}, true)
}

// JITAllowed reports whether a request may trigger a JIT load.
func JITAllowed(ctx context.Context) bool { return ctx.Value(noJITKey{}) == nil }

// jitCandidates JIT-loads model when nothing serves it. It returns the new
// candidates, or writes the error response and returns nil. A forwarded
// request never triggers a load: the node that received it from the client
// decides, so a model is not loaded on some other machine as a side effect.
func (r *Router) jitCandidates(w http.ResponseWriter, req *http.Request, path, model string, ttl int) ([]mesh.Candidate, bool) {
	if r.JIT == nil || req.Header.Get(HopHeader) != "" || !JITAllowed(req.Context()) {
		return nil, false
	}
	// The TTL comes off the request body. It is stored as seconds and later
	// multiplied by time.Second, so an unbounded value overflows the duration
	// and can mean "never idle out" — or a negative one, "already expired".
	if ttl < 0 {
		ttl = 0
	}
	if ttl > maxJITTTLSeconds {
		ttl = maxJITTTLSeconds
	}
	start := time.Now()
	attempted, err := r.JIT(req.Context(), model, ttl)
	if !attempted {
		return nil, false
	}
	if err == nil {
		if c := r.m.Candidates(model, false); len(c) > 0 {
			return c, true
		}
		err = errors.New("loaded, but the engine is not routable yet")
	}
	r.emit(Event{Time: start, Path: path, Model: model, Status: http.StatusServiceUnavailable,
		Millis: time.Since(start).Milliseconds(), Error: "JIT load failed"})
	writeError(w, http.StatusServiceUnavailable, fmt.Sprintf("loading %q on demand failed: %v", model, err))
	return nil, true
}

// Event describes one routed request.
type Event struct {
	Time     time.Time `json:"time"`
	Path     string    `json:"path"`
	Model    string    `json:"model"`
	Node     string    `json:"node,omitempty"`
	Engine   string    `json:"engine,omitempty"`
	Local    bool      `json:"local"`
	Status   int       `json:"status"`
	Millis   int64     `json:"ms"`
	BytesOut int64     `json:"bytes_out"`
	Affine   bool      `json:"affine,omitempty"` // placed by prefix affinity
	// QueuedMillis is how long this router held the request before sending
	// it, zero when it went at once. It is part of Millis, not added to it.
	QueuedMillis int64  `json:"queued_ms,omitempty"`
	Error        string `json:"error,omitempty"`
	// Trace ties this event to the other records of the same client request —
	// the row at the front door and the row at the node that served it. Empty
	// only for an event recorded before the id was minted.
	Trace string `json:"trace,omitempty"`
	// Via names whoever chose the engine when it was not this router —
	// "llm-d" for a request ModelFabric proxied to its Envoy. Node and Engine are
	// filled in where ModelFabric can map the upstream back to an engine it knows;
	// where it cannot, they stay empty rather than being guessed at, and
	// Upstream says what was dialled.
	Via string `json:"via,omitempty"`
	// Upstream is the address the chooser reported dialling, when it named
	// one. Kept even after Node and Engine resolve: it is the ground truth
	// behind them, and the only thing left to go on when they do not.
	Upstream string `json:"upstream,omitempty"`
	// ReqBody and RespBody are the request and response as sent, truncated to
	// the configured cap, and only when body logging is turned on. Empty
	// otherwise — which is the default.
	ReqBody  string `json:"req_body,omitempty"`
	RespBody string `json:"resp_body,omitempty"`
	// Truncated says the bodies above were cut at the cap, so nobody reads a
	// clipped JSON document as a malformed one.
	Truncated bool `json:"truncated,omitempty"`
}

// BodyLog holds the body-capture settings a Router was given. The zero value
// captures nothing.
type BodyLog struct {
	Enabled bool
	Max     int
}

// DefaultBodyCap is how much of a request or response body a captured event
// keeps.
//
// 8KB cut a multimodal request off well before anything interesting: a chat
// body carrying a base64 image runs to megabytes, and 8KB did not even reach
// the end of the message list on an ordinary tool-calling exchange. 32KB holds
// a realistic prompt and the reply to it.
//
// The cost is memory, because the ring is in memory: the backlog's default 200
// events hold two bodies each, so about 13MB, and an operator who raises the
// backlog to its 2000 maximum is asking for about 128MB. Capture is off by
// default, so none of it is held until someone turns it on.
const DefaultBodyCap = 32 << 10

// Cap is Max with its default applied.
func (b BodyLog) Cap() int {
	if b.Max <= 0 {
		return DefaultBodyCap
	}
	return b.Max
}

// Clip returns at most b.Max bytes of s and whether it had to cut.
func (b BodyLog) Clip(s []byte) (string, bool) {
	if !b.Enabled || len(s) == 0 {
		return "", false
	}
	max := b.Cap()
	if len(s) > max {
		return string(s[:max]), true
	}
	return string(s), false
}

// constrainedOutput reports whether the request asks for output an engine has
// to constrain, rather than merely format. "text" and an absent field ask for
// nothing; json_object and json_schema both need grammar support, as does
// llama.cpp's own top-level grammar/json_schema.
func constrainedOutput(responseFormat, grammar string, jsonSchema json.RawMessage) bool {
	switch responseFormat {
	case "json_object", "json_schema":
		return true
	}
	return grammar != "" || len(bytes.TrimSpace(jsonSchema)) > 0
}

// carriesImage reports whether a request has an image in it.
//
// Content is a string on an ordinary text turn and an array of parts when it
// is multimodal, so it is decoded loosely: anything that fails to parse as
// parts is text, which is the safe reading — it only widens where the request
// may go, and a text request reaching a vision engine works fine. The reverse
// does not, which is why this exists.
func carriesImage(messages []struct {
	Content json.RawMessage `json:"content"`
}) bool {
	for _, m := range messages {
		var parts []struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(m.Content, &parts) != nil {
			continue
		}
		for _, p := range parts {
			switch p.Type {
			case "image_url", "image", "input_image":
				return true
			}
		}
	}
	return false
}

// CarriesImage reports whether a buffered request body has an image in it, for
// callers outside the router that have the bytes but not the decoded request —
// the front door, which has to tell a scheduler in front of it that this one
// needs an engine with a projector.
//
// A body it cannot parse is text, for the same reason as carriesImage: that
// only widens where the request may go.
func CarriesImage(body []byte) bool {
	var probe struct {
		Messages []struct {
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if json.Unmarshal(body, &probe) != nil {
		return false
	}
	return carriesImage(probe.Messages)
}

// wrongKind says why a request cannot be served by any candidate because of
// what kind of model it names: an embedding model asked to generate, or a
// chat model asked to embed. Only this node's own engines are judged; a peer's
// kind is not known here, and its router applies the same rule. Empty when
// any candidate could serve it.
func wrongKind(path, model string, cs []mesh.Candidate) string {
	embeds := path == "/v1/embeddings"
	if !embeds && !generates(path) {
		return ""
	}
	for _, c := range cs {
		if !c.Local || c.Embedding == embeds {
			return ""
		}
	}
	if len(cs) == 0 {
		return ""
	}
	if embeds {
		return fmt.Sprintf("%q is not an embedding model, so it cannot serve /v1/embeddings; load an embedding model and name it instead (`mfsh ls` lists them under EMBEDDING)", model)
	}
	return fmt.Sprintf("%q is an embedding model: it serves /v1/embeddings and cannot generate text on %s", model, path)
}

// splitVision separates candidates that can serve an image from those that
// cannot, naming the second group for the error message.
func splitVision(in []mesh.Candidate) (kept []mesh.Candidate, dropped []string) {
	for _, c := range in {
		if c.NoVision {
			dropped = append(dropped, candidateName(c))
			continue
		}
		kept = append(kept, c)
	}
	return kept, dropped
}

// candidateName is how a candidate reads in an error. A peer candidate is the
// node itself, so Name repeats Node and "helion/helion" is not useful.
func candidateName(c mesh.Candidate) string {
	name := c.Node
	if c.Name != "" && c.Name != c.Node {
		name += "/" + c.Name
	}
	return name
}

// splitConstrained separates candidates that honour constrained output from
// those that do not, naming the second group for the error message.
func splitConstrained(in []mesh.Candidate) (kept []mesh.Candidate, dropped []string) {
	for _, c := range in {
		if c.NoConstrainedDecoding {
			dropped = append(dropped, candidateName(c))
			continue
		}
		kept = append(kept, c)
	}
	return kept, dropped
}

func (r *Router) emit(e Event) {
	if r.OnRoute != nil {
		r.OnRoute(e)
	}
}

// bodyLog reads the live capture settings, or the zero value when none are set.
func (r *Router) bodyLog() BodyLog {
	if r.Bodies == nil {
		return BodyLog{}
	}
	return r.Bodies()
}

func New(m *mesh.Mesh, log *slog.Logger) *Router {
	return &Router{m: m, log: log}
}

// EnablePrefixAffinity turns on cache-aware placement. The table is bounded
// and entries expire, because engines evict their caches too.
func (r *Router) EnablePrefixAffinity() {
	r.aff = NewPlacement(nil)
}

// EngineGone tells placement that the named candidate's engine has stopped or
// been replaced, so nothing is cached there any more (see Placement.Forget).
func (r *Router) EngineGone(name string) {
	if r.aff != nil {
		r.aff.Forget(name)
	}
}

// UseNoRoomRulePlacement disables the room rule, for measuring what it is
// worth. The rest of placement is unchanged. Call after EnablePrefixAffinity.
func (r *Router) UseNoRoomRulePlacement() {
	if r.aff != nil {
		r.aff.NoRoomRule = true
	}
}

// UseHomeSlotPlacement selects the legacy slot-based affinity policy, including
// migration to much faster engines. Call after EnablePrefixAffinity.
func (r *Router) UseHomeSlotPlacement() {
	if r.aff != nil {
		r.aff.HomeSlot = true
	}
}

// EnableQueue holds requests at this router when no engine has a slot for
// them, for up to maxWait, and offers a slot to a waiting request only once it
// has stayed free for grace. See Queue. It needs prefix affinity, which is how
// the router knows whose slot a slot is.
func (r *Router) EnableQueue(grace, maxWait time.Duration) {
	r.queue = NewQueue(grace, maxWait)
}

type routedTo struct{ calls, moved int64 }

// countRoute records one request sent to node; moved says its conversation
// was last on a different engine.
func (r *Router) countRoute(node string, moved bool) {
	r.routedMu.Lock()
	defer r.routedMu.Unlock()
	if r.routed == nil {
		r.routed = map[string]*routedTo{}
	}
	t := r.routed[node]
	if t == nil {
		t = &routedTo{}
		r.routed[node] = t
	}
	t.calls++
	if moved {
		t.moved++
	}
}

// Routed reports, per node, the requests this router has sent there since it
// started and how many arrived from another engine.
func (r *Router) Routed() []mesh.RoutedTo {
	r.routedMu.Lock()
	defer r.routedMu.Unlock()
	out := make([]mesh.RoutedTo, 0, len(r.routed))
	for node, t := range r.routed {
		out = append(out, mesh.RoutedTo{Node: node, Calls: t.calls, Moved: t.moved})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Node < out[j].Node })
	return out
}

// Holding lists the requests this router is holding for a slot.
func (r *Router) Holding() []mesh.HeldRequest {
	if r.queue == nil {
		return nil
	}
	hs := r.queue.Holding()
	out := make([]mesh.HeldRequest, 0, len(hs))
	for _, h := range hs {
		out = append(out, mesh.HeldRequest{WaitedMs: h.Waited.Milliseconds(), Home: h.Home, Why: h.Why})
	}
	return out
}

// Queued reports how many requests this router is holding for a slot.
func (r *Router) Queued() int {
	if r.queue == nil {
		return 0
	}
	return r.queue.Waiting()
}

// Forward routes a JSON inference request and streams the upstream response.
func (r *Router) Forward(w http.ResponseWriter, req *http.Request, path string) {
	// Minted before anything can fail, so even a rejected request is traceable
	// and the client is told the id whatever the outcome.
	tc := TraceOf(req)
	trace := tc.TraceID
	w.Header().Set(TraceHeader, trace)
	// Also onto the request, so the dispatch that forwards to a peer carries
	// the same context without it having to be threaded through every call.
	// The traceparent written here names *this* node's span, replacing the
	// caller's, so the next hop's parent is us and not whoever called us.
	req.Header.Set(TraceHeader, trace)
	req.Header.Set(TraceParentHeader, tc.Header())

	body, err := io.ReadAll(http.MaxBytesReader(w, req.Body, 256<<20))
	if err != nil {
		writeError(w, http.StatusBadRequest, "read request body: "+err.Error())
		return
	}

	var probe struct {
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
		// TTL is LM Studio's per-request idle TTL, in seconds, for a model
		// this request causes to be JIT-loaded.
		TTL int `json:"ttl"`
		// The ways a request can ask for constrained output: OpenAI's
		// response_format, and llama.cpp's own top-level grammar/json_schema.
		// Only read to decide where the request may go; the body is forwarded
		// untouched either way.
		ResponseFormat struct {
			Type string `json:"type"`
		} `json:"response_format"`
		Grammar    string          `json:"grammar"`
		JSONSchema json.RawMessage `json:"json_schema"`
		// Messages are read only for the shape of their content: a part of
		// type image_url (OpenAI) or image (Anthropic) means the request needs
		// an engine that was loaded with its projector.
		Messages []struct {
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		writeError(w, http.StatusBadRequest, "parse request body: "+err.Error())
		return
	}
	if probe.Model == "" {
		writeError(w, http.StatusBadRequest, "request is missing the \"model\" field")
		return
	}

	forwarded := req.Header.Get(HopHeader) != ""
	candidates := r.m.Candidates(probe.Model, forwarded)
	if len(candidates) == 0 {
		var handled bool
		if candidates, handled = r.jitCandidates(w, req, path, probe.Model, probe.TTL); handled && candidates == nil {
			return
		}
	}
	if len(candidates) == 0 {
		msg := fmt.Sprintf("%v: %q", ErrNoCandidate, probe.Model)
		if forwarded {
			msg = fmt.Sprintf("forwarded request for %q but no local engine serves it", probe.Model)
		}
		// An unknown model is the most common reason a tool "doesn't work",
		// so it belongs in the traffic log as much as a success does.
		r.emit(Event{Time: time.Now(), Path: path, Model: probe.Model, Trace: trace,
			Status: http.StatusNotFound, Error: "no node serves this model"})
		writeError(w, http.StatusNotFound, msg)
		return
	}

	// An engine that ignores response_format answers a schema request with
	// free prose and a 200. Which machine is idle must not decide whether the
	// answer parses, so those engines are taken out of the running here.
	constrained := constrainedOutput(probe.ResponseFormat.Type, probe.Grammar, probe.JSONSchema)
	if constrained {
		kept, dropped := splitConstrained(candidates)
		if len(kept) == 0 {
			where := strings.Join(dropped, ", ")
			r.emit(Event{Time: time.Now(), Path: path, Model: probe.Model, Trace: trace,
				Status: http.StatusServiceUnavailable,
				Error:  "no engine serving this model honours constrained output"})
			writeError(w, http.StatusServiceUnavailable, fmt.Sprintf(
				"%q is served only by engines that ignore response_format and grammar (%s), "+
					"so a schema could not be enforced. Load it on a llama.cpp node, or drop the constraint.",
				probe.Model, where))
			return
		}
		candidates = kept
	}

	// The wrong kind of model for the endpoint. The engine's own answers were
	// a 501 and a 500 ("the current context does not logits computation"),
	// each retried on every candidate and returned as a 502 quoting a 502: it
	// read as the mesh failing, when the request could never have worked.
	if msg := wrongKind(path, probe.Model, candidates); msg != "" {
		r.emit(Event{Time: time.Now(), Path: path, Model: probe.Model, Trace: trace,
			Status: http.StatusBadRequest, Error: msg})
		writeError(w, http.StatusBadRequest, msg)
		return
	}

	// An engine loaded without its projector answers to a multimodal model's
	// name and fails any request carrying an image — llama.cpp returns
	// "failed to process mtmd chunk" for the whole request. Which machine is
	// idle must not decide whether an image can be read, so the same rule as
	// constrained output applies: take those engines out of the running.
	image := carriesImage(probe.Messages)
	if image {
		kept, dropped := splitVision(candidates)
		if len(kept) == 0 {
			where := strings.Join(dropped, ", ")
			r.emit(Event{Time: time.Now(), Path: path, Model: probe.Model, Trace: trace,
				Status: http.StatusServiceUnavailable,
				Error:  "no engine serving this model was loaded with its projector"})
			writeError(w, http.StatusServiceUnavailable, fmt.Sprintf(
				"%q is served only by engines loaded without their image projector (%s), "+
					"so the image in this request could not be read. Reload it with -vision on, "+
					"or send this request without the image.",
				probe.Model, where))
			return
		}
		candidates = kept
	}

	// Before affinity hashing, so the key describes the body that is actually
	// sent, and before dispatch so a retry cannot send a different request than
	// the first attempt did.
	if generates(path) {
		body = capOutput(body, r.MaxOutputTokens)
	}

	// The disk prompt cache places requests in the engine's shim, not here:
	// the shim is the one hop every routing method shares (see
	// engineshim.Placer), and placed here it never saw what llm-d scheduled.
	affineTo := ""
	var choice Choice
	if r.aff != nil {
		choice = r.aff.Order(candidates, probe.Model, body, r.m.Preferred())
		candidates, affineTo = choice.Candidates, choice.Target
	}
	start := time.Now()
	var onUsage func(usage)
	// Only where the request entered the mesh. A request a peer forwarded has
	// been through that peer's queue already, and was sent here for a slot.
	var queuedFor time.Duration
	var grant *Grant
	if r.queue != nil && r.aff != nil && !forwarded && generates(path) {
		// What the engine says this request used goes back to the queue: it
		// is the conversation's size, and the queue places by it.
		var used usage
		defer func() { r.queue.Done(grant, used.prompt, used.completion) }()
		onUsage = func(u usage) { used = u }
		candidates, queuedFor, grant = r.queue.Admit(req.Context(), Ask{Blocks: choice.blocks, Bytes: len(body)}, func() ([]mesh.Candidate, string) {
			// Read again each time, the first included: the queue holds its
			// lock while it does, and needs the mesh as it is now. Through
			// the same filters the first look went through. If the model has gone from
			// every node in the meantime, the last order stands and the
			// dispatch reports it.
			cs := r.m.Candidates(probe.Model, forwarded)
			if constrained {
				cs, _ = splitConstrained(cs)
			}
			if image {
				cs, _ = splitVision(cs)
			}
			if len(cs) == 0 {
				return choice.Candidates, choice.Target
			}
			choice = r.aff.Order(cs, probe.Model, body, r.m.Preferred())
			return choice.Candidates, choice.Target
		})
		affineTo = choice.Target
	}

	// Walk candidates in order so a node that died between polls costs one
	// retry rather than the request. Once bytes are on the wire we stop:
	// a half-streamed response cannot be retried safely.
	var lastErr error
	for i, c := range candidates {
		// Expose the pending prefix before dispatch so concurrent requests can
		// follow it. Failed attempts remove only their own speculative hints.
		var attempt *Attempt
		if r.aff != nil {
			attempt = r.aff.Placed(choice, c.Name)
		}
		if i == 0 && !forwarded && generates(path) {
			r.countRoute(c.Node, affineTo != "" && affineTo != c.Name)
		}
		release := c.Acquire()
		if i == 0 {
			r.queue.Sent(grant)
		}
		out, err := r.dispatch(w, req, c, body, path, attempt.Prefilled)
		release()
		if err == nil && out.status < 400 {
			attempt.Finished(out.usage.prompt, out.usage.completion)
		} else {
			attempt.Failed()
		}
		if err == nil {
			if onUsage != nil {
				onUsage(out.usage)
			}
			node, engine := c.Node, c.Name
			if out.node != "" {
				node, engine = out.node, out.engine
			}
			e := Event{Time: start, Path: path, Model: probe.Model, Node: node, Engine: engine,
				Local: c.Local, Status: out.status, Millis: time.Since(start).Milliseconds(),
				BytesOut: out.bytes, Affine: affineTo == c.Name, Trace: trace,
				QueuedMillis: queuedFor.Milliseconds()}
			// The request body is already in memory (Forward read it to find
			// the model), so capturing it costs nothing extra.
			if bl := r.bodyLog(); bl.Enabled {
				reqBody, cut1 := bl.Clip(body)
				respBody, cut2 := bl.Clip(out.body)
				e.ReqBody, e.RespBody, e.Truncated = reqBody, respBody, cut1 || cut2
			}
			r.emit(e)
			return
		}
		wrote := out.committed
		lastErr = err
		if wrote {
			r.log.Error("upstream failed mid-response",
				"model", probe.Model, "target", c.Name, "err", err)
			return
		}
		r.log.Warn("candidate failed, trying next",
			"model", probe.Model, "target", c.Name, "attempt", i+1, "err", err)
	}
	r.emit(Event{Time: start, Path: path, Model: probe.Model, Trace: trace, Status: http.StatusBadGateway,
		Millis: time.Since(start).Milliseconds(), Error: fmt.Sprint(lastErr)})
	writeError(w, http.StatusBadGateway,
		fmt.Sprintf("all %d candidate(s) for %q failed: %v", len(candidates), probe.Model, lastErr))
}

// result describes what reached the client. Any bytes written make a failure
// unretryable, since a half-sent response cannot be replayed.
type result struct {
	status    int
	bytes     int64
	committed bool // headers sent: the response can no longer be retried
	// body holds the response as streamed, up to the capture cap, and only
	// when body logging is on. Nil otherwise.
	body []byte
	// usage is what the engine said the request used, zero when the response
	// did not say.
	usage usage
	// node and engine are who actually served it. For a local engine that is
	// the candidate; for a peer it is what that peer reported, since only it
	// knows which of its engines took the request.
	node, engine string
}

// dispatch sends the request to one candidate.
func (r *Router) dispatch(w http.ResponseWriter, req *http.Request, c mesh.Candidate, body []byte, path string, prefilled func()) (result, error) {
	return r.dispatchRaw(w, req, c, servedBody(c, body), path, "application/json", prefilled)
}

// servedBody rewrites the request's "model" to the id the chosen engine
// answers to, for an engine that cannot be told to answer to ours (mlx-lm; see
// mesh.Engine.Served). Everything else in the body is passed through
// untouched, and a body we cannot parse is left alone — the engine's own error
// is better than one invented here.
func servedBody(c mesh.Candidate, body []byte) []byte {
	if c.ServedModel == "" {
		return body
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return body
	}
	served, err := json.Marshal(c.ServedModel)
	if err != nil {
		return body
	}
	if bytes.Equal(fields["model"], served) {
		return body
	}
	fields["model"] = served
	out, err := json.Marshal(fields)
	if err != nil {
		return body
	}
	return out
}

func (r *Router) dispatchRaw(w http.ResponseWriter, req *http.Request, c mesh.Candidate, body []byte, path, contentType string, prefilled func()) (res result, err error) {
	start := time.Now()
	// The upstream call hangs off a context this function can cancel, so a
	// request that goes silent is abandoned rather than held. Cancelling closes
	// the connection to the engine, which is what actually stops the
	// generation and frees the slot.
	ctx := req.Context()
	var (
		stalled  atomic.Bool
		watchdog *time.Timer
	)
	if stall := r.stall(); stall > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithCancel(ctx)
		defer cancel()
		watchdog = time.AfterFunc(stall, func() { stalled.Store(true); cancel() })
		defer watchdog.Stop()
	}
	// Progress, not elapsed time: every byte in either direction earns the full
	// window again, so a slow request is left alone and a dead one is not.
	progress := func() {
		if watchdog != nil {
			watchdog.Reset(r.stall())
		}
	}
	out, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+path, bytes.NewReader(body))
	if err != nil {
		return res, err
	}
	copyRequestHeaders(out.Header, req.Header)
	out.Header.Set("Content-Type", contentType)
	if !c.Local {
		out.Header.Set(HopHeader, "1")
		// Beside the hop marker, and only there: the receiving node honours a
		// trace only on a hop, so the two must travel together.
		if t := req.Header.Get(TraceHeader); validTrace(t) {
			out.Header.Set(TraceHeader, t)
		}
	}

	// The inference client, not the mesh's probe client: that one gives up if
	// headers have not arrived in 120s, and a request that does not stream
	// sends none until the answer is complete. The watchdog above is what
	// bounds a silent engine.
	resp, err := r.m.InferenceClient().Do(out)
	if err != nil {
		return res, err
	}
	defer resp.Body.Close()

	// A 5xx from a peer means try elsewhere; a 4xx is the client's fault and
	// retrying on another node would produce the same answer.
	if resp.StatusCode >= 500 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return res, fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(snippet)))
	}

	// A peer answers with its own idea of who served the request, which for a
	// forwarded request is better than ours: we picked the node, it picked the
	// engine. Read before the copy, since the copy skips them.
	peerNode, peerEngine := resp.Header.Get(NodeHeader), resp.Header.Get(EngineHeader)
	for k, vs := range resp.Header {
		lower := strings.ToLower(k)
		// Node and engine are decided below. The trace is already on the
		// response, set at the front door — copying the peer's echo of it
		// would send the same header twice.
		if hopByHop[lower] || lower == strings.ToLower(NodeHeader) ||
			lower == strings.ToLower(EngineHeader) || lower == strings.ToLower(TraceHeader) {
			continue
		}
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	res.node, res.engine = c.Node, c.Name
	if !c.Local && peerNode != "" {
		// c.Name for a peer is the node, not an engine: this node cannot know
		// which engine the peer would choose, and reporting the node as the
		// engine told the client something untrue.
		res.node = peerNode
		res.engine = peerEngine
	}
	w.Header().Set(NodeHeader, res.node)
	if res.engine != "" {
		w.Header().Set(EngineHeader, res.engine)
	}
	w.WriteHeader(resp.StatusCode)
	res.status, res.committed = resp.StatusCode, true

	rc := http.NewResponseController(w)
	buf := make([]byte, 32<<10)
	// The end of the response, kept for its usage block (see readUsage).
	var tail []byte
	var stream prefillStream
	isStream := strings.HasPrefix(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream") && resp.StatusCode < 400
	defer func() { res.usage = readUsage(tail) }()
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			tail = keepTail(tail, buf[:n])
			if isStream && prefilled != nil && stream.Add(buf[:n]) {
				prefilled()
				prefilled = nil
			}
			progress()
			// A write deadline, because the blocked party in the incident this
			// guards against was the write: the client had stopped reading, so
			// w.Write never returned and no amount of watching the read side
			// would have noticed. Set per chunk, so it bounds this write rather
			// than the whole response.
			if stall := r.stall(); stall > 0 {
				_ = rc.SetWriteDeadline(time.Now().Add(stall))
			}
			written, werr := w.Write(buf[:n])
			res.bytes += int64(written)
			// Bounded by the cap: a long stream is clipped, not accumulated.
			if bl := r.bodyLog(); bl.Enabled && len(res.body) < bl.Cap() {
				res.body = append(res.body, buf[:n]...)
			}
			if werr != nil {
				if stalled.Load() {
					return res, r.stallErr(c, "the caller stopped reading")
				}
				return res, werr
			}
			progress()
			// Flush every chunk: SSE deltas are worthless if they are buffered.
			_ = rc.Flush()
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			if stalled.Load() {
				return res, r.stallErr(c, "the engine sent nothing")
			}
			return res, rerr
		}
	}

	r.log.Info("routed",
		"path", path, "target", c.Name, "node", c.Node,
		"local", c.Local, "status", resp.StatusCode, "took", time.Since(start).Round(time.Millisecond))
	return res, nil
}

var hopByHop = map[string]bool{
	"connection": true, "keep-alive": true, "proxy-authenticate": true,
	"proxy-authorization": true, "te": true, "trailer": true,
	"transfer-encoding": true, "upgrade": true, "content-length": true,
}

// clientCredentials are the caller's own secrets. An engine does not need
// them — ModelFabric gives an engine its own key when one is required — and a peer
// must never be handed the end user's credential just because it was chosen
// to serve the request. They stop at this node.
var clientCredentials = map[string]bool{
	"authorization": true, "cookie": true, "x-api-key": true, "api-key": true,
}

func copyRequestHeaders(dst, src http.Header) {
	for k, vs := range src {
		lk := strings.ToLower(k)
		if hopByHop[lk] || lk == "host" || clientCredentials[lk] {
			continue
		}
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
}

func writeError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{"message": msg, "type": "modelfabric_error", "code": code},
	})
}

// ForwardMultipart routes a multipart request such as an audio transcription.
//
// The model arrives as a form field rather than a JSON key, so the body is
// buffered, parsed for that one field, and then replayed verbatim upstream —
// re-encoding it would risk changing boundaries or dropping file parts.
func (r *Router) ForwardMultipart(w http.ResponseWriter, req *http.Request, path string) {
	tc := TraceOf(req)
	trace := tc.TraceID
	w.Header().Set(TraceHeader, trace)
	req.Header.Set(TraceHeader, trace)
	req.Header.Set(TraceParentHeader, tc.Header())

	// Buffered in memory to be forwarded, so the cap is what a node can hold,
	// not what an upload might be: 1 GiB times a few concurrent transcriptions
	// is the node's RAM.
	body, err := io.ReadAll(http.MaxBytesReader(w, req.Body, maxMultipartBytes))
	if err != nil {
		writeError(w, http.StatusBadRequest, "read request body: "+err.Error())
		return
	}

	model, err := multipartModel(req.Header.Get("Content-Type"), body)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	forwarded := req.Header.Get(HopHeader) != ""
	candidates := r.m.Candidates(model, forwarded)
	if len(candidates) == 0 {
		var handled bool
		if candidates, handled = r.jitCandidates(w, req, path, model, 0); handled && candidates == nil {
			return
		}
	}
	if len(candidates) == 0 {
		writeError(w, http.StatusNotFound, fmt.Sprintf("%v: %q", ErrNoCandidate, model))
		return
	}

	var lastErr error
	for _, c := range candidates {
		release := c.Acquire()
		out, err := r.dispatchRaw(w, req, c, body, path, req.Header.Get("Content-Type"), nil)
		release()
		if err == nil {
			node, engine := c.Node, c.Name
			if out.node != "" {
				node, engine = out.node, out.engine
			}
			r.emit(Event{Time: time.Now(), Path: path, Model: model, Node: node, Engine: engine,
				Local: c.Local, Status: out.status, BytesOut: out.bytes, Trace: trace})
			return
		}
		wrote := out.committed
		lastErr = err
		if wrote {
			r.log.Error("upstream failed mid-response", "model", model, "target", c.Name, "err", err)
			return
		}
	}
	writeError(w, http.StatusBadGateway, fmt.Sprintf("all candidates for %q failed: %v", model, lastErr))
}

// multipartModel pulls the "model" field out of a multipart body without
// disturbing it.
func multipartModel(contentType string, body []byte) (string, error) {
	mediaType, params, err := mime.ParseMediaType(contentType)
	if err != nil || !strings.HasPrefix(mediaType, "multipart/") {
		return "", fmt.Errorf("expected a multipart body, got %q", contentType)
	}
	boundary, ok := params["boundary"]
	if !ok {
		return "", fmt.Errorf("multipart body has no boundary")
	}
	mr := multipart.NewReader(bytes.NewReader(body), boundary)
	for {
		part, err := mr.NextPart()
		if err != nil {
			break
		}
		if part.FormName() == "model" {
			// Bounded read: this field is a short identifier, never a file.
			v, err := io.ReadAll(io.LimitReader(part, 1<<12))
			part.Close()
			if err != nil {
				return "", err
			}
			return strings.TrimSpace(string(v)), nil
		}
		part.Close()
	}
	return "", fmt.Errorf(`request is missing the "model" form field`)
}
