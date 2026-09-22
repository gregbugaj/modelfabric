package server

import (
	"bytes"
	"crypto/subtle"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/gregbugaj/modelfabric/internal/discovery"
	"github.com/gregbugaj/modelfabric/internal/mesh"
	"github.com/gregbugaj/modelfabric/internal/router"
	"github.com/gregbugaj/modelfabric/internal/tokentap"
)

// ModelFabric is the front door. Apps call this node's :1234 and ModelFabric routes the
// request itself, across every engine in the mesh:
//
//	app ──► ModelFabric :1234 ──► ModelFabric's router ──► engines, local or on a peer
//	                   └──► llm-d's Envoy ──► engines   (its one model only)
//
// Budgets, spend tracking and third-party providers are not ModelFabric's job:
// a proxy that does them goes *in front* of this door, where ModelFabric
// neither knows nor needs to know about it.
//
// Auth is OpenAI's: "Authorization: Bearer <key>" (or Anthropic's x-api-key),
// checked by ModelFabric itself. The key is the node's own (`mfsh key`). Two listeners
// face apps:
//   - the loopback front door (FrontHandler): the key is checked only when
//     require_api_key is on — off by default, as LM Studio's is.
//   - the public listener (PublicHandler), for exposure through Tailscale
//     Funnel or a TLS proxy: inference only, the key always checked.
//
// A caller's credentials stop here. Nothing downstream is handed them: Envoy is
// on loopback and needs none, and an engine gets ModelFabric's own key where one is
// required.
//
// Peers on the tailnet listener always reach the router directly: they have
// already been routed once, and the hop marker they carry is what stops a
// request going round the mesh twice.

// inferencePaths are the routes that generate, and so the ones a scheduler may
// be given instead of ModelFabric's own router. Anything else — LM Studio's /api/v0,
// ModelFabric's own /z and /api — stays with ModelFabric.
var inferencePaths = map[string]bool{
	"/v1/chat/completions":     true,
	"/v1/completions":          true,
	"/v1/embeddings":           true,
	"/v1/responses":            true,
	"/v1/messages":             true,
	"/v1/audio/transcriptions": true,
	"/v1/rerank":               true,
	"/v1/models":               true,
}

// inferencePath reports a request that runs a model: what auth protects.
func inferencePath(r *http.Request) bool {
	if r.Method != http.MethodPost {
		return false
	}
	// /api/v1/chat is the one route under /api/v1 that runs a model, so it is
	// an app's to call and a key's to protect, unlike the management beside it.
	return strings.HasPrefix(r.URL.Path, "/v1/") || strings.HasPrefix(r.URL.Path, "/api/v0/") || r.URL.Path == "/api/v1/chat"
}

// apiKey is the key a request presents, OpenAI-style or Anthropic-style.
func apiKey(r *http.Request) string {
	if k, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok {
		return strings.TrimSpace(k)
	}
	return strings.TrimSpace(r.Header.Get("X-Api-Key"))
}

// authorized checks the presented key against this node's, in constant time.
// A node whose key cannot be read authorizes nothing.
func (s *Server) authorized(r *http.Request) bool {
	if s.apiKey == nil {
		return false
	}
	want, err := s.apiKey()
	given := apiKey(r)
	return err == nil && want != "" && given != "" && subtle.ConstantTimeCompare([]byte(given), []byte(want)) == 1
}

// credentialed reports a request an app may make: the node key or any named
// token. Not a stand-in for authorized, which also means "this request is
// from this node" and lets a caller keep the forwarding marker — a token
// handed to someone else's script must not be able to claim that.
func (s *Server) credentialed(r *http.Request) bool {
	if s.authorized(r) {
		return true
	}
	if s.keys == nil {
		return false
	}
	_, ok := s.keys.Match(apiKey(r))
	return ok
}

func unauthorized(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="modelfabric"`)
	writeError(w, http.StatusUnauthorized, "a valid API key is required: send Authorization: Bearer <key> (`mfsh key` on the node)")
}

func isInferencePath(r *http.Request) bool {
	if r.Method == http.MethodGet {
		return r.URL.Path == "/v1/models"
	}
	return r.Method == http.MethodPost && inferencePaths[r.URL.Path]
}

// scheduled reports whether something other than ModelFabric's own router should be
// given this request, and where.
//
// Today that is llm-d, and only for the one model it schedules: every other
// model is the router's business. It reads the model from the body, which
// toUpstream reads again — the body is buffered and put back, so the second
// read is free, and doing it here keeps the decision in one place.
func (s *Server) scheduled(r *http.Request) (*url.URL, bool) {
	model := peekRequest(r).Model
	// scheduler is a seam for tests, which need a stand-in upstream without
	// running a real Envoy. Production leaves it nil.
	if s.scheduler != nil {
		return s.scheduler(model)
	}
	if s.llmd == nil {
		return nil, false
	}
	return s.llmdTarget(model)
}

// resolveEngine maps an upstream base URL, as a scheduler reports it, back to the
// node and instance it belongs to.
func (s *Server) resolveEngine(apiBase string) (node, engine string, ok bool) {
	if s.m == nil {
		return "", "", false
	}
	// ModelFabric's own plumbing is not an engine. llm-d's Envoy listens on this
	// machine but the EPP behind it may have scheduled the request onto any
	// node in the mesh, so matching on the address would name this node as the
	// server when it holds no engine at all. Better to record no node than the
	// wrong one — the row still says llm-d chose it.
	if s.isOwnProxy(apiBase) {
		return "", "", false
	}
	return matchEngine(apiBase, s.m.State(), s.m.Peers())
}

// isOwnProxy reports whether an upstream address is one of this node's own
// proxies rather than an inference engine.
func (s *Server) isOwnProxy(apiBase string) bool {
	host := hostPort(apiBase)
	if host == "" {
		return false
	}
	if s.llmd != nil && hostPort(s.llmd.URL()) == host {
		return true
	}
	// And this node's own front door. An upstream that reports it is ModelFabric
	// talking to itself, not an engine: naming this node as the server would
	// credit it with work its GPUs may not have done.
	return s.frontListen != "" && hostPort("http://"+s.frontListen) == host
}

// hostPort is the host:port of a URL, or "" if it has none.
func hostPort(raw string) string {
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Host
}

// upstreamHeader is where llm-d's Envoy reports the address it dialled.
const upstreamHeader = "X-Fabric-Upstream"

// upstreamURL turns Envoy's "host:port" into the URL the lookup takes. Empty
// stays empty.
func upstreamURL(addr string) string {
	if addr == "" || strings.Contains(addr, "://") {
		return addr
	}
	return "http://" + addr
}

// matchEngine is the lookup itself, kept free of the mesh so it can be tested
// directly. Matching is on host:port, since two engines on one host differ
// only by port; any path on the address is ignored.
func matchEngine(apiBase string, self mesh.NodeState, peers []mesh.PeerView) (string, string, bool) {
	if apiBase == "" {
		return "", "", false
	}
	u, err := url.Parse(apiBase)
	if err != nil || u.Host == "" {
		return "", "", false
	}
	host, _, err := net.SplitHostPort(u.Host)
	if err != nil {
		host = u.Host
	}
	find := func(n, addr string, insts []mesh.InstanceState) (string, string, bool) {
		for _, i := range insts {
			// Address is a bare host and Port is separate, so join them before
			// comparing — comparing host against host:port matches nothing.
			h := i.Address
			if h == "" {
				h = addr
			}
			if h == "" {
				continue
			}
			// Both ports name the same engine. llm-d dials the shim, so the
			// engine's own node is in the request path and can watch what it
			// is generating, and scrapes the same port. Matching only the
			// engine's own port left every row saying "engine unknown".
			for _, port := range [2]int{i.Port, i.MetricsPort} {
				if port == 0 {
					continue
				}
				if net.JoinHostPort(h, strconv.Itoa(port)) == u.Host {
					return n, i.ID, true
				}
			}
		}
		return "", "", false
	}
	if n, e, found := find(self.Node, self.Addr, self.Instances); found {
		return n, e, true
	}
	for _, p := range peers {
		if !p.Alive {
			continue
		}
		if n, e, found := find(p.Node, p.Addr, p.Instances); found {
			return n, e, true
		}
	}
	// No engine on that port. The machine is still knowable from the address,
	// and "minion, engine unknown" is a better answer than "not recorded" —
	// it says what we know and leaves the gap visible. This is what a request
	// through a per-node proxy or a shim looks like, and it is also the shape
	// an orphaned engine takes: the port answers, no node claims it.
	if host == "" {
		return "", "", false
	}
	if host == self.Addr || host == "127.0.0.1" || host == "localhost" || host == "::1" {
		return self.Node, "", true
	}
	for _, p := range peers {
		if p.Alive && p.Addr != "" && p.Addr == host {
			return p.Node, "", true
		}
	}
	return "", "", false
}

// statusClientClosed is nginx's 499: the caller went away before the upstream
// answered. Nothing reaches that caller — the point is the traffic log, where
// a row saying the client gave up after 300s is a different bug report from
// one saying llm-d failed.
const statusClientClosed = 499

// toUpstream proxies r to a scheduler that will choose the engine — llm-d's
// Envoy — streaming the response as it arrives.
//
// ModelFabric stays the proxy even though it is not choosing, so it still sees every
// request: `mfsh log` and the dashboard show the traffic instead of an empty
// table, and the token tap can watch replies. Node and Engine stay empty,
// because the choice was not this node's to make.
func (s *Server) toUpstream(w http.ResponseWriter, r *http.Request, base *url.URL) {
	start := time.Now()
	// This path records its own event, so it mints its own id. Envoy dials
	// engines directly rather than another node's front door, so this is the
	// only record of the request.
	tc := router.TraceOf(r)
	trace := tc.TraceID
	r.Header.Set(router.TraceHeader, trace)
	w.Header().Set(router.TraceHeader, trace)

	peek := peekRequest(r)
	model, reqBody := peek.Model, peek.Body
	var servedNode, servedEngine, upstream string
	via := "llm-d"

	// A scheduler in front cannot read the request the way ModelFabric can, so it is
	// told what this one needs: an engine loaded with its projector, or any
	// engine. Without this llm-d can place an image on an engine that has no
	// projector, which fails inside llama.cpp with "failed to process mtmd
	// chunk" — it is given the modelfabric.sh/vision label and, until now, nothing
	// that read it (see discovery.visionProfile).
	//
	// Set on every request, not only the ones with an image: naming the profile
	// each time means the scheduler never has to guess, and a header a caller
	// sent for itself does not survive to pick its own scheduling.
	profile := "default"
	if carriesImage(peek, router.CarriesImage) {
		profile = discovery.VisionProfile
	}
	r.Header.Set(discovery.ProfileHeader, profile)

	// Ask upstream for a stream even when the caller did not, so what the
	// engine writes can be watched, and hand the caller the single body it
	// asked for. Always, not only while someone is watching: a path that runs
	// only when observed is one whose bugs appear only when observed.
	//
	// The request body was already read by peekRequest, so rewriting it here
	// costs nothing extra — but only a whole body can be rewritten, since a
	// prefix is not a JSON document. A body over bodyPeekMax streams as the
	// caller sent it.
	var watch func([]byte)
	if upgraded, ok := unstreamable(r.URL.Path, reqBody); ok && peek.Whole {
		r.Body = io.NopCloser(bytes.NewReader(upgraded))
		r.ContentLength = int64(len(upgraded))
		r.Header.Del("Content-Length")
		if tw := tokentap.WriterOf(w); tw != nil {
			watch = tw.WatchUpstream()
		} else {
			// Nothing is watching, but the upgrade still happened, so the
			// response still has to be put back together.
			watch = func([]byte) {}
		}
	}
	sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
	capture, _, capMax, _ := s.traffic.settings()
	if capture {
		sw.capture = make([]byte, 0, 1024)
		sw.captureMax = capMax
	}
	defer func() {
		if s.traffic != nil {
			e := router.Event{
				Time: start, Path: r.URL.Path, Model: model,
				Node: servedNode, Engine: servedEngine,
				Local:    servedNode != "" && servedNode == s.m.State().Node,
				Status:   sw.status,
				Millis:   time.Since(start).Milliseconds(),
				BytesOut: sw.written, Via: via, Upstream: upstream, Trace: trace,
			}
			bl := router.BodyLog{Enabled: capture, Max: capMax}
			rb, cut1 := bl.Clip(reqBody)
			pb, cut2 := bl.Clip(sw.capture)
			e.ReqBody, e.RespBody, e.Truncated = rb, pb, cut1 || cut2
			s.traffic.publish(e)
		}
	}()
	w = sw

	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(base)
			pr.Out.Host = base.Host
			// The caller's credentials stop here. Envoy is on loopback and
			// needs none, and forwarding an end user's key to anything
			// downstream is never right — an Anthropic-style client sends
			// X-Api-Key, which ReverseProxy copies in before this rewrite.
			pr.Out.Header.Del("Authorization")
			pr.Out.Header.Del("X-Api-Key")
			pr.Out.Header.Del("Api-Key")
			pr.Out.Header.Del("Cookie")
			// Internal markers never reach the scheduler from outside.
			pr.Out.Header.Del(router.HopHeader)
		},
		// Flush every write: chat completions stream for as long as
		// generation takes.
		FlushInterval: -1,
		ModifyResponse: func(resp *http.Response) error {
			// The address Envoy dialled for the EPP (discovery.EnvoyConfig adds
			// it to every response). ModelFabric knows every engine's address,
			// so that maps back to a node and an instance, and a request
			// ModelFabric did not route still reports where it ran, exactly
			// like one it did.
			//
			// It used to read a header nothing sets any more, so under llm-d
			// nothing was ever resolved and every row said no node. When the
			// header is absent (an Envoy config written by an older build)
			// the upstream stays empty rather than guessed at. It is taken
			// off the response: the caller gets the node's name, not an
			// engine's address.
			upstream = upstreamURL(resp.Header.Get(upstreamHeader))
			resp.Header.Del(upstreamHeader)
			resp.Header.Set("X-Fabric-Via", via)
			if node, engine, ok := s.resolveEngine(upstream); ok {
				resp.Header.Set(router.NodeHeader, node)
				// An engine only when one was identified: an empty header
				// would read as "engine: nothing" rather than "not known".
				if engine != "" {
					resp.Header.Set(router.EngineHeader, engine)
				}
				servedNode, servedEngine = node, engine
			}
			if watch == nil {
				return nil
			}
			// Only an event stream is reassembled. An error, or anything that
			// did not come back as one, is passed through as it is: guessing
			// at it would turn the engine's own report into a wrong answer.
			if !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
				return nil
			}
			body, err := assembleStream(resp.Body, watch)
			resp.Body.Close()
			if err != nil {
				return err
			}
			resp.Body = io.NopCloser(bytes.NewReader(body))
			resp.ContentLength = int64(len(body))
			resp.Header.Set("Content-Type", "application/json")
			resp.Header.Set("Content-Length", strconv.Itoa(len(body)))
			resp.Header.Del("Transfer-Encoding")
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			// ReverseProxy reports a caller who hung up and a scheduler that
			// broke through the same handler, and calling both "did not
			// answer" blamed the scheduler for client timeouts: hours went
			// into the wrong component before the traffic log showed the
			// requests had run 300s first. A cancelled request context means
			// the caller stopped waiting. 499 is nginx's, not the RFC's, but
			// it is what reads "client closed the request" in a log.
			if r.Context().Err() != nil {
				writeError(w, statusClientClosed, "the client closed the request before llm-d answered; it had been waiting "+time.Since(start).Round(time.Second).String())
				return
			}
			writeError(w, http.StatusBadGateway, "llm-d did not answer: "+err.Error())
		},
	}
	proxy.ServeHTTP(w, r)
}

// FrontHandler is what apps reach at the loopback address: inference through
// llm-d for the model it schedules, everything else from ModelFabric itself.
func (s *Server) FrontHandler() http.Handler {
	inner := s.Handler()
	return s.browserSafe(s.frontCounter(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.requireKey.Load() && inferencePath(r) && !s.credentialed(r) {
			unauthorized(w)
			return
		}
		// The hop marker is ModelFabric's own: a request carrying one has already been
		// forwarded once and must not be forwarded again. Only a caller that
		// proves it is this node's may set it; otherwise it is stripped, so
		// nobody can dodge loop protection with a header. Peers forward on
		// their own listener, so they are unaffected.
		if !s.authorized(r) {
			r.Header.Del(router.HopHeader)
		}
		s.serveInference(w, r, inner, false)
	})))
}

// frontCounter counts the inference requests a listener is holding.
//
// Here because here is the one place that sees every request: whatever routes
// it afterwards, llm-d or this node's own router, it came through this door.
// An engine's count cannot see a request still queued in Envoy, and under
// llm-d cannot see one it is serving either,
// because Envoy dials the engine directly and ModelFabric is never told. That is why
// the dashboard read 0 in flight through an 8000-word request whose KV cache
// was visibly climbing.
func (s *Server) frontCounter(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if inferencePath(r) {
			s.accepted.Add(1)
			defer s.accepted.Add(-1)
		}
		next.ServeHTTP(w, r)
	})
}

// PublicHandler is the listener meant for exposure (public_listen): inference
// only, the API key checked on every request, and nothing else — no
// dashboard, no management, no mesh views. Requests go to llm-d for the model
// it schedules, else to ModelFabric's router.
func (s *Server) PublicHandler() http.Handler {
	inner := s.Handler()
	return logging(s.log, s.browserSafe(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !inferencePath(r) && !(r.Method == http.MethodGet && r.URL.Path == "/v1/models") {
			writeError(w, http.StatusNotFound, "not available here: this address serves inference (/v1) only")
			return
		}
		if !s.credentialed(r) {
			unauthorized(w)
			return
		}
		// Internal markers are ModelFabric's own; nothing outside may set them.
		r.Header.Del(router.HopHeader)
		r.Header.Del("X-Api-Key")
		r.Header.Del(innerHeader)
		s.serveInference(w, r, inner, true)
	})))
}

// handleFront describes this node's front door: where apps point, whether a key
// is required, and whether anything is published beyond loopback.
//
// The Overview panel reads this. Without it the panel fell back to "no key
// needed on loopback", which is a lie on a node that requires one.
func (s *Server) handleFront(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"listen":          s.frontListen,
		"require_api_key": s.requireKey.Load(),
		"public_listen":   s.publicListen,
	})
}

// serveInference keeps scheduling, stream assembly and token capture in one
// place after each listener has enforced its own authentication boundary.
// Public credentials are stripped only after the tap has identified the caller.
func (s *Server) serveInference(w http.ResponseWriter, r *http.Request, inner http.Handler, public bool) {
	// Stored conversations and MCP tools on /v1/responses (chat.go).
	// A forwarded request has been through its own node's layer.
	if s.layered(r) && r.Header.Get(router.HopHeader) == "" {
		s.handleResponses(w, r)
		return
	}
	if isInferencePath(r) && r.Header.Get(router.HopHeader) == "" {
		if base, ok := s.scheduled(r); ok {
			var done func()
			w, done = s.tapTokens(w, r)
			defer done()
			s.toUpstream(w, r, base)
			return
		}
	}
	// ModelFabric routes this one itself, so ModelFabric does the upgrade: ask the
	// engine for a stream, let the tap read it, and give the caller the one
	// body it asked for. Outermost first, so the tap sits between the
	// router and the reassembly and sees the stream rather than the result
	// (unstream_router.go).
	assemble := func() {}
	w, assemble = s.unstreamed(w, r)
	defer assemble()
	var done func()
	w, done = s.tapTokens(w, r)
	defer done()
	if public {
		r.Header.Del("Authorization")
	}
	inner.ServeHTTP(w, r)
}
