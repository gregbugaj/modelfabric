// Package mesh tracks local inference engines and peer ModelFabric nodes.
//
// Discovery is probe-based rather than registry-based: we ask the local
// tailscaled for its peer list, then probe each online peer on the agreed mesh
// port. A peer that answers /z/state is in the mesh. Nothing is uploaded
// anywhere and there is no central server to run.
package mesh

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"runtime"
	"slices"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gregbugaj/modelfabric/internal/config"
	"github.com/gregbugaj/modelfabric/internal/engineshim"
	"github.com/gregbugaj/modelfabric/internal/osproc"
	// Aliased: this file also uses the standard library runtime for Platform().
	engines "github.com/gregbugaj/modelfabric/internal/runtime"
	"github.com/gregbugaj/modelfabric/internal/tscli"
)

// NodeState is what a node publishes to its peers at /z/state.
type NodeState struct {
	Node string `json:"node"`
	Addr string `json:"addr,omitempty"`
	// Platform is the OS and architecture this node's ModelFabric runs on
	// ("linux/amd64", "darwin/arm64"). The fleet is deliberately mixed, and
	// what a node can run follows from it: MLX and Metal need a Mac, llm-d
	// needs Linux. Empty from a peer too old to report it.
	Platform string `json:"platform,omitempty"`
	// OSVersion is what the operating system calls itself ("macOS 26.6.2",
	// "Ubuntu 24.04.1 LTS") — the detail Tailscale's own node list does not
	// carry, and what tells two Linux boxes apart.
	OSVersion string   `json:"os_version,omitempty"`
	Models    []string `json:"models"`
	Inflight  int64    `json:"inflight"`
	// Accepted is what this node's front door is holding: requests it took and
	// has not answered. Every request enters here whoever routes it afterwards,
	// so this is the one in-flight figure ModelFabric always knows — an engine count
	// cannot see a request still queued in Envoy, and under llm-d cannot see
	// one it dispatched either.
	Accepted int64 `json:"accepted,omitempty"`
	// Scheduler is llm-d on this node, when it is scheduling a model for the
	// mesh. Every node advertises its own, because the node that schedules is
	// not always the node you are looking at: with an entrypoint it is a
	// machine with no GPUs at all, and a dashboard that only knows its own
	// llm-d reported "not running" while a benchmark ran entirely through one.
	Scheduler *SchedulerState `json:"scheduler,omitempty"`
	Engines   []EngineState   `json:"engines"`
	Instances []InstanceState `json:"instances,omitempty"`
	Updated   time.Time       `json:"updated"`
}

// SchedulerState is llm-d as one node is running it.
type SchedulerState struct {
	Model   string `json:"model"`
	Profile string `json:"profile"`
	// Engines is how many it can place on, which is the difference between
	// "scheduling" and "scheduling nothing".
	Engines int `json:"engines"`
}

// Platform is this node's OS and architecture, as advertised to peers. What a
// node can run follows from it: MLX and Metal need a Mac, llm-d needs Linux.
func Platform() string { return runtime.GOOS + "/" + runtime.GOARCH }

type EngineState struct {
	Name    string   `json:"name"`
	Healthy bool     `json:"healthy"`
	Models  []string `json:"models"`
	EngineStats
	// Slots is how many requests the engine runs at once (llama.cpp's
	// --parallel); zero when unknown. A request beyond it queues.
	Slots int64  `json:"slots,omitempty"`
	Error string `json:"error,omitempty"`
}

// EngineStats is the common set of measurements advertised for an engine and
// its supervised instance. Embedding keeps the peer protocol's flat JSON keys.
type EngineStats struct {
	Inflight int64 `json:"inflight"`
	// ContextLength is how many tokens one conversation may use here, and
	// KVPoolTokens what the engine holds in total (that context once per slot).
	// Zero means unknown, not unlimited: a caller that cannot compute headroom
	// must say nothing rather than something reassuring.
	//
	// Published because nothing reading the mesh could say how close a
	// conversation was to the limit. The only tasks a 2026-09-24 benchmark run
	// failed to solve died exactly there, at ~130,900 of 131,072, with no
	// warning anywhere.
	ContextLength int `json:"context_length,omitempty"`
	KVPoolTokens  int `json:"kv_pool_tokens,omitempty"`
	// ContextNote explains a disagreement between what ModelFabric loaded and what the
	// engine reports. Empty normally, which is what makes it worth surfacing.
	ContextNote string `json:"context_note,omitempty"`
	// PrefillTokS is this engine's measured prompt-processing rate; zero until
	// it has served enough prompt tokens to measure. Engines differ widely
	// across a mixed fleet, so it is per engine, never pooled.
	PrefillTokS float64 `json:"prefill_tok_s,omitempty"`
	// DecodeTokS is generated tokens per second, and SpecAccepted the share of
	// drafted tokens kept (-1 when not speculating). Prefill is the reading
	// half of a turn; these are the writing half.
	DecodeTokS   float64 `json:"decode_tok_s,omitempty"`
	SpecAccepted float64 `json:"spec_accepted,omitempty"`
	// PrefillTrusted says the rate is settled enough to route by. Below it the
	// rate is still reported so a reader sees something, but nothing places
	// traffic on it.
	PrefillTrusted bool `json:"prefill_trusted,omitempty"`
	// PromptTokens, CachedTokens and OutputTokens are lifetime totals: how
	// much of the fleet's work this engine was given, which is what a
	// placement decision produces and a rate cannot show.
	PromptTokens int64 `json:"prompt_tokens,omitempty"`
	CachedTokens int64 `json:"cached_tokens,omitempty"`
	OutputTokens int64 `json:"output_tokens,omitempty"`
	// KVUsage is the share of this engine's KV pool in use, -1 when the engine
	// cannot be asked. See internal/engineshim for what it counts.
	KVUsage float64 `json:"kv_usage"`
	// LoadAvg is the mean requests in flight over mesh.LoadWindow, -1 before
	// there are enough samples. The instant says what an engine is doing; this
	// says how work was shared, which on a mixed fleet is the question worth
	// asking — an even share of requests to an engine a ninth as fast is not
	// an even share of work.
	LoadAvg float64 `json:"load_avg"`
}

// Engine is a local OpenAI-compatible server this node fronts.
type Engine struct {
	Name    string
	BaseURL string
	// via, when set, is where the router sends requests instead of BaseURL:
	// the engine's shim, which holds the disk prompt cache. BaseURL stays the
	// engine itself, for health polls and for measuring it (tune, bench),
	// which must not go through a cache.
	via atomic.Pointer[string]
	// Served is the model id this engine answers to, when it differs from the
	// catalog key: mlx-lm dispatches on the request's "model" field and only
	// recognises its own name for the model it was started with. Empty means
	// the engine answers to the catalog key, as llama.cpp does via --alias.
	Served string
	// FixedModels and NoMetrics mirror the engine's runtime traits: what this
	// engine cannot be asked. The zero values describe llama.cpp, so an engine
	// registered without them behaves exactly as before. KVFromSlots says its
	// KV utilization must be computed from /slots rather than read from a
	// gauge.
	FixedModels bool
	NoMetrics   bool
	KVFromSlots bool
	// NoConstrainedDecoding says this engine ignores response_format and
	// grammar instead of honouring them (mlx-lm). The router keeps requests
	// that ask for constrained output away from it.
	NoConstrainedDecoding bool
	// NoVision says this engine was loaded without its projector (-vision
	// off), so it answers to a multimodal model's name but fails any request
	// carrying an image. The router keeps those requests away from it, the
	// same way and for the same reason.
	NoVision bool

	// Embedding says this engine was started to embed (--embeddings): it
	// serves /v1/embeddings and fails anything that generates text. Known for
	// this node's own engines; a peer applies the same rule to its own.
	Embedding bool

	mu      sync.RWMutex
	models  []string
	healthy bool
	lastErr string

	inflight atomic.Int64
	// lastUsed is when a request last started or finished here, in unix
	// nanoseconds; zero means never. Idle TTLs are measured from it.
	lastUsed atomic.Int64
	// slots is the engine's concurrent request capacity; zero is unknown.
	slots atomic.Int64
	// prefill is the measured prompt tokens per second (a float64's bits).
	prefill atomic.Uint64
	// decode is the measured generated tokens per second, and specAccepted the
	// share of drafted tokens kept (a float64's bits). Prefill describes the
	// reading half of an agent's turn; these describe the writing half, which
	// is the half speculative decoding moves.
	decode       atomic.Uint64
	specAccepted atomic.Uint64
	// Lifetime token counters, for how much of the fleet's work this engine
	// was given. A rate says how fast it is; these say what it did.
	// askedContext is the per-request context ModelFabric loaded this engine with, and
	// enginePool what the engine reports holding in total. contextLen is the two
	// reconciled (context.go); contextNote explains a disagreement. Zero means
	// nobody has said yet.
	askedContext atomic.Int64
	enginePool   atomic.Int64
	contextLen   atomic.Int64
	contextNote  atomic.Value // string
	// propsAsked stops the /props probe repeating: an engine's context cannot
	// change without a reload, and a reload makes a new Engine.
	propsAsked atomic.Bool

	prefillTrusted atomic.Bool
	promptTokens   atomic.Int64
	cachedTokens   atomic.Int64
	outputTokens   atomic.Int64
	// observedInflight is what the engine itself reports as in flight, plus one
	// so that a real zero is distinguishable from "never measured". ModelFabric's own
	// inflight counter only sees what its router dispatched, and under llm-d
	// that is nothing.
	observedInflight atomic.Int64
	// load is a short history of in-flight samples, for the rolling average.
	// Guarded by its own mutex: it is written from the poll loop and read by
	// every state render, and neither should wait on the model list.
	loadMu sync.Mutex
	load   []loadSample
	// kvUsage is the last measured KV-cache utilization (a float64's bits),
	// valid only once kvKnown is set: an idle engine measures a true zero,
	// which must not read the same as never having been measured.
	kvUsage atomic.Uint64
	kvKnown atomic.Bool
}

// SetSlots records how many requests the engine runs at once.
func (e *Engine) SetSlots(n int) { e.slots.Store(int64(n)) }

// SetAskedContext records the per-request context ModelFabric loaded this engine
// with. It is what was asked for, not necessarily what the engine did.
func (e *Engine) SetAskedContext(tokens int) {
	e.askedContext.Store(int64(tokens))
	e.reconcile()
}

// SetEnginePool records the total KV capacity the engine reports.
func (e *Engine) SetEnginePool(tokens int) {
	e.enginePool.Store(int64(tokens))
	e.reconcile()
}

func (e *Engine) reconcile() {
	perRequest, note := reconcileContext(
		int(e.askedContext.Load()), int(e.slots.Load()), int(e.enginePool.Load()))
	e.contextLen.Store(int64(perRequest))
	e.contextNote.Store(note)
}

// Context is the per-request context a conversation on this engine may use, the
// engine's total KV pool, and an explanation when those disagree. Zero means
// unknown rather than unlimited, which is the distinction a caller has to make:
// a headroom warning it cannot compute must be absent, not reassuring.
func (e *Engine) Context() (perRequest, pool int, note string) {
	n, _ := e.contextNote.Load().(string)
	return int(e.contextLen.Load()), int(e.enginePool.Load()), n
}

// Slots reports the engine's concurrent request capacity; zero is unknown.
func (e *Engine) Slots() int64 { return e.slots.Load() }

// PrefillRate reports the measured prompt tokens per second; zero until the
// engine has served enough to measure at all. It may be rough — see
// TrustedPrefillRate for the one placement uses.
func (e *Engine) PrefillRate() float64 { return math.Float64frombits(e.prefill.Load()) }

// TrustedPrefillRate is the rate only once the engine has processed enough
// prompt for it to decide where traffic goes, else zero. Routing and the label
// llm-d schedules by read this one: a figure from a few short prompts would
// place real traffic on the strength of nothing.
func (e *Engine) TrustedPrefillRate() float64 {
	if !e.prefillTrusted.Load() {
		return 0
	}
	return e.PrefillRate()
}

// PrefillTrusted reports whether the rate is settled enough to route by.
func (e *Engine) PrefillTrusted() bool { return e.prefillTrusted.Load() }

// DecodeRate reports the measured generated tokens per second; zero until the
// engine has generated anything.
func (e *Engine) DecodeRate() float64 { return math.Float64frombits(e.decode.Load()) }

// SpecAccepted reports the share of drafted tokens the model kept, 0..1, or -1
// when the engine is not speculating or has not been measured.
func (e *Engine) SpecAccepted() float64 {
	v := e.specAccepted.Load()
	if v == 0 {
		return -1
	}
	return math.Float64frombits(v)
}

// Tokens reports this engine's lifetime prompt, cached and output totals.
func (e *Engine) Tokens() (prompt, cached, output int64) {
	return e.promptTokens.Load(), e.cachedTokens.Load(), e.outputTokens.Load()
}

// SetRates records what the engine's counters said.
func (e *Engine) SetRates(r EngineRates) {
	e.prefill.Store(math.Float64bits(r.PrefillTokS))
	e.prefillTrusted.Store(r.Trusted)
	e.decode.Store(math.Float64bits(r.DecodeTokS))
	if r.SpecAccepted >= 0 {
		e.specAccepted.Store(math.Float64bits(r.SpecAccepted))
	}
	e.promptTokens.Store(r.PromptTokens)
	e.cachedTokens.Store(r.CachedTokens)
	e.outputTokens.Store(r.OutputTokens)
}

// SetKVUsage records the engine's KV-cache utilization.
func (e *Engine) SetKVUsage(v float64) {
	e.kvUsage.Store(math.Float64bits(v))
	e.kvKnown.Store(true)
}

// KVUsage reports the last measured KV-cache utilization, and whether it has
// been measured at all — an engine nobody could ask is not an idle one.
func (e *Engine) KVUsage() (float64, bool) {
	if !e.kvKnown.Load() {
		return 0, false
	}
	return math.Float64frombits(e.kvUsage.Load()), true
}

// loadSample is one in-flight reading.
type loadSample struct {
	at time.Time
	n  int64
}

// LoadWindow is how far back the rolling average looks.
//
// Two minutes at the default 2s poll is about 60 samples: enough that one
// burst does not dominate, short enough to still show a fleet that has just
// gone idle. It matches the window the dashboard used to compute for itself,
// which it could only do while the page was open — the number that answers
// "has this engine carried an even share" has to outlive a page load.
const LoadWindow = 2 * time.Minute

// SetObservedInflight records what the engine says it is serving.
func (e *Engine) SetObservedInflight(n int) {
	e.observedInflight.Store(int64(n) + 1)
	e.recordLoad(int64(n))
}

func (e *Engine) recordLoad(n int64) {
	now := time.Now()
	e.loadMu.Lock()
	defer e.loadMu.Unlock()
	e.load = append(e.load, loadSample{at: now, n: n})
	cut := 0
	for cut < len(e.load) && now.Sub(e.load[cut].at) > LoadWindow {
		cut++
	}
	// Re-slice onto the front rather than keep a growing backing array: this
	// appends every poll for as long as the node runs.
	if cut > 0 {
		e.load = append(e.load[:0], e.load[cut:]...)
	}
}

// LoadAvg is the mean requests in flight over LoadWindow, and whether there
// were enough samples to mean anything. Two readings of a two-minute window
// is not an average.
func (e *Engine) LoadAvg() (float64, bool) {
	e.loadMu.Lock()
	defer e.loadMu.Unlock()
	if len(e.load) < 3 {
		return 0, false
	}
	var sum int64
	for _, s := range e.load {
		sum += s.n
	}
	return float64(sum) / float64(len(e.load)), true
}

// Inflight reports how many requests this engine is serving right now.
//
// The engine's own count wins when ModelFabric has one: it counts every request,
// including those llm-d sent straight to the engine, where ModelFabric's router
// counter reads zero however busy the engine is. Without it — an engine that
// publishes nothing, or one not yet polled — the router's own count stands.
func (e *Engine) Inflight() int64 {
	if v := e.observedInflight.Load(); v > 0 {
		return v - 1
	}
	return e.inflight.Load()
}

// LastUsed reports when a request last started or finished on this engine.
func (e *Engine) LastUsed() time.Time {
	if n := e.lastUsed.Load(); n != 0 {
		return time.Unix(0, n)
	}
	return time.Time{}
}

func (e *Engine) snapshot() EngineState {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return EngineState{
		Name:        e.Name,
		Healthy:     e.healthy,
		Models:      append([]string(nil), e.models...),
		EngineStats: e.Stats(),
		Slots:       e.slots.Load(),
		Error:       e.lastErr,
	}
}

// Stats reads the same measurements for local engine and supervised-instance
// reports, including the sentinels that distinguish unknown from idle.
func (e *Engine) Stats() EngineStats {
	prompt, cached, output := e.Tokens()
	perRequest, pool, note := e.Context()
	return EngineStats{
		// The engine's observed count includes llm-d traffic that bypasses the
		// router. Using its raw counter made the local Serving page read 0/2
		// while peers correctly showed the same engine as busy.
		Inflight:       e.Inflight(),
		ContextLength:  perRequest,
		KVPoolTokens:   pool,
		ContextNote:    note,
		PrefillTokS:    e.PrefillRate(),
		DecodeTokS:     e.DecodeRate(),
		SpecAccepted:   e.SpecAccepted(),
		PrefillTrusted: e.PrefillTrusted(),
		PromptTokens:   prompt,
		CachedTokens:   cached,
		OutputTokens:   output,
		KVUsage:        kvOrUnknown(e),
		LoadAvg:        loadOrUnknown(e),
	}
}

// loadOrUnknown reports the engine's rolling load, or -1 when it has not been
// sampled enough — zero would read as "idle for two minutes".
func loadOrUnknown(e *Engine) float64 {
	if v, ok := e.LoadAvg(); ok {
		return v
	}
	return -1
}

// kvOrUnknown reports the engine's KV usage, or -1 when it has none to give —
// JSON has no way to say "not measured", and zero would read as an idle cache.
func kvOrUnknown(e *Engine) float64 {
	if v, ok := e.KVUsage(); ok {
		return v
	}
	return -1
}

func (e *Engine) has(model string) bool {
	e.mu.RLock()
	defer e.mu.RUnlock()
	if !e.healthy {
		return false
	}
	for _, m := range e.models {
		if m == model {
			return true
		}
	}
	return false
}

// Peer is another ModelFabric node reachable over the tailnet.
type Peer struct {
	Node    string
	Addr    string
	BaseURL string

	mu        sync.RWMutex
	platform  string
	osVersion string
	models    []string
	instances []InstanceState
	engines   []EngineState
	reported  int64
	// accepted and scheduler are what the peer said about itself: what its
	// front door is holding, and whether it is scheduling for the mesh.
	accepted  int64
	scheduler *SchedulerState
	lastSeen  time.Time
	alive     bool

	// pending counts requests dispatched since the last successful poll, so a
	// burst between polls is not invisible to the router.
	pending atomic.Int64
}

// identity returns the peer's name and base URL under its own lock, which is
// what refreshPeers takes when it updates them.
func (p *Peer) identity() (string, string) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.Node, p.BaseURL
}

func (p *Peer) has(model string) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if !p.alive {
		return false
	}
	for _, m := range p.models {
		if m == model {
			return true
		}
	}
	return false
}

// capacity is the peer's engines serving model: requests in flight on them
// (as last reported, plus what we have sent since), their total slots, and the
// fastest measured prefill rate. Zero slots means the peer did not say — an
// older ModelFabric — and is treated as unknown, never as full.
func (p *Peer) capacity(model string) (inflight, slots int64, rate float64) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	for _, e := range p.engines {
		if !e.Healthy || !slices.Contains(e.Models, model) {
			continue
		}
		inflight += e.Inflight
		slots += e.Slots
		rate = max(rate, e.PrefillTokS)
	}
	return inflight + p.pending.Load(), slots, rate
}

// constrains reports whether this peer has any engine for the model that
// honours response_format and grammar.
//
// A peer reports the engine family behind each instance, and an instance from
// a node too old to say it is llama.cpp by history — which does honour them,
// so the optimistic reading is also the backwards-compatible one. A peer with
// both kinds is capable: forwarding reaches its router, which applies this
// same rule to its own engines.
// refusesImages reports whether every ready instance of this model on the peer
// declined its projector.
//
// A model loaded -vision off answers to the same name and fails an image
// request outright, so a request carrying one must not be sent there because
// that node happened to be idle.
func (p *Peer) refusesImages(model string) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	saw := false
	for _, i := range p.instances {
		if i.Model != model || i.State != "ready" {
			continue
		}
		saw = true
		if !i.VisionOff {
			return false // one engine here kept its projector
		}
	}
	// Nothing ready, or a peer too old to report instances: it has not said it
	// declined anything, and claiming otherwise would strand image traffic on
	// a mesh that has always carried it.
	return saw
}

func (p *Peer) constrains(model string) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	saw := false
	for _, i := range p.instances {
		if i.Model != model || i.State != "ready" {
			continue
		}
		saw = true
		if i.Engine != EngineMLX {
			return true
		}
	}
	// No instance list at all (an older peer, or one that reports only
	// engines) says nothing about the engines behind it, so it is not held
	// against them.
	return !saw
}

// load is the router's view of how busy a peer is: what it last reported, plus
// what we have sent it since.
func (p *Peer) load() int64 {
	p.mu.RLock()
	r := p.reported
	p.mu.RUnlock()
	return r + p.pending.Load()
}

// PeerView is the peer state rendered for /z/mesh.
type PeerView struct {
	Node string `json:"node"`
	Addr string `json:"addr"`
	// BaseURL is the peer's mesh listener, which serves inference to other
	// nodes on the tailnet.
	BaseURL string `json:"base_url"`
	// Platform and OSVersion are the peer's, as it reports them (see NodeState).
	Platform  string          `json:"platform,omitempty"`
	OSVersion string          `json:"os_version,omitempty"`
	Instances []InstanceState `json:"instances,omitempty"`
	Alive     bool            `json:"alive"`
	Models    []string        `json:"models"`
	Inflight  int64           `json:"inflight"`
	// Accepted is what this node's front door is holding: requests it took and
	// has not answered. Every request enters here whoever routes it afterwards,
	// so this is the one in-flight figure ModelFabric always knows — an engine count
	// cannot see a request still queued in Envoy, and under llm-d cannot see
	// one it dispatched either.
	Accepted int64 `json:"accepted,omitempty"`
	// Scheduler is llm-d on this peer, when it is scheduling for the mesh.
	// The node that schedules is not always the node you are asking: with an
	// entrypoint it has no GPUs at all.
	Scheduler *SchedulerState `json:"scheduler,omitempty"`
	LastSeen  time.Time       `json:"last_seen"`
}

// Engine families, as reported in InstanceState.Engine. An empty value is
// llama.cpp: it is what every node ran before the field existed.
const (
	EngineLlamaCPP = "llama.cpp"
	EngineMLX      = "mlx"
)

// InstanceState describes one supervised model instance, as reported by `ps`.
type InstanceState struct {
	ID      string `json:"id"`
	Model   string `json:"model"`
	Source  string `json:"source"`
	Runtime string `json:"runtime"`
	// Engine is the runtime's family (EngineLlamaCPP, EngineMLX), which decides how
	// anything outside ModelFabric can read this endpoint — what metrics it serves,
	// and whether it has slots at all. Empty from a peer too old to say, which
	// is llama.cpp by history.
	Engine string `json:"engine,omitempty"`
	// ServedModel is the id this engine answers to when it is not Model, so
	// anything dialling it directly knows it must say that instead.
	ServedModel string `json:"served_model,omitempty"`
	// MetricsPort is where ModelFabric republishes this engine's metrics with a KV
	// gauge (internal/engineshim), for schedulers that scrape what they route
	// to; zero when the engine is scraped directly.
	MetricsPort int `json:"metrics_port,omitempty"`
	EngineStats
	GPU string `json:"gpu,omitempty"`
	// Address is where this instance is reachable from outside the node. It is
	// loopback unless engine_bind says otherwise, which matters to anything
	// scheduling across the mesh rather than through ModelFabric.
	Address string `json:"address"`
	Port    int    `json:"port"`
	// Slots and PrefillTokS describe the engine to schedulers outside ModelFabric
	// (they become llm-d endpoint labels); see EngineState.
	Slots int `json:"slots,omitempty"`
	// Vision says this instance takes images. It is published to llm-d as
	// an endpoint label, so a request carrying an image is scheduled only
	// onto an engine that can read one. False from a peer too old to say.
	Vision bool `json:"vision,omitempty"`
	// VisionOff says this instance declined a projector the model has. Only
	// then can it be said to refuse images: a model with no projector never
	// had one to offer, and an image sent to it is the caller's mistake, not a
	// placement ModelFabric should route around.
	VisionOff bool      `json:"vision_off,omitempty"`
	State     string    `json:"state"`
	PID       int       `json:"pid,omitempty"`
	Error     string    `json:"error,omitempty"`
	Started   time.Time `json:"started"`
}

type Mesh struct {
	cfg    config.Config
	node   string
	client *http.Client

	poll, probe, dead time.Duration

	// Engines change at runtime as the supervisor loads and unloads models, so
	// they get their own lock rather than riding the peer lock.
	engMu   sync.RWMutex
	engines []*Engine

	mu    sync.RWMutex
	peers map[string]*Peer // keyed by tailnet address

	// instances reports supervised instances for /z/state. Nil on a node that
	// only fronts externally-started engines.
	instances func() []InstanceState
	// accepted reports what the front door is holding; nil on a node whose
	// server has not attached one.
	accepted func() int64
	// scheduler reports this node's llm-d, nil when it runs none.
	scheduler func() *SchedulerState

	// selfAddr is this node's own tailnet IPv4, advertised so peers can reach
	// instances directly rather than only through this node's proxy.
	selfAddr string

	// preferred is the node to resolve a model to when several hold it —
	// LM Link's preferred device, which "LM Link uses ... to load and use the
	// model" when the same model is available on multiple devices.
	prefMu    sync.RWMutex
	preferred string
}

// SetSelfAddr records this node's tailnet address.
func (m *Mesh) SetSelfAddr(addr string) { m.selfAddr = addr }

// SetPreferred names the node to resolve models to when several hold the same
// one. Empty means no preference.
func (m *Mesh) SetPreferred(node string) {
	m.prefMu.Lock()
	defer m.prefMu.Unlock()
	m.preferred = node
}

// Preferred reports the current preferred node.
func (m *Mesh) Preferred() string {
	m.prefMu.RLock()
	defer m.prefMu.RUnlock()
	return m.preferred
}

func New(cfg config.Config, selfNode string) *Mesh {
	poll, probe, dead := cfg.Durations()
	m := &Mesh{
		cfg:   cfg,
		node:  selfNode,
		poll:  poll,
		probe: probe,
		dead:  dead,
		peers: map[string]*Peer{},
		// No overall client timeout: generations stream for minutes. Bound the
		// time to first response header instead.
		client: &http.Client{
			Transport: &http.Transport{
				ResponseHeaderTimeout: 120 * time.Second,
				MaxIdleConnsPerHost:   64,
				IdleConnTimeout:       90 * time.Second,
			},
		},
	}
	for _, e := range cfg.Engines {
		m.engines = append(m.engines, &Engine{Name: e.Name, BaseURL: e.BaseURL})
	}
	return m
}

// rateWeighted reports whether candidates are compared by estimated time
// rather than queue depth. On unless explicitly disabled.
func (m *Mesh) rateWeighted() bool {
	return m.cfg.RateWeightedRouting == nil || *m.cfg.RateWeightedRouting
}

func (m *Mesh) Node() string         { return m.node }
func (m *Mesh) Client() *http.Client { return m.client }

// Engines returns a snapshot of the currently registered engines.
func (m *Mesh) Engines() []*Engine {
	m.engMu.RLock()
	defer m.engMu.RUnlock()
	return append([]*Engine(nil), m.engines...)
}

// RegisterEngine adds a routable engine, replacing any existing one with the
// same name. The supervisor calls this once an instance reports ready.
func (m *Mesh) RegisterEngine(e *Engine) {
	m.engMu.Lock()
	defer m.engMu.Unlock()
	for i, existing := range m.engines {
		if existing.Name == e.Name {
			m.engines[i] = e
			return
		}
	}
	m.engines = append(m.engines, e)
}

// UnregisterEngine removes an engine from routing. In-flight requests already
// dispatched to it are unaffected; they finish against the old handle.
func (m *Mesh) UnregisterEngine(name string) {
	m.engMu.Lock()
	defer m.engMu.Unlock()
	for i, e := range m.engines {
		if e.Name == name {
			m.engines = append(m.engines[:i], m.engines[i+1:]...)
			return
		}
	}
}

// SetInstanceProvider wires in the supervisor's view of running instances.
func (m *Mesh) SetInstanceProvider(f func() []InstanceState) { m.instances = f }

// SetAcceptedProvider attaches the front door's in-flight count.
func (m *Mesh) SetAcceptedProvider(f func() int64) { m.accepted = f }

// SetSchedulerProvider attaches this node's llm-d state, advertised to peers.
func (m *Mesh) SetSchedulerProvider(f func() *SchedulerState) { m.scheduler = f }

// NewEngine builds an engine handle for a supervised instance.
func NewEngine(name, baseURL string) *Engine {
	return &Engine{Name: name, BaseURL: baseURL}
}

// SetVia routes requests for this engine through addr (its shim); "" sends
// them to the engine directly again.
func (e *Engine) SetVia(addr string) {
	if addr == "" {
		e.via.Store(nil)
		return
	}
	e.via.Store(&addr)
}

// DispatchURL is where a request for this engine is sent.
func (e *Engine) DispatchURL() string {
	if v := e.via.Load(); v != nil {
		return *v
	}
	return e.BaseURL
}

// MarkReady pre-seeds an engine as healthy with a known model, so it is
// routable immediately rather than after the next poll.
func (e *Engine) MarkReady(models ...string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.healthy, e.models, e.lastErr = true, models, ""
}

// Run refreshes engine and peer state until ctx is cancelled.
func (m *Mesh) Run(ctx context.Context) {
	m.refresh(ctx)
	t := time.NewTicker(m.poll)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			m.refresh(ctx)
		}
	}
}

func (m *Mesh) refresh(ctx context.Context) {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); m.refreshEngines(ctx) }()
	go func() { defer wg.Done(); m.refreshPeers(ctx) }()
	wg.Wait()
}

// refreshEngines asks each local engine what models it currently serves.
func (m *Mesh) refreshEngines(ctx context.Context) {
	var wg sync.WaitGroup
	// Over a snapshot: loading or unloading a model rewrites this slice while
	// the poll, a request and the peer view all walk it.
	for _, e := range m.Engines() {
		wg.Add(1)
		go func(e *Engine) {
			defer wg.Done()
			models, err := m.fetchModels(ctx, e.BaseURL)
			e.mu.Lock()
			defer e.mu.Unlock()
			if err != nil {
				e.healthy, e.lastErr = false, err.Error()
				e.models = nil
				return
			}
			e.healthy, e.lastErr = true, ""
			// An engine with a fixed model list has nothing to tell us here
			// beyond being alive: mlx-lm's /v1/models lists the Hugging Face
			// cache, not what it is serving, so what it was launched for stands.
			if !e.FixedModels {
				e.models = models
			}
			if e.KVFromSlots {
				// The engine holds KV usage but exports no gauge, so ModelFabric
				// reads /slots itself — for its own views, and so peers see it
				// without anyone scraping through the shim. The same read says
				// how many requests it is serving, which ModelFabric's own counter
				// cannot know under llm-d: Envoy dials the engine directly.
				if slots, ok := m.fetchSlots(ctx, e.BaseURL); ok {
					if usage, ok := engineshim.KVUsage(slots); ok {
						e.SetKVUsage(usage)
					}
					if busy, ok := engineshim.BusySlots(slots); ok {
						e.SetObservedInflight(busy)
					}
				}
			}
			if e.NoMetrics {
				return
			}
			// Once: the context cannot change without a reload, and a reload
			// makes a new Engine. Asking every poll would be one more request
			// per engine per second for an answer that never moves.
			if !e.propsAsked.Swap(true) {
				if pool, ok := m.fetchContext(ctx, e.BaseURL); ok {
					// A disagreement is published rather than logged: it
					// belongs on the engine, where doctor and the dashboard
					// read it, not in a file nobody tails. The quiet version of
					// this cost a benchmark — conversations overflowed on an
					// engine running less context than ModelFabric believed.
					e.SetEnginePool(pool)
				}
			}
			if rates, ok := m.fetchRates(ctx, e.BaseURL); ok {
				e.SetRates(rates)
			}
		}(e)
	}
	wg.Wait()
}

type modelsResponse struct {
	Data []struct {
		ID string `json:"id"`
	} `json:"data"`
}

func (m *Mesh) fetchModels(ctx context.Context, base string) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, m.probe)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/v1/models", nil)
	if err != nil {
		return nil, err
	}
	resp, err := m.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET /v1/models: %s", resp.Status)
	}
	var mr modelsResponse
	if err := json.NewDecoder(resp.Body).Decode(&mr); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(mr.Data))
	for _, d := range mr.Data {
		if d.ID != "" {
			out = append(out, d.ID)
		}
	}
	sort.Strings(out)
	return out, nil
}

// tailscaleStatus is the subset of `tailscale status --json` we need.
type tailscaleStatus struct {
	Self *tsNode            `json:"Self"`
	Peer map[string]*tsNode `json:"Peer"`
}

type tsNode struct {
	HostName     string   `json:"HostName"`
	DNSName      string   `json:"DNSName"`
	TailscaleIPs []string `json:"TailscaleIPs"`
	Online       bool     `json:"Online"`
	// Active is tailscaled's view of the data plane: a live connection to this
	// peer, independent of whether the control plane still calls it online.
	// minion reported Online=false and Active=true while serving traffic
	// normally, which is the case Online alone gets wrong.
	Active bool `json:"Active"`
	// LastSeen separates a peer whose control plane just dropped from one that
	// has been gone for days. Zero on a peer that is online now.
	LastSeen time.Time `json:"LastSeen"`
	Tags     []string  `json:"Tags"`
}

// reachable reports whether a peer is worth a probe.
//
// Online is tailscaled's control-plane opinion and it can be wrong in the
// direction that matters: minion reported *itself* offline while its data
// plane was fine — direct LAN path, ping 1ms, /z/state answering, engines
// serving — and ModelFabric dropped it, so a benchmark would have run
// across two nodes while reporting three. The probe is the ground
// truth; these flags only decide where it is worth spending one. A device
// that is neither online nor active nor seen recently is left alone, so a
// tailnet full of dormant laptops costs nothing.
func (n *tsNode) reachable(recently time.Duration) bool {
	return n.Online || n.Active || (!n.LastSeen.IsZero() && time.Since(n.LastSeen) < recently)
}

// peerGracePeriod is how long after tailscaled last saw a peer ModelFabric keeps
// probing it anyway.
const peerGracePeriod = 15 * time.Minute

func (n *tsNode) v4() string {
	for _, ip := range n.TailscaleIPs {
		// Tailscale lists IPv4 first; select it explicitly rather than by
		// position so an IPv6-only peer is skipped instead of mis-dialed.
		for i := 0; i < len(ip); i++ {
			if ip[i] == ':' {
				break
			}
			if ip[i] == '.' {
				return ip
			}
		}
	}
	return ""
}

func (n *tsNode) tagged(tag string) bool {
	if tag == "" {
		return true
	}
	for _, t := range n.Tags {
		if t == tag {
			return true
		}
	}
	return false
}

// SelfIdentity reports this machine's Tailscale hostname and IPv4 address.
func SelfIdentity(ctx context.Context) (name, addr string, err error) {
	st, err := queryTailscale(ctx)
	if err != nil {
		return "", "", err
	}
	if st.Self == nil || st.Self.HostName == "" {
		return "", "", fmt.Errorf("tailscale status: no self hostname")
	}
	return st.Self.HostName, st.Self.v4(), nil
}

func queryTailscale(ctx context.Context) (*tailscaleStatus, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := tscli.Run(ctx, "status", "--json")
	if err != nil {
		return nil, fmt.Errorf("tailscale status: %w", err)
	}
	var st tailscaleStatus
	if err := json.Unmarshal(out, &st); err != nil {
		return nil, fmt.Errorf("parse tailscale status: %w", err)
	}
	return &st, nil
}

// knownAddrs is every tailnet address already in the peer table.
func (m *Mesh) knownAddrs() map[string]bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make(map[string]bool, len(m.peers))
	for addr := range m.peers {
		out[addr] = true
	}
	return out
}

// refreshPeers probes every eligible tailnet peer and folds the responses into
// the peer table. Peers that stop answering for DeadAfter are dropped.
func (m *Mesh) refreshPeers(ctx context.Context) {
	st, err := queryTailscale(ctx)
	if err != nil {
		// Losing tailscaled should not wipe the mesh; let DeadAfter age peers out.
		m.expire()
		return
	}

	// A peer ModelFabric already knows is probed whatever tailscaled says; see
	// tsNode.reachable for the rest.
	known := m.knownAddrs()

	type cand struct{ node, addr string }
	var cands []cand
	for _, p := range st.Peer {
		if !p.tagged(m.cfg.Tag) {
			continue
		}
		if !p.reachable(peerGracePeriod) && !known[p.v4()] {
			continue
		}
		addr := p.v4()
		if addr == "" {
			continue
		}
		name := p.HostName
		if name == "" {
			name = addr
		}
		cands = append(cands, cand{node: name, addr: addr})
	}

	// Bounded fan-out: one goroutine and one outbound request per tailnet
	// device, unbounded, is fine for four nodes and a stampede on a tailnet
	// with hundreds — every one of them held open for the probe timeout.
	var wg sync.WaitGroup
	slots := make(chan struct{}, maxPeerProbes)
	for _, c := range cands {
		wg.Add(1)
		go func(c cand) {
			defer wg.Done()
			slots <- struct{}{}
			defer func() { <-slots }()
			base := fmt.Sprintf("http://%s:%d", c.addr, m.cfg.MeshPort)
			state, err := m.probePeer(ctx, base)
			if err != nil {
				return // not a mesh node, or momentarily unreachable
			}
			if state.Node == m.node {
				return // ourselves via another address
			}
			m.upsert(c.addr, c.node, base, state)
		}(c)
	}
	wg.Wait()
	m.expire()
}

// maxPeerProbes caps how many peers are probed at once.
const maxPeerProbes = 16

func (m *Mesh) probePeer(ctx context.Context, base string) (*NodeState, error) {
	ctx, cancel := context.WithTimeout(ctx, m.probe)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/z/state", nil)
	if err != nil {
		return nil, err
	}
	resp, err := m.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET /z/state: %s", resp.Status)
	}
	var s NodeState
	if err := json.NewDecoder(resp.Body).Decode(&s); err != nil {
		return nil, err
	}
	return &s, nil
}

func (m *Mesh) upsert(addr, node, base string, s *NodeState) {
	m.mu.Lock()
	p, ok := m.peers[addr]
	if !ok {
		p = &Peer{Node: node, Addr: addr, BaseURL: base}
		m.peers[addr] = p
	}
	m.mu.Unlock()

	p.mu.Lock()
	if s.Node != "" {
		p.Node = s.Node
	}
	p.platform, p.osVersion = s.Platform, s.OSVersion
	p.models = s.Models
	p.instances = s.Instances
	p.engines = s.Engines
	p.reported = s.Inflight
	p.accepted = s.Accepted
	p.scheduler = s.Scheduler
	p.lastSeen = time.Now()
	p.alive = true
	p.mu.Unlock()

	// The report we just stored already accounts for everything sent so far.
	p.pending.Store(0)
}

func (m *Mesh) expire() {
	cutoff := time.Now().Add(-m.dead)
	m.mu.Lock()
	defer m.mu.Unlock()
	for addr, p := range m.peers {
		p.mu.Lock()
		if p.lastSeen.Before(cutoff) {
			p.alive = false
		}
		stale := p.lastSeen.Before(cutoff.Add(-4 * m.dead))
		p.mu.Unlock()
		if stale {
			delete(m.peers, addr)
		}
	}
}

// State renders this node's own state for peers.
func (m *Mesh) State() NodeState {
	seen := map[string]bool{}
	var models []string
	var engines []EngineState
	var inflight int64
	for _, e := range m.Engines() {
		es := e.snapshot()
		engines = append(engines, es)
		inflight += es.Inflight
		if !es.Healthy {
			continue
		}
		for _, mod := range es.Models {
			if !seen[mod] {
				seen[mod] = true
				models = append(models, mod)
			}
		}
	}
	sort.Strings(models)
	var instances []InstanceState
	if m.instances != nil {
		instances = m.instances()
	}
	var accepted int64
	if m.accepted != nil {
		accepted = m.accepted()
	}
	var sched *SchedulerState
	if m.scheduler != nil {
		sched = m.scheduler()
	}
	return NodeState{
		Node:      m.node,
		Addr:      m.selfAddr,
		Platform:  Platform(),
		OSVersion: osproc.OSVersion(),
		Models:    models,
		Inflight:  inflight,
		Accepted:  accepted,
		Scheduler: sched,
		Engines:   engines,
		Instances: instances,
		Updated:   time.Now().UTC(),
	}
}

// Peers renders the peer table for /z/mesh.
func (m *Mesh) Peers() []PeerView {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]PeerView, 0, len(m.peers))
	for _, p := range m.peers {
		p.mu.RLock()
		out = append(out, PeerView{
			Node:      p.Node,
			Addr:      p.Addr,
			BaseURL:   p.BaseURL,
			Platform:  p.platform,
			OSVersion: p.osVersion,
			Alive:     p.alive,
			Instances: append([]InstanceState(nil), p.instances...),
			Models:    append([]string(nil), p.models...),
			Inflight:  p.reported + p.pending.Load(),
			// Both were declared and never filled: a peer's front-door count
			// read 0 however busy it was, and nothing in the mesh could name
			// the node doing the scheduling.
			Accepted:  p.accepted,
			Scheduler: p.scheduler,
			LastSeen:  p.lastSeen,
		})
		p.mu.RUnlock()
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Node < out[j].Node })
	return out
}

// Models is the union of every model served anywhere in the mesh.
func (m *Mesh) Models() []string {
	seen := map[string]bool{}
	for _, mod := range m.State().Models {
		seen[mod] = true
	}
	m.mu.RLock()
	for _, p := range m.peers {
		p.mu.RLock()
		if p.alive {
			for _, mod := range p.models {
				seen[mod] = true
			}
		}
		p.mu.RUnlock()
	}
	m.mu.RUnlock()

	out := make([]string, 0, len(seen))
	for mod := range seen {
		out = append(out, mod)
	}
	sort.Strings(out)
	return out
}

// Candidate is a routable destination for one request.
type Candidate struct {
	Local   bool
	Node    string
	Name    string
	BaseURL string
	Score   float64
	// Inflight and Slots say whether the candidate has room: a request sent
	// to one with Inflight >= Slots queues. Slots is zero when unknown. For a
	// peer they cover that node's engines serving the model.
	Inflight int64
	Slots    int64
	// PrefillTokS is the measured prompt rate (for a peer, its fastest engine
	// for the model); zero when not yet measured. It is what weights this
	// candidate's score: a slow engine's queue costs more per entry (cost.go).
	PrefillTokS float64
	// RateUnmeasurable says this candidate can never report a throughput rate:
	// its engine serves no metrics at all (mlx-lm). That is different from a
	// rate not measured yet, and the two are scored differently — see cost.go.
	RateUnmeasurable bool
	// outstanding is the candidate's own load measure — a local engine's
	// in-flight count, or a peer's reported load plus what has been dispatched
	// since its last poll. Scoring happens after the whole set is collected, so
	// it is carried rather than folded into Score on the spot.
	outstanding int64
	// ServedModel is the id a local engine answers to, when it is not the
	// catalog key (see Engine.Served). Empty for a peer: forwarding sends the
	// catalog key, and that node's own router translates it.
	ServedModel string
	// NoConstrainedDecoding says a request asking for response_format or a
	// grammar would come back unconstrained here. For a local engine that is
	// its trait; for a peer it means every engine it has for this model
	// ignores them, so forwarding could only produce free prose. A peer with a
	// mix is left capable: its own router applies this same rule.
	NoConstrainedDecoding bool
	// NoVision says a request carrying an image would fail here: the engine
	// was loaded without its projector. For a peer it means every engine it
	// has for this model was; a peer with a mix is left capable, because its
	// own router applies this same rule.
	NoVision bool
	// Embedding says a local engine embeds and does not generate. Always
	// false for a peer, whose own router decides.
	Embedding bool

	engine *Engine
	peer   *Peer
}

// Full reports whether a request sent here would queue behind the engine's
// slots. Unknown capacity is never full.
func (c Candidate) Full() bool { return c.Slots > 0 && c.Inflight >= c.Slots }

// Acquire marks the candidate busy. For a local engine that lasts for the
// duration of the request; for a peer it lasts until the peer's next poll,
// which reports its own load. The returned
// function must be called when the request completes.
func (c Candidate) Acquire() func() {
	switch {
	case c.engine != nil:
		c.engine.inflight.Add(1)
		c.engine.lastUsed.Store(time.Now().UnixNano())
		return func() {
			// Stamp before decrementing, so an idle check that sees zero in
			// flight also sees this request's finish time.
			c.engine.lastUsed.Store(time.Now().UnixNano())
			c.engine.inflight.Add(-1)
		}
	case c.peer != nil:
		// A peer's real in-flight count comes from its own next poll, which
		// resets this counter (see refreshPeers). pending only bridges the gap
		// between dispatching a request and hearing back, so there is nothing
		// to release here — releasing would double-count against the report.
		c.peer.pending.Add(1)
		return func() {}
	}
	return func() {}
}

// Candidates returns every destination able to serve model, cheapest first.
// When localOnly is set, peers are excluded — used to stop a forwarded request
// from being forwarded again.
func (m *Mesh) Candidates(model string, localOnly bool) []Candidate {
	var out []Candidate
	for _, e := range m.Engines() {
		if !e.has(model) {
			continue
		}
		// The engine observes llm-d traffic too. Keep the router's count as a
		// floor for dispatches since the last poll, without adding counts
		// that include the same requests.
		n := max(e.inflight.Load(), e.Inflight())
		out = append(out, Candidate{
			Local:            true,
			Node:             m.node,
			Name:             e.Name,
			BaseURL:          e.DispatchURL(),
			Inflight:         n,
			Slots:            e.slots.Load(),
			PrefillTokS:      e.TrustedPrefillRate(),
			RateUnmeasurable: e.NoMetrics,
			ServedModel:      e.Served,

			NoConstrainedDecoding: e.NoConstrainedDecoding,
			NoVision:              e.NoVision,
			Embedding:             e.Embedding,
			engine:                e,
		})
	}
	if !localOnly {
		m.mu.RLock()
		for _, p := range m.peers {
			if !p.has(model) {
				continue
			}
			inflight, slots, rate := p.capacity(model)
			unmeasurable := p.rateUnmeasurable(model)
			// Read under the peer's own lock: m.mu guards the map, not the
			// fields, and refreshPeers writes Node while requests are routing.
			node, baseURL := p.identity()
			out = append(out, Candidate{
				Node:             node,
				Name:             node,
				BaseURL:          baseURL,
				Inflight:         inflight,
				RateUnmeasurable: unmeasurable,
				outstanding:      p.load(),
				Slots:            slots,
				PrefillTokS:      rate,

				NoConstrainedDecoding: !p.constrains(model),
				NoVision:              p.refusesImages(model),
				peer:                  p,
			})
		}
		m.mu.RUnlock()
	}
	// Scored here rather than as each candidate is built: the weighting is
	// relative to the fastest engine *among these*, which is not known until
	// they have all been collected (see cost.go).
	ref := referenceRate(out)
	slowest := slowestRate(out)
	for i := range out {
		outstanding := out[i].outstanding
		if out[i].Local {
			outstanding = out[i].Inflight
		}
		if m.rateWeighted() {
			out[i].Score = cost(outstanding, assumedRate(out[i], slowest), ref, m.cfg.LocalBias, out[i].Local)
			continue
		}
		// The previous comparison, kept switchable: queue depth alone, with
		// rate only as a tie-break below.
		out[i].Score = float64(outstanding)
		if out[i].Local {
			out[i].Score -= m.cfg.LocalBias
		}
	}

	preferred := m.Preferred()
	sort.Slice(out, func(i, j int) bool {
		// A preferred node wins outright. This is a strict precedence rather
		// than a score bonus, so the choice cannot be eroded by load — but it
		// degrades safely: a preferred node that is down, or that does not hold
		// this model, never became a candidate, so the rest are used normally.
		pi := preferred != "" && out[i].Node == preferred
		pj := preferred != "" && out[j].Node == preferred
		if pi != pj {
			return pi
		}
		// A candidate with a free slot beats one where the request would
		// queue, whatever the scores: a queued request on a llama.cpp engine
		// waits for a slot and then evicts another conversation's cache.
		if fi, fj := out[i].Full(), out[j].Full(); fi != fj {
			return fj
		}
		if out[i].Score != out[j].Score {
			return out[i].Score < out[j].Score
		}
		// Equally loaded: the faster prefill serves a cold prompt sooner.
		if out[i].PrefillTokS != out[j].PrefillTokS {
			return out[i].PrefillTokS > out[j].PrefillTokS
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// rateUnmeasurable reports whether every engine this peer has for the model is
// one that can never report a throughput rate.
//
// Read from each instance's engine family, because traits follow from it:
// mlx-lm serves no /metrics whatever it is asked. Instances rather than the
// engine list, since the family is what the peer publishes per instance.
//
// "Every" rather than "any" — a peer with one measurable engine for the model
// can be scored on that one, and forwarding reaches its router, which picks
// among them.
func (p *Peer) rateUnmeasurable(model string) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	found := false
	for _, i := range p.instances {
		if i.Model != model {
			continue
		}
		found = true
		// An instance from a node too old to name its engine is llama.cpp by
		// history, which does report metrics.
		if i.Engine == "" || !engines.EngineTraits(i.Engine).NoMetrics {
			return false
		}
	}
	return found
}
