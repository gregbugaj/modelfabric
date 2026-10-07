package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	goruntime "runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gregbugaj/modelfabric/internal/bench"
)

// Local engine benchmark using internal/bench.
//
// POST /api/v1/bench starts a run; GET returns progress and the report;
// DELETE cancels and restores the engine. Runs outlive HTTP requests so a
// disconnect does not leave benchmark settings loaded.

type benchState struct {
	mu      sync.Mutex
	running bool
	rep     *bench.Report
	err     string
	cancel  context.CancelFunc
}

type benchStatus struct {
	Node    string        `json:"node"`
	Running bool          `json:"running"`
	Report  *bench.Report `json:"report,omitempty"`
	Error   string        `json:"error,omitempty"`
}

func (s *Server) registerBench(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/bench", s.handleBenchStatus)
	mux.HandleFunc("POST /api/v1/bench", s.handleBenchStart)
	mux.HandleFunc("DELETE /api/v1/bench", s.handleBenchCancel)
}

func (s *Server) handleBenchStatus(w http.ResponseWriter, r *http.Request) {
	s.bench.mu.Lock()
	defer s.bench.mu.Unlock()
	// ?format=text is the report as the CLI prints it, for the dashboard's
	// copy button: one renderer, so the two never disagree.
	if r.URL.Query().Get("format") == "text" {
		if s.bench.rep == nil {
			writeError(w, http.StatusNotFound, "no benchmark has run on this node")
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte(s.bench.rep.Text()))
		return
	}
	writeJSON(w, http.StatusOK, benchStatus{Node: s.m.State().Node, Running: s.bench.running, Report: s.bench.rep, Error: s.bench.err})
}

func (s *Server) handleBenchStart(w http.ResponseWriter, r *http.Request) {
	if !s.requireSupervisor(w) {
		return
	}
	var cfg bench.Config
	if r.ContentLength != 0 {
		if err := decodeBody(w, r, &cfg, 1<<20); err != nil {
			writeError(w, http.StatusBadRequest, "parse request body: "+err.Error())
			return
		}
	}
	if cfg.Model == "" {
		for _, i := range s.m.State().Instances {
			cfg.Model = i.Model
			break
		}
		if cfg.Model == "" {
			writeError(w, http.StatusBadRequest, `no model is loaded on this node and none was named: send {"model": "..."}`)
			return
		}
	}
	cfg.Fill()
	if _, err := bench.LoadCorpus(cfg.Prompts); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	ctx, err := s.claimBench()
	if err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	eng := benchEngine{supEngine{s}}
	base := s.benchReport(cfg)
	s.bench.mu.Lock()
	s.bench.rep = &base
	s.bench.mu.Unlock()

	go func() {
		rep, err := bench.Run(ctx, eng, bench.NvidiaMemory, cfg, func(r bench.Report) {
			s.fillBenchReport(&r, base)
			s.bench.mu.Lock()
			s.bench.rep = &r
			s.bench.mu.Unlock()
		})
		s.fillBenchReport(&rep, base)
		s.bench.mu.Lock()
		s.bench.running = false
		s.bench.rep = &rep
		if err != nil {
			s.bench.err = err.Error()
		}
		s.bench.mu.Unlock()
		if err != nil {
			s.log.Warn("benchmark failed", "model", cfg.Model, "err", err)
			return
		}
		s.log.Info("benchmark finished", "model", cfg.Model, "took", time.Duration(rep.Took*float64(time.Second)).Round(time.Second))
	}()
	writeJSON(w, http.StatusAccepted, benchStatus{Node: s.m.State().Node, Running: true, Report: &base})
}

// claimBench reserves the engine. A benchmark and a tuning sweep both reload
// it; run together, each would measure a load the other just replaced.
func (s *Server) claimBench() (context.Context, error) {
	s.tune.mu.Lock()
	tuning := s.tune.running
	s.tune.mu.Unlock()
	if tuning {
		return nil, fmt.Errorf("a slot-tuning sweep is running on this node; wait for it or stop it with DELETE /api/v1/tune")
	}
	s.bench.mu.Lock()
	defer s.bench.mu.Unlock()
	if s.bench.running {
		return nil, fmt.Errorf("a benchmark is already running on this node; wait for it or stop it with DELETE /api/v1/bench")
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.bench.running, s.bench.cancel, s.bench.err, s.bench.rep = true, cancel, "", nil
	return ctx, nil
}

func (s *Server) handleBenchCancel(w http.ResponseWriter, _ *http.Request) {
	s.bench.mu.Lock()
	running, cancel := s.bench.running, s.bench.cancel
	s.bench.mu.Unlock()
	if !running {
		writeError(w, http.StatusConflict, "no benchmark is running on this node")
		return
	}
	cancel()
	writeJSON(w, http.StatusOK, map[string]any{"stopped": true, "message": "stopping; the engine is put back as it was found"})
}

func (s *Server) benchReport(cfg bench.Config) bench.Report {
	st := s.m.State()
	rep := bench.Report{Node: st.Node, Model: cfg.Model, At: time.Now(), Config: cfg}
	rep.Machine.Version = s.build.Version
	rep.Machine.Platform = st.Platform
	if rep.Machine.Platform == "" {
		rep.Machine.Platform = goruntime.GOOS + "/" + goruntime.GOARCH
	}
	rep.Machine.OS = st.OSVersion
	rep.Machine.GPU, rep.Machine.Driver, rep.Machine.VRAMMB = bench.GPUInfo()
	if m, ok := s.sup.Catalog().Lookup(cfg.Model); ok {
		rep.Engine.Quant = m.Quantization
		rep.Engine.SizeMB = int(m.SizeBytes >> 20)
		if m.Path != "" {
			rep.Engine.File = filepath.Base(m.Path)
		}
	}
	rep.Command = benchCommand(st.Node, cfg)
	return rep
}

func (s *Server) fillBenchReport(r *bench.Report, base bench.Report) {
	r.Node, r.Machine, r.Command = base.Node, base.Machine, base.Command
	r.Engine.Quant, r.Engine.SizeMB, r.Engine.File = base.Engine.Quant, base.Engine.SizeMB, base.Engine.File
}

// benchCommand is the command line that repeats this run, written out in full
// so a report says exactly what was asked even where it matches the defaults.
func benchCommand(node string, c bench.Config) string {
	join := func(v []int) string {
		s := make([]string, len(v))
		for i, n := range v {
			s[i] = strconv.Itoa(n)
		}
		return strings.Join(s, ",")
	}
	cmd := fmt.Sprintf("mfsh bench -model %s -prompts %s -pp %s -tg %d -batch %s -batch-pp %d -reps %d",
		c.Model, c.Prompts, join(c.PP), c.TG, join(c.Batch), c.BatchPP, c.Reps)
	if node != "" {
		cmd += " -node " + node
	}
	return cmd
}

type benchEngine struct{ supEngine }

func (e benchEngine) Loaded(_ context.Context, model string) (int, string, map[string]any, error) {
	i := e.instance(model)
	if i.ID == "" {
		return 0, "", nil, fmt.Errorf("no engine for %s", model)
	}
	var settings map[string]any
	b, _ := json.Marshal(i.Config)
	_ = json.Unmarshal(b, &settings)
	return i.PID, i.Runtime, settings, nil
}
