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

// Slot tuning uses the local supervisor and runs independently of HTTP requests.
// POST /api/v1/tune starts a sweep; GET returns progress and the report;
// DELETE cancels it. The sweep restores original settings, including on cancel.
// Each node measures its own hardware; peers use /api/v1/nodes/{node}/tune.

// tuneState permits one sweep per node. Concurrent sweeps replace each other's
// engine loads and invalidate measurements.
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
	// ContextLength is per request; zero uses the loaded model's context.
	ContextLength int   `json:"context_length"`
	Prompt        int   `json:"prompt"`
	Output        int   `json:"output"`
	Slots         []int `json:"slots"`
}

type tuneStatus struct {
	Node      string        `json:"node"`
	Running   bool          `json:"running"`
	Config    tuner.Config  `json:"config"`
	Rows      []tuner.Row   `json:"rows"`
	Report    *tuner.Report `json:"report,omitempty"`
	Error     string        `json:"error,omitempty"`
	StartedAt time.Time     `json:"started_at,omitempty"`
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

// claimTune reserves the single sweep independently of supervisor availability.
// The returned context outlives the initiating request and is cancelled by DELETE.
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
	// Assign fields individually: replacing tuneState would replace the locked
	// mutex and make the deferred Unlock panic.
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
	// Preserve running vision and speculation settings so saved defaults cannot
	// change them during the sweep. Other unspecified fields use model defaults.
	req := supervisor.LoadRequest{Model: model}
	req.ContextLength = &contextLen
	req.Parallel = &slots
	if before.ID != "" {
		vision, spec := before.Config.Vision, specModeOf(before.Config)
		req.Vision = &vision
		req.SpecMode = &spec
		// Preserve the runtime and weights variant so reloads cannot switch between
		// GGUF and MLX while reporting the original engine's measurements.
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
	after := e.instance(model)
	switch {
	case after.ID == "":
		return fmt.Errorf("the load reported success but no instance is serving %s", model)
	case after.Config.Parallel != slots:
		return fmt.Errorf("asked for %d slot(s) and the engine loaded %d", slots, after.Config.Parallel)
	}
	return nil
}

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
	// Measure the engine directly, using its bound address. A tailnet-bound
	// engine does not accept loopback connections.
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

// formatOf returns the loaded variant's format. Empty selects the catalog's
// primary variant when the original format is unknown.
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
