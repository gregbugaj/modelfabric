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

// The front door routes inference through the mesh router or through llm-d's
// Envoy for the model it schedules.
//
// FrontHandler checks credentials when require_api_key is enabled (default off).
// PublicHandler serves inference only and always requires credentials. Bearer
// and Anthropic x-api-key authentication are accepted. Caller credentials are
// stripped before forwarding; engines receive configured engine credentials.
//
// Peers use the router directly. Their forwarding marker prevents a second
// mesh hop.

// inferencePaths lists generation routes eligible for scheduling. Management,
// mesh and model-list routes remain local.
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

func inferencePath(r *http.Request) bool {
	if r.Method != http.MethodPost {
		return false
	}
	// /api/v1/chat is the one route under /api/v1 that runs a model, so it is
	// an app's to call and a key's to protect, unlike the management beside it.
	return strings.HasPrefix(r.URL.Path, "/v1/") || strings.HasPrefix(r.URL.Path, "/api/v0/") || r.URL.Path == "/api/v1/chat"
}

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

// credentialed accepts the node key or a named inference token. Only the node
// key also authorizes forwarding markers; named tokens cannot bypass scheduling.
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

// scheduled selects an upstream scheduler for the requested model. llm-d
// handles only its configured model. The buffered body remains available to
// toUpstream.
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

func (s *Server) resolveEngine(apiBase string) (node, engine string, ok bool) {
	if s.m == nil {
		return "", "", false
	}
	// An address belonging to our proxy does not identify the serving engine;
	// Envoy may have scheduled any node. Leave attribution unset.
	if s.isOwnProxy(apiBase) {
		return "", "", false
	}
	return matchEngine(apiBase, s.m.State(), s.m.Peers())
}

func (s *Server) isOwnProxy(apiBase string) bool {
	host := hostPort(apiBase)
	if host == "" {
		return false
	}
	if s.llmd != nil && hostPort(s.llmd.URL()) == host {
		return true
	}
	// Our own front door is a proxy address, not evidence of engine location.
	return s.frontListen != "" && hostPort("http://"+s.frontListen) == host
}

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
			// comparing - comparing host against host:port matches nothing.
			h := i.Address
			if h == "" {
				h = addr
			}
			if h == "" {
				continue
			}
			// llm-d dials and scrapes the shim, so both shim and engine ports must
			// resolve to the same instance.
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
	// An unmatched port can still identify the node. Leave the engine unknown
	// for per-node proxies or untracked engines.
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

// statusClientClosed uses nginx's 499 to distinguish client cancellation from
// upstream failure in the traffic log.
const statusClientClosed = 499

// toUpstream proxies to the scheduler and streams its response. Proxying keeps
// traffic logging and token capture active. Attribution remains empty unless
// the upstream identifies the serving engine.
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

	// Select the vision profile so llm-d excludes engines without projectors
	// for image requests. Set it for every request to override caller-supplied
	// scheduling headers.
	profile := "default"
	if carriesImage(peek, router.CarriesImage) {
		profile = discovery.VisionProfile
	}
	r.Header.Set(discovery.ProfileHeader, profile)

	// Request a stream for token capture, then reassemble non-streaming replies.
	// Only complete bodies can be rewritten; bodies over bodyPeekMax pass through.
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
			// Strip caller credentials, including Anthropic's X-Api-Key copied by
			// ReverseProxy. Loopback Envoy needs no credentials.
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
			// Envoy reports its upstream address; map it to a node and instance.
			// Older configs omit the header, leaving attribution unknown. Remove the
			// address header before returning the node name to the caller.
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
			// ReverseProxy uses this handler for both upstream failure and client
			// cancellation. A cancelled request context is recorded as 499.
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
		// Only the node key authorizes the forwarding marker, which prevents a
		// second mesh hop. Strip untrusted markers to prevent scheduler bypass.
		if !s.authorized(r) {
			r.Header.Del(router.HopHeader)
		}
		s.serveInference(w, r, inner, false)
	})))
}

// frontCounter counts inference requests held by a listener, including those
// queued or served through Envoy that engine counters cannot observe.
func (s *Server) frontCounter(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if inferencePath(r) {
			s.accepted.Add(1)
			defer s.accepted.Add(-1)
		}
		next.ServeHTTP(w, r)
	})
}

// PublicHandler serves authenticated inference and model listing on public_listen.
// It excludes management, mesh views and the dashboard.
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
		// Strip internal routing markers and the caller's X-Api-Key.
		r.Header.Del(router.HopHeader)
		r.Header.Del("X-Api-Key")
		r.Header.Del(innerHeader)
		s.serveInference(w, r, inner, true)
	})))
}

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
	// The tap must see the engine's stream before reassembly into the caller's
	// non-streaming response (unstream_router.go).
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
