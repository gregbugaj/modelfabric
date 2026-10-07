package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/gregbugaj/modelfabric/internal/discovery"
	"github.com/gregbugaj/modelfabric/internal/llmd"
)

// llm-d schedules one model through Envoy and EPP; all other models use the
// mesh router. Its management routes use the /api/v1 access controls.

type llmdState struct {
	Enabled     bool   `json:"enabled"`
	Model       string `json:"model,omitempty"`
	Profile     string `json:"profile,omitempty"`
	PrefixCache bool   `json:"prefix_cache"`
	// RoomFilter is a pointer so state saved before it existed reads as the
	// default (on) rather than off.
	RoomFilter *bool `json:"room_filter,omitempty"`
	// KVCeiling and KVScorer are off unless asked for: on llama.cpp a full KV
	// cache means warm, not busy (see discovery.EPPOptions).
	KVCeiling float64 `json:"kv_ceiling,omitempty"`
	KVScorer  int     `json:"kv_scorer,omitempty"`
}

func (st llmdState) options() llmd.Options {
	return llmd.Options{Profile: st.Profile, PrefixCache: st.PrefixCache,
		RoomFilter: st.RoomFilter == nil || *st.RoomFilter,
		KVCeiling:  st.KVCeiling, KVScorer: st.KVScorer}
}

// readState loads persisted state, leaving v at its zero value when there is
// none. A corrupt or unreadable file is reported rather than read as "default
// state", which silently discarded an operator's llm-d settings.
func readState(path string, v any) error {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		return fmt.Errorf("%s is unreadable: %w", path, err)
	}
	return nil
}

// writeState replaces state atomically. Several HTTP handlers read, modify and
// rewrite these files; a direct WriteFile could be seen half-written by a
// concurrent reader, or leave a truncated file behind after a crash.
func writeState(path string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	if _, err := f.Write(b); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Chmod(tmp, 0o644); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

func (s *Server) SetLLMD(l *llmd.LLMD, statePath string) {
	s.llmd, s.llmdState = l, statePath
}

func LLMDEnabled(statePath string) (bool, string, llmd.Options) {
	var st llmdState
	_ = readState(statePath, &st)
	return st.Enabled && st.Model != "", st.Model, st.options()
}

func (s *Server) handleLLMDEnable(w http.ResponseWriter, r *http.Request) {
	if s.llmd == nil {
		writeError(w, http.StatusNotImplemented, "this node does not run llm-d")
		return
	}
	req := llmdState{PrefixCache: true}
	if err := decodeBody(w, r, &req, 1<<20); err != nil {
		writeError(w, http.StatusBadRequest, "parse request body: "+err.Error())
		return
	}
	if req.Model == "" {
		writeError(w, http.StatusBadRequest, "llm-d serves one model; name it")
		return
	}
	if req.Profile == "" {
		req.Profile = discovery.ProfileLoadAware
	}
	if _, ok := discovery.ProfileByName(req.Profile); !ok {
		writeError(w, http.StatusBadRequest, "unknown or unavailable profile "+req.Profile+" (see mfsh llmd profiles)")
		return
	}
	if req.KVCeiling < 0 || req.KVCeiling > 1 {
		writeError(w, http.StatusBadRequest, "kv_ceiling is a share of the KV cache between 0 and 1")
		return
	}
	if req.KVScorer < 0 || req.KVScorer > 10 {
		writeError(w, http.StatusBadRequest, "kv_scorer is a scoring weight between 0 and 10")
		return
	}
	if s.sup != nil {
		if m, err := s.sup.Catalog().Resolve(req.Model); err == nil {
			req.Model = m.Key
		}
	}
	if !s.llmd.Config().Installed() {
		writeError(w, http.StatusPreconditionFailed, "llm-d is not installed: run `mfsh llmd install`")
		return
	}
	// Start llm-d before anything is routed to it: until Envoy answers, the
	// model stays with the router.
	s.llmd.Start(req.Model, req.options())
	deadline := time.Now().Add(3 * time.Minute)
	for s.llmd.Status().State != "running" && time.Now().Before(deadline) {
		if st := s.llmd.Status(); st.State == "restarting" {
			s.llmd.Stop()
			writeError(w, http.StatusBadGateway, "llm-d did not start: "+st.Error)
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	// Falling out of the loop is not success. Persisting Enabled=true for an
	// llm-d that never reached "running" sent the model's route to an Envoy
	// that was not answering, and left that state to be restored on restart.
	if st := s.llmd.Status(); st.State != "running" {
		s.llmd.Stop()
		msg := "llm-d did not become ready within 3 minutes (state " + st.State + ")"
		if st.Error != "" {
			msg += ": " + st.Error
		}
		writeError(w, http.StatusGatewayTimeout, msg)
		return
	}
	req.Enabled = true
	if err := writeState(s.llmdState, req); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.handleLLMDStatus(w, r)
}

// handleLLMDInstall fetches llm-d's binaries as a durable operation, so the
// dashboard can show progress and a closed tab does not cancel it.
func (s *Server) handleLLMDInstall(w http.ResponseWriter, _ *http.Request) {
	if s.llmd == nil || s.sup == nil {
		writeError(w, http.StatusNotImplemented, "this node does not run llm-d")
		return
	}
	cfg := s.llmd.Config()
	if cfg.Installed() {
		writeError(w, http.StatusConflict, "llm-d is already installed")
		return
	}
	j := s.sup.Journal()
	op, created := j.Begin("llmd-install", "llm-d", "llmd-install")
	if created {
		go func() {
			last := time.Time{}
			err := cfg.Install(context.Background(), func(what string, done, total int64) {
				if time.Since(last) < time.Second && done != total {
					return
				}
				last = time.Now()
				j.Report(op.ID, "downloading "+what, float64(done)/float64(max(total, 1)))
			})
			if err != nil {
				j.Fail(op.ID, err)
				return
			}
			j.Report(op.ID, "installed", 1)
			j.Succeed(op.ID, "")
		}()
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"operation": op})
}

func (s *Server) handleLLMDProfiles(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"profiles": discovery.Profiles})
}

func (s *Server) handleLLMDDisable(w http.ResponseWriter, r *http.Request) {
	if s.llmd == nil {
		writeError(w, http.StatusNotImplemented, "this node does not run llm-d")
		return
	}
	if err := writeState(s.llmdState, llmdState{}); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	// Nothing to reroute: llmdTarget reads llm-d's live state per request, so
	// the moment it stops reporting "running" the router has the model back.
	s.llmd.Stop()
	s.handleLLMDStatus(w, r)
}

func (s *Server) handleLLMDStatus(w http.ResponseWriter, _ *http.Request) {
	if s.llmd == nil {
		writeJSON(w, http.StatusOK, llmd.Status{State: "disabled"})
		return
	}
	writeJSON(w, http.StatusOK, s.llmd.Status())
}

func (s *Server) llmdTarget(model string) (*url.URL, bool) {
	if s.llmd == nil || model == "" {
		return nil, false
	}
	st := s.llmd.Status()
	if st.State != "running" || st.Model != model {
		return nil, false
	}
	// Without endpoints Envoy returns 503; fall back to the mesh router.
	if len(st.Endpoints) == 0 {
		return nil, false
	}
	return &url.URL{Scheme: "http", Host: s.llmd.Config().Listen}, true
}
