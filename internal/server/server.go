// Package server exposes the local OpenAI-compatible endpoint plus the mesh
// control endpoints.
//
// Apps only ever talk to this server, on loopback. Whether a model lives on
// this machine or three hops across the tailnet is not their concern.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/gregbugaj/modelfabric/internal/chatapi"
	"github.com/gregbugaj/modelfabric/internal/llmd"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gregbugaj/modelfabric/internal/discovery"
	"github.com/gregbugaj/modelfabric/internal/doctor"
	"github.com/gregbugaj/modelfabric/internal/mesh"
	"github.com/gregbugaj/modelfabric/internal/nodekey"
	"github.com/gregbugaj/modelfabric/internal/router"
	"github.com/gregbugaj/modelfabric/internal/supervisor"
	"github.com/gregbugaj/modelfabric/internal/tokentap"
	"github.com/gregbugaj/modelfabric/internal/tsid"
)

type Server struct {
	m   *mesh.Mesh
	r   *router.Router
	sup *supervisor.Supervisor
	// tune is the slot sweep this node is running, if any (tune.go).
	tune    tuneState
	bench   benchState
	cluster clusterState
	// shared remembers the digests of files served to peers (share.go).
	shared shareHashes
	log    *slog.Logger
	ui     http.Handler

	// supErr explains why model management is unavailable, so the API can say
	// what is actually wrong instead of guessing.
	supErr string

	// prefStore persists the preferred node across restarts.
	prefStore string
	// prefMu keeps each saved preference and its live update in the same order.
	prefMu sync.Mutex

	traffic *traffic
	// tokens taps replies as they stream, for `mfsh log -tokens`. Node-local
	// by construction: the tap sits on this node's front door and the route
	// is not in peerAllowed, so the mesh listener refuses it.
	tokens *tokentap.Tap
	// feed pushes the dashboard's state to /api/v1/events as it changes.
	feed *stateFeed
	// metrics are the Prometheus counters served at /metrics. Fed from the
	// same publish the traffic ring uses, so every routed request counts once.
	metrics *metrics
	// Doctor runs this node's health checks. Supplied by the caller because
	// the paths and the runtime registry are the binary's, not the server's —
	// and a node must report on itself, not on whoever is asking.
	Doctor func() []doctor.Check

	// runtimeStore persists `mfsh runtime select` across restarts.
	runtimeStore string
	// reloadRuntimes rediscovers installed runtimes into the registry.
	reloadRuntimes func() error
	runtimesRoot   string // where ModelFabric installs its own runtimes
	// apiKey returns this node's API key (created on first use); requireKey
	// makes the loopback front door check it (see front.go).
	apiKey func() (string, error)
	// accepted is what the front door is holding: inference requests taken and
	// not yet answered. Every request enters there however it is routed after,
	// which is the only vantage point that sees them all.
	accepted atomic.Int64

	requireKey atomic.Bool
	// /api/v1/chat (chat.go): the two MCP switches, where mcp.json is, and
	// the conversations it keeps.
	mcpEphemeral, mcpConfigured atomic.Bool
	mcpFile                     string
	chatOnce                    sync.Once
	chat                        *chatapi.Runner
	// keys holds the named tokens apps may use instead of the node key.
	keys *nodekey.Store
	// cors holds the origins allowed to call /v1 from a browser (origin.go).
	cors corsOrigins
	// conf is the config file this node runs from, for Server settings.
	conf                      serverConfig
	frontListen, publicListen string
	keyHome                   string
	meshListen                string
	ids                       *tsid.Resolver
	meshAdmin                 string
	hub                       hubCache
	cancels                   cancellable
	llmd                      *llmd.LLMD
	// scheduler overrides where a model's requests are sent, for tests. nil
	// means ask llm-d (see front.go's scheduled).
	scheduler func(model string) (*url.URL, bool)
	llmdState string // persisted llm-d choice
	build     BuildInfo
	available availableCache
}

// SetRuntimeStore sets where the selected runtime is persisted.
func (s *Server) SetRuntimeStore(path string) { s.runtimeStore = path }

// SetSupervisorError records why the supervisor could not be built.
func (s *Server) SetSupervisorError(err error) {
	if err != nil {
		s.supErr = err.Error()
	}
}

// New builds the HTTP surface. sup may be nil on a node that only routes for
// its peers and hosts no models of its own.
func New(m *mesh.Mesh, r *router.Router, sup *supervisor.Supervisor, log *slog.Logger, ui http.Handler) *Server {
	mx := newMetrics()
	srv := &Server{m: m, r: r, sup: sup, log: log, ui: ui, metrics: mx, traffic: newTraffic(mx), tokens: tokentap.New()}
	srv.feed = newStateFeed(srv.feedSources(), feedEvery)
	// The front door's own count, published to peers with the rest of this
	// node's state: it is the only figure that sees every request, whatever
	// routes it afterwards.
	if m != nil {
		m.SetAcceptedProvider(func() int64 { return srv.accepted.Load() })
		// Advertised so the mesh knows which node is scheduling, not only
		// whether this one is: with an entrypoint the scheduler runs on a
		// machine with no GPUs, and a dashboard reading its own llm-d showed
		// "not running" through an entire benchmark that went through one.
		m.SetSchedulerProvider(func() *mesh.SchedulerState {
			if srv.llmd == nil {
				return nil
			}
			st := srv.llmd.Status()
			if st.State != "running" || st.Model == "" {
				return nil
			}
			return &mesh.SchedulerState{Model: st.Model, Profile: st.Profile, Engines: len(st.Endpoints)}
		})
	}
	// The shims run on the machine that holds the model, which under llm-d is
	// the only time that machine is in the request path at all.
	if sup != nil {
		sup.SetTokenTap(srv.tokens)
	}
	r.OnRoute = srv.traffic.publish
	// The router asks per request, so the dashboard's capture switch applies
	// to the next request rather than after a restart.
	r.Bodies = func() router.BodyLog {
		on, _, max, _ := srv.traffic.settings()
		return router.BodyLog{Enabled: on, Max: max}
	}
	if sup != nil && sup.JITEnabled() {
		r.JIT = func(ctx context.Context, model string, ttl int) (bool, error) {
			err := sup.EnsureLoaded(ctx, model, ttl)
			if errors.Is(err, supervisor.ErrJITDisabled) || errors.Is(err, supervisor.ErrNotInCatalog) {
				return false, nil // not JIT's to serve: the plain 404 stands
			}
			return true, err
		}
	}
	return srv
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// OpenAI-compatible surface.
	//
	// The set matches what LM Studio documents plus what the llama.cpp engine
	// actually serves, so existing tools work by pointing their base URL here.
	// Every one of these carries a "model" in a JSON body, which is what the
	// router needs to choose a destination.
	mux.HandleFunc("GET /v1/models", s.handleModels)
	mux.HandleFunc("GET /v1/models/{id}", s.handleModel)
	for _, p := range []string{
		"/v1/chat/completions",
		"/v1/completions",
		"/v1/embeddings",
		// Responses is what makes Codex work against a local server.
		"/v1/responses",
		// Anthropic-shaped messages, served by the same engine.
		"/v1/messages",
		"/v1/messages/count_tokens",
		"/v1/responses/input_tokens",
		"/v1/chat/completions/input_tokens",
		"/v1/rerank",
		"/v1/reranking",
	} {
		path := p
		mux.HandleFunc("POST "+path, func(w http.ResponseWriter, req *http.Request) {
			s.r.Forward(w, req, path)
		})
	}
	// Transcription bodies are multipart, so the model is a form field rather
	// than a JSON key.
	mux.HandleFunc("POST /v1/audio/transcriptions", func(w http.ResponseWriter, req *http.Request) {
		s.r.ForwardMultipart(w, req, "/v1/audio/transcriptions")
	})

	// LM Studio's enhanced REST surface. Same routing, richer model metadata.
	mux.HandleFunc("GET /api/v0/models", s.handleModelsV0)
	mux.HandleFunc("GET /api/v0/models/{id}", s.handleModelV0)
	for _, p := range []string{"/api/v0/chat/completions", "/api/v0/completions", "/api/v0/embeddings"} {
		path := p
		target := strings.TrimPrefix(path, "/api/v0")
		mux.HandleFunc("POST "+path, func(w http.ResponseWriter, req *http.Request) {
			s.r.Forward(w, req, "/v1"+target)
		})
	}

	// Mesh surface. /z/state is both the peer probe target and the liveness
	// signal, so it must stay cheap.
	mux.HandleFunc("GET /z/state", s.handleState)
	mux.HandleFunc("GET /z/mesh", s.handleMesh)
	mux.HandleFunc("GET /z/endpoints.yaml", s.handleLLMDEndpoints)
	mux.HandleFunc("GET /z/log/stream", s.handleTrafficStream)
	// The dashboard's state as it changes, so an open page stops polling.
	mux.HandleFunc("GET /api/v1/events", s.handleEvents)
	// Live reply text as it is generated. Not in peerAllowed, so this is a
	// loopback-only view of this node's own traffic — a peer asking for it
	// gets the usual "use this node's loopback address for management".
	mux.HandleFunc("GET /z/log/tokens", s.handleTokenStream)
	mux.HandleFunc("GET /api/v1/traffic", s.handleTrafficSettings)
	mux.HandleFunc("POST /api/v1/traffic", s.handleTrafficSettings)
	mux.HandleFunc("GET /api/v1/traffic/recent", s.handleTrafficRecent)
	mux.HandleFunc("GET /api/v1/tokens", s.handleTokens)
	mux.HandleFunc("POST /api/v1/tokens", s.handleTokenCreate)
	mux.HandleFunc("POST /api/v1/tokens/revoke", s.handleTokenRevoke)
	mux.HandleFunc("POST /api/v1/chat", s.handleChat)
	mux.HandleFunc("GET /api/v1/key", s.handleKeyInfo)
	mux.HandleFunc("POST /api/v1/key/rotate", s.handleKeyRotate)
	mux.HandleFunc("GET /api/v1/server-settings", s.handleGetServerSettings)
	mux.HandleFunc("PUT /api/v1/server-settings", s.handlePutServerSettings)
	mux.HandleFunc("GET /metrics", s.handleMetrics)
	mux.HandleFunc("GET /api/v1/doctor", s.handleDoctor)
	// Model management; handlers report unavailable when there is no supervisor.
	s.registerLifecycle(mux)

	s.registerHub(mux)
	s.registerShare(mux)
	mux.HandleFunc("GET /api/v1/topology", s.handleTopology)
	// Another node's management API, for this node's dashboard.
	mux.HandleFunc("/api/v1/nodes/{node}/{rest...}", s.handleNodeProxy)

	mux.HandleFunc("GET /z/version", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, s.build)
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})

	if s.ui != nil {
		mux.Handle("/", s.ui)
	}
	return logging(s.log, mux)
}

type modelObject struct {
	ID      string   `json:"id"`
	Object  string   `json:"object"`
	Created int64    `json:"created"`
	OwnedBy string   `json:"owned_by"`
	Nodes   []string `json:"nodes,omitempty"` // ModelFabric extension: who serves it
}

// handleModels returns the union of every model in the mesh. This is the
// endpoint that makes remote models indistinguishable from local ones.
func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	byModel := s.listedModels(r)
	ids := make([]string, 0, len(byModel))
	for id := range byModel {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	now := time.Now().Unix()
	data := make([]modelObject, 0, len(ids))
	for _, id := range ids {
		nodes := byModel[id]
		sort.Strings(nodes)
		data = append(data, modelObject{
			ID:      id,
			Object:  "model",
			Created: now,
			OwnedBy: "modelfabric",
			Nodes:   nodes,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

// listedModels is what /v1/models advertises. With JIT on that includes every
// model this node could load on demand, as LM Studio lists downloaded models
// when JIT is enabled — a client can only request what it can see. Models not
// loaded anywhere have no nodes. Tailnet callers cannot JIT, so they are shown
// only what is loaded.
func (s *Server) listedModels(r *http.Request) map[string][]string {
	owners := s.modelOwners()
	if s.sup != nil && s.sup.JITEnabled() && router.JITAllowed(r.Context()) {
		for _, m := range s.sup.Catalog().Models() {
			if _, ok := owners[m.Key]; !ok {
				owners[m.Key] = []string{}
			}
		}
	}
	return owners
}

func (s *Server) modelOwners() map[string][]string {
	owners := map[string][]string{}
	self := s.m.State()
	for _, mod := range self.Models {
		owners[mod] = append(owners[mod], self.Node)
	}
	for _, p := range s.m.Peers() {
		if !p.Alive {
			continue
		}
		for _, mod := range p.Models {
			owners[mod] = append(owners[mod], p.Node)
		}
	}
	return owners
}

func (s *Server) handleState(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.m.State())
}

// MeshView is the whole-mesh snapshot the UI renders.
type MeshView struct {
	Self   mesh.NodeState  `json:"self"`
	Peers  []mesh.PeerView `json:"peers"`
	Models []ModelView     `json:"models"`
	// Preferred is the operator's preferred node (mfsh prefer), if any.
	Preferred string `json:"preferred_node,omitempty"`
}

type ModelView struct {
	ID    string   `json:"id"`
	Nodes []string `json:"nodes"`
}

func (s *Server) handleMesh(w http.ResponseWriter, _ *http.Request) {
	owners := s.modelOwners()
	ids := make([]string, 0, len(owners))
	for id := range owners {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	models := make([]ModelView, 0, len(ids))
	for _, id := range ids {
		nodes := owners[id]
		sort.Strings(nodes)
		models = append(models, ModelView{ID: id, Nodes: nodes})
	}

	writeJSON(w, http.StatusOK, MeshView{
		Self:      s.m.State(),
		Peers:     s.m.Peers(),
		Models:    models,
		Preferred: s.m.Preferred(),
	})
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]any{
		"error": map[string]any{"message": msg, "type": "modelfabric_error", "code": code},
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// logging records request outcomes without touching the response stream, so
// SSE flushing behaviour is unaffected.
func logging(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Peer probes and UI polling would otherwise dominate the log.
		quiet := r.URL.Path == "/z/state" || r.URL.Path == "/z/mesh" || r.URL.Path == "/healthz" ||
			r.URL.Path == "/z/log/stream"
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sw, r)
		if !quiet {
			log.Debug("request",
				"method", r.Method, "path", r.URL.Path,
				"status", sw.status, "took", time.Since(start).Round(time.Millisecond))
		}
	})
}

type statusWriter struct {
	http.ResponseWriter
	status  int
	wrote   bool
	written int64
	// capture holds the response as written, up to captureMax, when body
	// logging is on. Nil otherwise, which is the default.
	capture    []byte
	captureMax int
}

func (w *statusWriter) Write(b []byte) (int, error) {
	n, err := w.ResponseWriter.Write(b)
	w.written += int64(n)
	if w.capture != nil && len(w.capture) < w.captureMax {
		w.capture = append(w.capture, b[:n]...)
	}
	return n, err
}

func (w *statusWriter) WriteHeader(code int) {
	if !w.wrote {
		w.status, w.wrote = code, true
	}
	w.ResponseWriter.WriteHeader(code)
}

// Unwrap lets http.ResponseController reach the underlying writer so Flush
// still works through this wrapper.
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// Endpoints lists every model server in the mesh, for llm-d's file-discovery
// plugin. Peers' instances are included, which is the whole point: an EPP wants
// the fleet, not one machine.
func (s *Server) Endpoints() []discovery.Endpoint {
	var out []discovery.Endpoint

	self := s.m.State()
	add := func(node, addr string, insts []mesh.InstanceState) {
		for _, i := range insts {
			if i.State != "ready" {
				continue
			}
			a := i.Address
			if a == "" {
				a = addr
			}
			out = append(out, discovery.Endpoint{
				// Instance ids are already unique mesh-wide.
				Name:    i.ID,
				Address: a,
				// llm-d scrapes whatever it routes to, so where ModelFabric
				// republishes an engine's metrics with a KV gauge, that is
				// what llm-d is given. Nothing else uses this port.
				Port:       metricsPort(i),
				EnginePort: i.Port,
				Labels: map[string]string{
					"modelfabric.sh/node":    node,
					"modelfabric.sh/model":   i.Model,
					"modelfabric.sh/runtime": i.Runtime,
					// Carried so a scheduler in front can stop retrying
					// image work on an engine that refuses it.
					"modelfabric.sh/vision": strconv.FormatBool(i.Vision),
				},
			})
			// Capacity and speed, for schedulers that can use them (and for
			// the operator reading the file): the fleet is heterogeneous.
			ep := &out[len(out)-1]
			// The EPP picks a metric mapping per endpoint from this label.
			// Without it every endpoint is scraped as vLLM and its queue
			// metrics silently read as absent — but claiming llama.cpp for an
			// engine that serves no Prometheus metrics at all (mlx-lm) would be
			// worse: the scheduler would read absent queues as idle. An engine
			// llm-d has no mapping for is left unlabelled, and an unset engine
			// is llama.cpp, which is all a peer too old to say can be running.
			if i.Engine == "" || i.Engine == "llama.cpp" {
				ep.Labels[discovery.EngineTypeLabel] = discovery.EngineTypeLlamaCPP
			}
			if i.KVUsage >= 0 {
				ep.Labels[discovery.KVUsageLabel] = strconv.FormatFloat(i.KVUsage, 'f', 4, 64)
			}
			if i.ServedModel != "" && i.ServedModel != i.Model {
				ep.Labels[discovery.ServedModelLabel] = i.ServedModel
			}
			if i.Slots > 0 {
				ep.Labels[discovery.SlotsLabel] = strconv.Itoa(i.Slots)
			}
			// Trusted only: llm-d schedules by this label, and a rate from a
			// few short prompts would place real traffic on the strength of
			// nothing. The dashboard shows the rough figure; the scheduler
			// does not get it.
			if i.PrefillTrusted && i.PrefillTokS > 0 {
				ep.Labels[discovery.PrefillLabel] = strconv.Itoa(int(i.PrefillTokS))
			}
		}
	}
	add(self.Node, self.Addr, self.Instances)
	for _, p := range s.m.Peers() {
		if p.Alive {
			add(p.Node, p.Addr, p.Instances)
		}
	}
	return out
}

func (s *Server) handleLLMDEndpoints(w http.ResponseWriter, _ *http.Request) {
	data, err := discovery.Render(s.Endpoints())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/yaml")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

// handleModel implements GET /v1/models/{id}, which OpenAI clients use to
// confirm a model exists before using it.
func (s *Server) handleModel(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	owners := s.listedModels(r)
	nodes, ok := owners[id]
	if !ok {
		writeError(w, http.StatusNotFound, "no model "+id+" in the mesh")
		return
	}
	sort.Strings(nodes)
	writeJSON(w, http.StatusOK, modelObject{
		ID: id, Object: "model", Created: time.Now().Unix(),
		OwnedBy: "modelfabric", Nodes: nodes,
	})
}

// modelV0 is LM Studio's richer model shape: it distinguishes loaded from
// available and carries quantization, context and architecture.
type modelV0 struct {
	ID                string `json:"id"`
	Object            string `json:"object"`
	Type              string `json:"type"`
	Publisher         string `json:"publisher,omitempty"`
	Arch              string `json:"arch,omitempty"`
	CompatibilityType string `json:"compatibility_type,omitempty"`
	Quantization      string `json:"quantization,omitempty"`
	State             string `json:"state"`
	MaxContextLength  int    `json:"max_context_length,omitempty"`
	// Capabilities follows LM Studio's v0 vocabulary, which lists only
	// "tool_use"; clients use it to decide whether to offer tools at all.
	Capabilities []string `json:"capabilities,omitempty"`
	Nodes        []string `json:"nodes,omitempty"`
}

func (s *Server) modelsV0() []modelV0 {
	loaded := map[string]bool{}
	byKey := map[string]modelV0{}

	if s.sup != nil {
		for _, inst := range s.sup.Instances() {
			loaded[inst.Model] = true
		}
		for _, m := range s.sup.Catalog().Models() {
			v := modelV0{
				ID: m.Key, Object: "model", Type: m.Type, Publisher: m.Publisher,
				Arch: m.Architecture, CompatibilityType: m.Format,
				Quantization: m.Quantization, MaxContextLength: m.MaxContextLength,
			}
			for _, c := range m.Capabilities {
				switch c {
				case "tool_use":
					v.Capabilities = append(v.Capabilities, c)
				case "vision":
					// LM Studio reports vision models as type "vlm".
					if v.Type == "llm" {
						v.Type = "vlm"
					}
				}
			}
			byKey[m.Key] = v
		}
	}
	// Models held only by peers are still usable through this node.
	for id, nodes := range s.modelOwners() {
		v, ok := byKey[id]
		if !ok {
			v = modelV0{ID: id, Object: "model", Type: "llm"}
		}
		v.Nodes = nodes
		sort.Strings(v.Nodes)
		loaded[id] = true
		byKey[id] = v
	}

	ids := make([]string, 0, len(byKey))
	for id := range byKey {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]modelV0, 0, len(ids))
	for _, id := range ids {
		v := byKey[id]
		v.State = "not-loaded"
		if loaded[id] {
			v.State = "loaded"
		}
		out = append(out, v)
	}
	return out
}

func (s *Server) handleModelsV0(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": s.modelsV0()})
}

func (s *Server) handleModelV0(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	for _, m := range s.modelsV0() {
		if m.ID == id {
			writeJSON(w, http.StatusOK, m)
			return
		}
	}
	writeError(w, http.StatusNotFound, "no model "+id)
}

// SetAuth wires the node's API key, and whether the loopback front door
// requires it. The public listener always does.
func (s *Server) SetAuth(key func() (string, error), requireOnLoopback bool) {
	s.apiKey = key
	s.requireKey.Store(requireOnLoopback)
}

// SetTokens wires the named tokens accepted alongside the node key.
func (s *Server) SetTokens(store *nodekey.Store) { s.keys = store }

// SetMCP gives the node its two MCP switches and the mcp.json it reads.
func (s *Server) SetMCP(file string, ephemeral, configured bool) {
	s.mcpFile = file
	s.mcpEphemeral.Store(ephemeral)
	s.mcpConfigured.Store(configured)
}

// SetKeyHome is the directory holding the node key, which `mfsh key` compares
// with its own and the dashboard's Rotate replaces. Unset, neither is offered.
func (s *Server) SetKeyHome(home string) { s.keyHome = home }

// SetFrontDoor records where apps connect, for status: the loopback front
// door and, if configured, the public listener.
func (s *Server) SetFrontDoor(listen, public string) { s.frontListen, s.publicListen = listen, public }

// metricsPort is the port a scheduler should dial for an instance: ModelFabric's
// metrics shim where the engine publishes no KV gauge of its own, else the
// engine itself. The shim proxies inference straight through, so routing is
// unaffected either way.
func metricsPort(i mesh.InstanceState) int {
	if i.MetricsPort > 0 {
		return i.MetricsPort
	}
	return i.Port
}
