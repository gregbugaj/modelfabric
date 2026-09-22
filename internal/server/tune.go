package server

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/gregbugaj/modelfabric/internal/runtime"
	"github.com/gregbugaj/modelfabric/internal/supervisor"
	"github.com/gregbugaj/modelfabric/internal/tuner"
)

// Slot tuning on this node: internal/tuner's sweep, driven from the supervisor
// rather than over HTTP.
//
//	POST   /api/v1/tune  start a sweep     {"model", "context_length", "prompt", "output", "slots"}
//	GET    /api/v1/tune  progress, then the report
//	DELETE /api/v1/tune  stop the sweep and put the node back
//
// A sweep is started and then polled rather than answered by one long request.
// It reloads the engine once per slot count and runs for minutes, and a caller
// that goes away — a closed browser tab, a dropped ssh session — must not leave
// the machine on whichever count was being tested when the connection died. So
// the work outlives the request that asked for it, and the last report stays
// readable afterwards.
//
// Tuning a whole fleet is this endpoint reached through the node proxy
// (/api/v1/nodes/{node}/tune) once per node: each machine measures its own
// hardware, because a slot count is a fact about one machine and nothing about
// a peer's GPU can be read from here. Nothing is applied — the sweep restores
// what it found and reports what it would recommend.

// tuneState is the one sweep this node will run at a time. A second concurrent
// sweep would fight the first for the same engine: both unload it, each waits
// for a slot count the other just replaced, and every row times out while the
// mesh reports the engine healthy throughout. That is not hypothetical — it is
// what two overlapping sweeps did on minion, which reported "did not run" for
// every row of a configuration that had measured fine minutes earlier.
type tuneState struct {
	mu      sync.Mutex
	running bool
	cfg     tuner.Config
	rows    []tuner.Row
	rep     *tuner.Report
	err     string
	at      time.Time
	cancel  context.CancelFunc
}

func (s *Server) registerTune(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/tune", s.handleTuneStatus)
	mux.HandleFunc("POST /api/v1/tune", s.handleTuneStart)
	mux.HandleFunc("DELETE /api/v1/tune", s.handleTuneCancel)
}

type tuneRequest struct {
	Model string `json:"model"`
	// ContextLength is per request. Zero means what the model is loaded with:
	// tuning a context nobody runs answers a question nobody asked.
	ContextLength int   `json:"context_length"`
	Prompt        int   `json:"prompt"`
	Output        int   `json:"output"`
	Slots         []int `json:"slots"`
}

type tuneStatus struct {
	Node    string        `json:"node"`
	Running bool          `json:"running"`
	Config  tuner.Config  `json:"config"`
	Rows    []tuner.Row   `json:"rows"`
	Report  *tuner.Report `json:"report,omitempty"`
	Error   string        `json:"error,omitempty"`
	// StartedAt dates the report as well as the run: a recommendation is only
	// as good as the hardware and build it was measured on.
	StartedAt time.Time `json:"started_at,omitempty"`
}

func (s *Server) handleTuneStatus(w http.ResponseWriter, _ *http.Request) {
	s.tune.mu.Lock()
	defer s.tune.mu.Unlock()
	st := tuneStatus{
		Node:    s.m.State().Node,
		Running: s.tune.running,
		Config:  s.tune.cfg,
		Rows:    s.tune.rows,
		Report:  s.tune.rep,
		Error:   s.tune.err,
	}
	if st.Rows == nil {
		st.Rows = []tuner.Row{}
	}
	if !s.tune.at.IsZero() {
		st.StartedAt = s.tune.at
	}
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) handleTuneStart(w http.ResponseWriter, r *http.Request) {
	if !s.requireSupervisor(w) {
		return
	}
	var req tuneRequest
	if r.ContentLength != 0 {
		if err := decodeBody(w, r, &req, 1<<20); err != nil {
			writeError(w, http.StatusBadRequest, "parse request body: "+err.Error())
			return
		}
	}
	eng := supEngine{s}
	if req.Model == "" {
		// The model this node is running. A sweep names it in the report, so a
		// fleet view can refuse to compare two nodes serving different models.
		for _, i := range s.m.State().Instances {
			req.Model = i.Model
			break
		}
		if req.Model == "" {
			writeError(w, http.StatusBadRequest,
				`no model is loaded on this node and none was named: send {"model": "..."}`)
			return
		}
	}
	cfg := tuner.Config{Model: req.Model, Context: req.ContextLength,
		Prompt: req.Prompt, Output: req.Output, Slots: req.Slots}
	if cfg.Context == 0 {
		_, cfg.Context, _ = eng.Current(r.Context(), cfg.Model)
	}
	cfg.Fill()

	ctx, cancel, err := s.claimTune(cfg)
	if err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}

	go func() {
		defer cancel()
		rep, err := tuner.Run(ctx, eng, cfg, func(row tuner.Row) {
			s.tune.mu.Lock()
			s.tune.rows = append(s.tune.rows, row)
			s.tune.mu.Unlock()
		})
		rep.Node = s.m.State().Node
		s.tune.mu.Lock()
		s.tune.running = false
		s.tune.rep = &rep
		if err != nil {
			s.tune.err = err.Error()
		}
		s.tune.mu.Unlock()
		if err != nil {
			s.log.Warn("slot tuning failed", "model", cfg.Model, "err", err)
			return
		}
		s.log.Info("slot tuning finished", "model", cfg.Model,
			"recommended", rep.Recommended, "restored_to", rep.RestoredTo)
	}()

	writeJSON(w, http.StatusAccepted, tuneStatus{
		Node: s.m.State().Node, Running: true, Config: cfg,
		Rows: []tuner.Row{}, StartedAt: s.tune.at,
	})
}

// claimTune reserves this node's one sweep, and is where a second one is
// refused. Separate from the handler because the refusal is the interesting
// part: a node with no supervisor answers 503 before this is ever reached, so
// the rule needs to be assertable without one.
//
// The returned context belongs to the sweep rather than to the request that
// started it: a sweep survives the browser tab or the ssh session that asked
// for it, and is stopped only by DELETE.
func (s *Server) claimTune(cfg tuner.Config) (context.Context, context.CancelFunc, error) {
	// A benchmark reloads the same engine; one at a time, whichever came first.
	s.bench.mu.Lock()
	benching := s.bench.running
	s.bench.mu.Unlock()
	if benching {
		return nil, nil, fmt.Errorf("a benchmark is running on this node; wait for it or stop it with DELETE /api/v1/bench")
	}
	s.tune.mu.Lock()
	defer s.tune.mu.Unlock()
	if s.tune.running {
		return nil, nil, fmt.Errorf(
			"a sweep is already running on this node (%s, started %s); wait for it or stop it with DELETE /api/v1/tune",
			s.tune.cfg.Model, s.tune.at.Format(time.TimeOnly))
	}
	ctx, cancel := context.WithCancel(context.Background())
	// Field by field, not `s.tune = tuneState{...}`: that assignment replaces
	// the mutex this function is holding, and the deferred unlock then releases
	// a different, never-locked one — "sync: unlock of unlocked mutex", which
	// kills the process rather than the request.
	s.tune.running, s.tune.cfg, s.tune.at, s.tune.cancel = true, cfg, time.Now(), cancel
	s.tune.rows, s.tune.rep, s.tune.err = nil, nil, ""
	return ctx, cancel, nil
}

func (s *Server) handleTuneCancel(w http.ResponseWriter, _ *http.Request) {
	s.tune.mu.Lock()
	running, cancel := s.tune.running, s.tune.cancel
	s.tune.mu.Unlock()
	if !running {
		writeError(w, http.StatusConflict, "no sweep is running on this node")
		return
	}
	// tuner.Run restores the slot count it found before returning, including
	// on a cancelled sweep, so stopping leaves the node as it was rather than
	// on whichever row was in flight.
	cancel()
	writeJSON(w, http.StatusOK, map[string]any{
		"stopped": true,
		"message": "stopping; the node is put back on the slot count the sweep found",
	})
}

// supEngine is the local machine, driven through its own supervisor. The CLI's
// httpEngine drives a node over its management API; both run the same sweep.
type supEngine struct{ s *Server }

// reloadWait bounds one row's reload. A load that has not produced a serving
// engine by now is a failed row, not a slow one.
const reloadWait = 3 * time.Minute

func (e supEngine) Reload(ctx context.Context, model string, contextLen, slots int) error {
	before := e.instance(model)
	if before.ID != "" {
		op, err := e.s.sup.Unload(before.ID)
		if err != nil {
			return fmt.Errorf("could not unload %s: %w", before.ID, err)
		}
		if op != nil {
			e.s.sup.Journal().Wait(op.ID, reloadWait)
		}
	}
	// Only the settings that change what is being measured are stated; the
	// rest come from the model's own defaults, exactly as an `mfsh load` would
	// take them. Vision and speculation are carried over from what was running
	// rather than left out, because a saved default for either would otherwise
	// decide them halfway through a sweep — and a sweep that changes two
	// things at once measures neither.
	req := supervisor.LoadRequest{Model: model}
	req.ContextLength = &contextLen
	req.Parallel = &slots
	if before.ID != "" {
		vision, spec := before.Config.Vision, specModeOf(before.Config)
		req.Vision = &vision
		req.SpecMode = &spec
		// And the engine, plus the weights it serves. A Mac holds this model as
		// GGUF and as MLX and can run either; without these the reload takes the
		// node's default runtime and the sweep reports one engine's numbers under
		// the other's name.
		req.Runtime = before.Runtime
		req.Format = e.formatOf(model, before.Variant)
	}
	op, _, err := e.s.sup.Load(req)
	if err != nil {
		return err
	}
	settled, done := e.s.sup.Journal().Wait(op.ID, reloadWait)
	switch {
	case settled == nil:
		return fmt.Errorf("the load operation vanished from the journal")
	case !done:
		return fmt.Errorf("the engine did not come up with %d slot(s) within %s", slots, reloadWait)
	case settled.Error != "":
		return fmt.Errorf("%s", settled.Error)
	}
	// Confirm what actually loaded. A row measured against settings other than
	// the ones it reports is worse than a missing row: it looks like data.
	after := e.instance(model)
	switch {
	case after.ID == "":
		return fmt.Errorf("the load reported success but no instance is serving %s", model)
	case after.Config.Parallel != slots:
		return fmt.Errorf("asked for %d slot(s) and the engine loaded %d", slots, after.Config.Parallel)
	}
	return nil
}

// specModeOf reads a loaded instance's speculation back as the setting that
// would reproduce it.
func specModeOf(a runtime.Applied) string {
	if !a.Speculative {
		return "off"
	}
	if a.SpecType == "draft-simple" {
		return "draft"
	}
	return "mtp"
}

func (e supEngine) instance(model string) supervisor.Instance {
	for _, i := range e.s.sup.Instances() {
		if i.Model == model {
			return i
		}
	}
	return supervisor.Instance{}
}

func (e supEngine) Target(_ context.Context, model string) (string, string, error) {
	i := e.instance(model)
	if i.ID == "" || i.Port == 0 {
		return "", "", fmt.Errorf("no ready engine for %s after loading", model)
	}
	// The id this engine answers to, which is not always the catalog's: mlx-lm
	// recognises only the name it was started with.
	served := model
	for _, st := range e.s.m.State().Instances {
		if st.ID == i.ID && st.ServedModel != "" {
			served = st.ServedModel
		}
	}
	// The engine's own address, not the shim's and not the front door's: the
	// measurement is of this machine, not of wherever a router would have sent
	// the work. The address the router itself dials, not 127.0.0.1 and the
	// port: an engine bound to the tailnet (engine_bind) does not listen on
	// loopback, and the sweep refused to connect on exactly that node.
	for _, eng := range e.s.m.Engines() {
		if eng.Name == i.ID && eng.BaseURL != "" {
			return eng.BaseURL, served, nil
		}
	}
	return fmt.Sprintf("http://127.0.0.1:%d", i.Port), served, nil
}

func (e supEngine) Inflight(_ context.Context, model string) (int, error) {
	var n int64
	for _, i := range e.s.m.State().Instances {
		if i.Model == model {
			n += i.Inflight
		}
	}
	return int(n), nil
}

func (e supEngine) Current(_ context.Context, model string) (int, int, error) {
	i := e.instance(model)
	return i.Config.Parallel, i.Config.ContextLength, nil
}

// formatOf is the weights format of the variant an instance loaded, so a reload
// picks the same ones. Empty when it cannot be determined, which leaves the
// catalog's primary variant — right for a model that has only one.
func (e supEngine) formatOf(model, variant string) string {
	if variant == "" || e.s.sup == nil {
		return ""
	}
	for _, m := range e.s.sup.Catalog().VariantsOf(model) {
		if m.PathKey == variant {
			return m.Format
		}
	}
	return ""
}
