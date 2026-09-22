package server

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gregbugaj/modelfabric/internal/bench"
	"github.com/gregbugaj/modelfabric/internal/mesh"
)

// The cluster benchmark: the whole setup measured as one system.
//
//	POST   /api/v1/bench/cluster  start   {"model": ..., "concurrency": [...], ...}
//	GET    /api/v1/bench/cluster  progress, then the report; ?format=text
//	DELETE /api/v1/bench/cluster  stop
//
// Requests go through this node's own front door, so they are placed as an
// app's are: by the router, or by llm-d for the model it schedules, across
// every node holding the model. Nothing is reloaded. The run is held here
// rather than in the browser so it survives the tab closing, like every
// other run.

type clusterState struct {
	mu      sync.Mutex
	running bool
	report  *bench.ClusterReport
	err     string
	cancel  context.CancelFunc
}

type clusterStatus struct {
	Running bool                 `json:"running"`
	Report  *bench.ClusterReport `json:"report,omitempty"`
	Error   string               `json:"error,omitempty"`
}

func (s *Server) registerBenchCluster(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/bench/cluster", s.handleClusterStatus)
	mux.HandleFunc("POST /api/v1/bench/cluster", s.handleClusterStart)
	mux.HandleFunc("DELETE /api/v1/bench/cluster", s.handleClusterCancel)
}

func (s *Server) handleClusterStatus(w http.ResponseWriter, r *http.Request) {
	s.cluster.mu.Lock()
	st := clusterStatus{Running: s.cluster.running, Report: s.cluster.report, Error: s.cluster.err}
	s.cluster.mu.Unlock()
	if r.URL.Query().Get("format") == "text" {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		if st.Report == nil {
			_, _ = w.Write([]byte("no cluster benchmark has run on this node\n"))
			return
		}
		_, _ = w.Write([]byte(st.Report.Text()))
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) handleClusterStart(w http.ResponseWriter, r *http.Request) {
	var cfg bench.ClusterConfig
	if err := decodeBody(w, r, &cfg, 1<<20); err != nil {
		writeError(w, http.StatusBadRequest, `send {"model": ...}, with any of prompts, pp, tg, concurrency, load_pp, reps`)
		return
	}
	if cfg.Model == "" {
		writeError(w, http.StatusBadRequest, "name the model to benchmark")
		return
	}
	cfg.Fill()
	if _, err := bench.LoadCorpus(cfg.Prompts); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	for _, n := range cfg.Concurrency {
		if n < 1 || n > 256 {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("concurrency %d is out of range: 1 to 256", n))
			return
		}
	}
	base, err := s.frontBase()
	if err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	rep := s.clusterReport(cfg)
	if len(rep.Holders) == 0 {
		writeError(w, http.StatusConflict, fmt.Sprintf("no node in the mesh holds %s; load it on one or more nodes first", cfg.Model))
		return
	}
	var key string
	if s.requireKey.Load() && s.apiKey != nil {
		if key, err = s.apiKey(); err != nil {
			writeError(w, http.StatusInternalServerError, "the front door asks for a key and this node's cannot be read: "+err.Error())
			return
		}
	}

	s.cluster.mu.Lock()
	if s.cluster.running {
		s.cluster.mu.Unlock()
		writeError(w, http.StatusConflict, "a cluster benchmark is already running; wait for it or stop it with DELETE /api/v1/bench/cluster")
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	rep.At = time.Now()
	s.cluster.running, s.cluster.report, s.cluster.err, s.cluster.cancel = true, &rep, "", cancel
	s.cluster.mu.Unlock()

	go s.runCluster(ctx, base, key, cfg, rep)
	w.WriteHeader(http.StatusAccepted)
	s.handleClusterStatus(w, r)
}

func (s *Server) runCluster(ctx context.Context, base, key string, cfg bench.ClusterConfig, rep bench.ClusterReport) {
	keep := func(r bench.ClusterReport) {
		s.cluster.mu.Lock()
		s.cluster.report = &r
		s.cluster.mu.Unlock()
	}
	out, err := bench.RunCluster(ctx, base, key, cfg, rep, keep)
	s.cluster.mu.Lock()
	s.cluster.running, s.cluster.report = false, &out
	if err != nil {
		s.cluster.err = err.Error()
	}
	s.cluster.mu.Unlock()
	s.log.Info("cluster benchmark finished", "model", cfg.Model, "holders", len(rep.Holders), "err", err)
}

func (s *Server) handleClusterCancel(w http.ResponseWriter, _ *http.Request) {
	s.cluster.mu.Lock()
	running, cancel := s.cluster.running, s.cluster.cancel
	s.cluster.mu.Unlock()
	if !running {
		writeError(w, http.StatusConflict, "no cluster benchmark is running")
		return
	}
	cancel()
	writeJSON(w, http.StatusOK, map[string]any{"stopped": true, "message": "stopping; requests already sent finish on their engines"})
}

// frontBase is this node's front door as this node can dial it. A wildcard
// listener is dialled on loopback, which every bind includes.
func (s *Server) frontBase() (string, error) {
	if s.frontListen == "" {
		return "", fmt.Errorf("this node has no front door to send the benchmark through")
	}
	host, port, err := net.SplitHostPort(s.frontListen)
	if err != nil {
		return "", fmt.Errorf("front door %q: %v", s.frontListen, err)
	}
	if ip := net.ParseIP(host); host == "" || (ip != nil && ip.IsUnspecified()) {
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port), nil
}

// clusterReport fills in what the run is measuring before it starts: which
// door, who routes the model, and the nodes holding it. The holders are
// recorded as the run began; the report says where requests actually went.
func (s *Server) clusterReport(cfg bench.ClusterConfig) bench.ClusterReport {
	self := s.m.State()
	rep := bench.ClusterReport{Model: cfg.Model, Config: cfg, Entry: self.Node, Routing: "ModelFabric router"}
	scheds := []*mesh.SchedulerState{self.Scheduler}
	// This node's engines include any it was configured with rather than
	// loaded, which have no instance. A peer publishes its instances; one
	// that lists the model with none (an external engine) is a holder whose
	// engines are not known, and is recorded with 0 rather than a guess.
	self.Engines = slices.DeleteFunc(slices.Clone(self.Engines), func(e mesh.EngineState) bool { return !e.Healthy })
	h := bench.ClusterNode{Node: self.Node, Platform: self.Platform}
	for _, e := range self.Engines {
		if slices.Contains(e.Models, cfg.Model) {
			h.Engines++
			h.Slots += int(e.Slots)
		}
	}
	if h.Engines > 0 {
		rep.Holders = append(rep.Holders, h)
	}
	for _, p := range s.m.Peers() {
		if !p.Alive {
			continue
		}
		scheds = append(scheds, p.Scheduler)
		h := bench.ClusterNode{Node: p.Node, Platform: p.Platform}
		for _, in := range p.Instances {
			if in.Model == cfg.Model || in.ServedModel == cfg.Model {
				h.Engines++
				h.Slots += in.Slots
			}
		}
		if h.Engines > 0 || slices.Contains(p.Models, cfg.Model) {
			rep.Holders = append(rep.Holders, h)
		}
	}
	sort.Slice(rep.Holders, func(i, j int) bool { return rep.Holders[i].Node < rep.Holders[j].Node })
	for _, sc := range scheds {
		if sc != nil && sc.Model == cfg.Model {
			rep.Routing = "llm-d"
			if sc.Profile != "" {
				rep.Routing += " (" + sc.Profile + ")"
			}
		}
	}
	rep.Command = clusterCommand(cfg)
	return rep
}

// clusterCommand is the run as `mfsh bench -cluster`, which reproduces it
// from a terminal on the same entry node.
func clusterCommand(cfg bench.ClusterConfig) string {
	ints := func(v []int) string {
		s := make([]string, len(v))
		for i, n := range v {
			s[i] = fmt.Sprint(n)
		}
		return strings.Join(s, ",")
	}
	return fmt.Sprintf("mfsh bench -cluster -model %s -prompts %s -pp %s -tg %d -concurrency %s -load-pp %d -reps %d",
		cfg.Model, cfg.Prompts, ints(cfg.PP), cfg.TG, ints(cfg.Concurrency), cfg.LoadPP, cfg.Reps)
}
