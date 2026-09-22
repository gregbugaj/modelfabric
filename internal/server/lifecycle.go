package server

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/gregbugaj/modelfabric/internal/catalog"
	"github.com/gregbugaj/modelfabric/internal/ops"
	"github.com/gregbugaj/modelfabric/internal/runtime"
	"github.com/gregbugaj/modelfabric/internal/supervisor"
)

// The model-management surface:
//
//	GET  /api/v1/models         prepared models and loaded_instances
//	POST /api/v1/models/load    {"model", "context_length", "echo_load_config"}
//	POST /api/v1/models/unload  {"instance_id"}
//
// Deliberately not OpenAI's shape: "The list uses `models`, each model's `key`,
// and `loaded_instances` containing `id` and applied `config`; it is not
// OpenAI's `data` list."

// loadWaitTimeout bounds how long the load route blocks. The operation stays
// observable after a timeout or disconnect, so callers can reattach instead
// of orphaning a load or starting it twice.
const loadWaitTimeout = 10 * time.Minute

type modelEntry struct {
	catalog.Model
	// File and Modified describe the weights on disk, as LM Studio's model
	// list does.
	File     string     `json:"file,omitempty"`
	Modified *time.Time `json:"modified,omitempty"`
	// Spec is the publisher's recommended settings (model.yaml): what a
	// setting left empty inherits.
	Spec            *catalog.ModelSpec `json:"spec,omitempty"`
	LoadedInstances []loadedInstance   `json:"loaded_instances"`
}

type loadedInstance struct {
	ID     string          `json:"id"`
	Config runtime.Applied `json:"config"`
	// Beyond LM Studio's shape: how the instance got here and when its idle
	// TTL, if any, will unload it.
	Origin     string     `json:"origin,omitempty"`
	TTLSeconds int        `json:"ttl,omitempty"`
	LastUsed   *time.Time `json:"last_used,omitempty"`
	PID        int        `json:"pid,omitempty"`
	Port       int        `json:"port,omitempty"`
	// Shim is the proxy in front of the engine, and what it does there.
	Shim *supervisor.ShimInfo `json:"shim,omitempty"`
}

type loadResponse struct {
	Instance loadedInstance `json:"instance"`
	// Note explains a successful load whose instance is already gone.
	Note            string           `json:"note,omitempty"`
	Model           string           `json:"model"`
	ElapsedMS       int64            `json:"elapsed_ms"`
	EffectiveConfig *runtime.Applied `json:"effective_config,omitempty"`
	Operation       *ops.Operation   `json:"operation,omitempty"`
}

func (s *Server) registerLifecycle(mux *http.ServeMux) {
	// Registered even without a supervisor. Leaving them unregistered let the
	// UI's catch-all serve index.html for /api/v1/models, so clients got HTML
	// and a bewildering "invalid character '<'" JSON error.
	mux.HandleFunc("GET /api/v1/models", s.handleListPrepared)
	mux.HandleFunc("POST /api/v1/models/load", s.handleLoad)
	mux.HandleFunc("POST /api/v1/models/unload", s.handleUnload)
	mux.HandleFunc("GET /api/v1/operations", s.handleOperations)
	mux.HandleFunc("POST /api/v1/models/repin", s.handleRepin)
	mux.HandleFunc("POST /api/v1/models/recover", s.handleRecover)
	mux.HandleFunc("GET /api/v1/runtimes", s.handleRuntimes)
	mux.HandleFunc("POST /api/v1/models/rescan", s.handleRescan)
	mux.HandleFunc("POST /api/v1/runtimes/select", s.handleSelectRuntime)
	mux.HandleFunc("POST /api/v1/runtimes/rescan", s.handleRuntimeRescan)
	mux.HandleFunc("GET /api/v1/runtimes/available", s.handleRuntimesAvailable)
	mux.HandleFunc("POST /api/v1/runtimes/get", s.handleRuntimeGet)
	mux.HandleFunc("POST /api/v1/runtimes/remove", s.handleRuntimeRemove)
	s.registerSettings(mux)
	s.registerTune(mux)
	s.registerBench(mux)
	s.registerBenchCluster(mux)
	mux.HandleFunc("GET /api/v1/llmd", s.handleLLMDStatus)
	// The front door's own shape, which the Overview reads.
	mux.HandleFunc("GET /api/v1/front", s.handleFront)
	mux.HandleFunc("POST /api/v1/llmd/enable", s.handleLLMDEnable)
	mux.HandleFunc("POST /api/v1/llmd/disable", s.handleLLMDDisable)
	mux.HandleFunc("GET /api/v1/llmd/profiles", s.handleLLMDProfiles)
	mux.HandleFunc("POST /api/v1/llmd/install", s.handleLLMDInstall)
	mux.HandleFunc("GET /z/preferred", s.handlePreferred)
	mux.HandleFunc("POST /z/preferred", s.handlePreferred)
}

// requireSupervisor answers with an actionable message on a node that only
// routes, instead of failing somewhere less obvious.
func (s *Server) requireSupervisor(w http.ResponseWriter) bool {
	if s.sup != nil {
		return true
	}
	reason := s.supErr
	if reason == "" {
		reason = "no models_root is configured"
	}
	writeError(w, http.StatusServiceUnavailable,
		"this node does not manage models: "+reason+
			". Fix it in your config (default ~/.config/modelfabric/config.json), then run `mfsh down && mfsh up`.")
	return false
}

func (s *Server) handleListPrepared(w http.ResponseWriter, _ *http.Request) {
	if !s.requireSupervisor(w) {
		return
	}
	loaded := map[string][]loadedInstance{}
	for _, inst := range s.sup.Instances() {
		li := loadedInstance{ID: inst.ID, Config: inst.Config, Origin: inst.Origin, TTLSeconds: inst.TTLSeconds,
			PID: inst.PID, Port: inst.Port, Shim: inst.Shim()}
		if !inst.LastUsed.IsZero() {
			t := inst.LastUsed
			li.LastUsed = &t
		}
		loaded[inst.Model] = append(loaded[inst.Model], li)
	}

	models := s.sup.Catalog().Models()
	entries := make([]modelEntry, 0, len(models))
	for _, m := range models {
		instances := loaded[m.Key]
		if instances == nil {
			// Always an array, never null: clients iterate this directly.
			instances = []loadedInstance{}
		}
		e := modelEntry{Model: m, Spec: m.Spec, LoadedInstances: instances}
		if m.Path != "" {
			e.File = filepath.Base(m.Path)
			if fi, err := os.Stat(m.Path); err == nil {
				t := fi.ModTime().UTC()
				e.Modified = &t
			}
		}
		entries = append(entries, e)
	}
	writeJSON(w, http.StatusOK, map[string]any{"models": entries})
}

// waitForLoad waits for a load to settle, giving up early if the caller
// disconnects. The operation itself is unaffected — it is journalled and
// pollable — but the handler used to hold its goroutine and connection for the
// full ten minutes even after nobody was listening.
func (s *Server) waitForLoad(r *http.Request, opID string) (*ops.Operation, bool) {
	type result struct {
		op   *ops.Operation
		done bool
	}
	ch := make(chan result, 1)
	go func() {
		settled, done := s.sup.Journal().Wait(opID, loadWaitTimeout)
		ch <- result{settled, done}
	}()
	select {
	case v := <-ch:
		return v.op, v.done
	case <-r.Context().Done():
		return nil, false
	}
}

func (s *Server) handleLoad(w http.ResponseWriter, r *http.Request) {
	if !s.requireSupervisor(w) {
		return
	}
	var req supervisor.LoadRequest
	if err := decodeBody(w, r, &req, 1<<20); err != nil {
		writeError(w, http.StatusBadRequest, "parse request body: "+err.Error())
		return
	}
	if req.Model == "" {
		writeError(w, http.StatusBadRequest, `request is missing the "model" field`)
		return
	}

	op, _, err := s.sup.Load(req)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}

	// Wait for readiness on this route only. The asynchronous operation stays
	// available regardless of what happens to this connection.
	settled, done := s.waitForLoad(r, op.ID)
	if settled == nil {
		return // the client went away; the load carries on without it
	}
	if !done {
		// Still running: hand back the operation so the caller can poll.
		writeJSON(w, http.StatusAccepted, map[string]any{
			"operation": settled,
			"message":   "load is still running; poll /api/v1/operations",
		})
		return
	}
	if settled.State == ops.StateFailed {
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"error":     map[string]any{"message": settled.Error, "type": "modelfabric_load_failed"},
			"operation": settled,
		})
		return
	}

	resp := loadResponse{
		Model:     settled.Model,
		ElapsedMS: settled.ElapsedMS,
		Operation: settled,
	}
	found := false
	for _, inst := range s.sup.Instances() {
		if inst.ID == settled.InstanceID {
			found = true
			resp.Instance = loadedInstance{ID: inst.ID, Config: inst.Config}
			// "Only applied settings appear in effective configuration."
			if req.EchoConfig {
				cfg := inst.Config
				resp.EffectiveConfig = &cfg
			}
			break
		}
	}
	if !found {
		// The load succeeded but the instance is already gone — unloaded, or
		// it exited between the journal settling and this snapshot. An empty
		// instance object read as "loaded, with no configuration".
		resp.Instance = loadedInstance{ID: settled.InstanceID}
		resp.Note = "the instance is no longer running; it was unloaded or exited right after loading"
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleUnload(w http.ResponseWriter, r *http.Request) {
	if !s.requireSupervisor(w) {
		return
	}
	var req struct {
		InstanceID string `json:"instance_id"`
	}
	if err := decodeBody(w, r, &req, 1<<20); err != nil {
		writeError(w, http.StatusBadRequest, "parse request body: "+err.Error())
		return
	}
	if req.InstanceID == "" {
		writeError(w, http.StatusBadRequest, `request is missing the "instance_id" field`)
		return
	}

	op, err := s.sup.Unload(req.InstanceID)
	if err != nil {
		// A stale instance id is the caller's mistake, not a server fault, and
		// it must never stop that instance's replacement.
		if op == nil {
			writeError(w, http.StatusNotFound, err.Error())
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"error":     map[string]any{"message": err.Error(), "type": "modelfabric_unload_failed"},
			"operation": op,
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"instance_id": req.InstanceID,
		"operation":   op,
	})
}

func (s *Server) handleOperations(w http.ResponseWriter, r *http.Request) {
	if !s.requireSupervisor(w) {
		return
	}
	if id := r.URL.Query().Get("id"); id != "" {
		op, ok := s.sup.Journal().Get(id)
		if !ok {
			writeError(w, http.StatusNotFound, "no operation "+id)
			return
		}
		writeJSON(w, http.StatusOK, op)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"operations": s.sup.Journal().List()})
}

// handleRepin accepts a model's current on-disk contents as its pinned state.
// Explicit by design: it discards the previous integrity guarantee.
func (s *Server) handleRepin(w http.ResponseWriter, r *http.Request) {
	if !s.requireSupervisor(w) {
		return
	}
	var req struct {
		Model string `json:"model"`
	}
	if err := decodeBody(w, r, &req, 1<<20); err != nil {
		writeError(w, http.StatusBadRequest, "parse request body: "+err.Error())
		return
	}
	key, err := s.sup.Repin(req.Model)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"model": key, "pinned": true})
}

// handleRecover settles an unresolved launch window. Explicit by design: the
// window exists precisely because ModelFabric cannot prove whether an orphan is
// running, and only an operator can confirm that.
func (s *Server) handleRecover(w http.ResponseWriter, r *http.Request) {
	if !s.requireSupervisor(w) {
		return
	}
	var req struct {
		Model string `json:"model"`
	}
	if err := decodeBody(w, r, &req, 1<<20); err != nil {
		writeError(w, http.StatusBadRequest, "parse request body: "+err.Error())
		return
	}
	key, err := s.sup.ClearUnresolved(req.Model)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"model": key, "cleared": true})
}

type runtimeView struct {
	Name        string   `json:"name"`
	Engine      string   `json:"engine"`
	Origin      string   `json:"origin"`
	Version     string   `json:"version"`
	Backend     string   `json:"backend"`
	Protocol    string   `json:"protocol"`
	Entrypoint  string   `json:"entrypoint"`
	Pin         string   `json:"pin"`
	Digest      string   `json:"digest,omitempty"`
	Domains     []string `json:"domains,omitempty"`
	VendorDirs  []string `json:"vendor_dirs,omitempty"`
	Default     bool     `json:"default"`
	Fit         string   `json:"fit,omitempty"`
	Reasons     []string `json:"reasons,omitempty"`
	LlamaBuild  int      `json:"llama_build,omitempty"`
	DisplayName string   `json:"display_name,omitempty"`
	PackageDir  string   `json:"package_dir,omitempty"`
	// Managed is true for runtimes ModelFabric installed itself, the only ones
	// `mfsh runtime remove` will touch.
	Managed bool `json:"managed,omitempty"`
	// InUse lists loaded instances running on this runtime.
	InUse []string `json:"in_use,omitempty"`
}

func (s *Server) handleRuntimes(w http.ResponseWriter, _ *http.Request) {
	if !s.requireSupervisor(w) {
		return
	}
	reg := s.sup.Runtimes()
	if reg == nil {
		writeJSON(w, http.StatusOK, map[string]any{"runtimes": []runtimeView{}})
		return
	}
	def, _ := reg.Default()
	inUse := map[string][]string{}
	for _, inst := range s.sup.Instances() {
		inUse[inst.Runtime] = append(inUse[inst.Runtime], inst.ID)
	}
	out := make([]runtimeView, 0, reg.Len())
	for _, d := range reg.All() {
		v := runtimeView{
			Name: d.Name, Engine: d.Engine, Origin: string(d.Origin),
			Version: d.Version, Backend: d.Backend, Protocol: string(d.Protocol),
			Entrypoint: d.Path(), Pin: string(d.Pin()), Digest: d.Digest,
			Domains: d.Domains(), VendorDirs: d.VendorDirs(),
			Default:    def != nil && def.Name == d.Name,
			LlamaBuild: d.LlamaBuild, DisplayName: d.DisplayName, PackageDir: d.PackageDir(),
			Managed: d.Provenance() != nil, InUse: inUse[d.Name],
		}
		if hw := reg.Hardware(); hw != nil {
			fit, reasons := d.Check(*hw)
			v.Fit, v.Reasons = string(fit), reasons
		}
		out = append(out, v)
	}
	// "auto" follows new builds as they are installed; a name is a pin.
	selection := "auto"
	if p := reg.Pinned(); p != "" {
		selection = p
	}
	resp := map[string]any{"runtimes": out, "selection": selection, "can_install": s.runtimesRoot != ""}
	if hw := reg.Hardware(); hw != nil {
		resp["hardware"] = hw
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleSelectRuntime sets the default runtime and persists it.
func (s *Server) handleSelectRuntime(w http.ResponseWriter, r *http.Request) {
	if !s.requireSupervisor(w) {
		return
	}
	var req struct {
		Name string `json:"name"`
	}
	if err := decodeBody(w, r, &req, 1<<20); err != nil {
		writeError(w, http.StatusBadRequest, "parse request body: "+err.Error())
		return
	}
	reg := s.sup.Runtimes()
	if err := reg.SetDefault(req.Name); err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	// A selection that is only in memory is lost on restart, so a write that
	// fails is reported rather than answered with success.
	if s.runtimeStore != "" {
		var err error
		if req.Name == "" {
			if err = os.Remove(s.runtimeStore); os.IsNotExist(err) {
				err = nil
			}
		} else {
			err = writeState(s.runtimeStore, map[string]string{"runtime": req.Name})
		}
		if err != nil {
			writeError(w, http.StatusInternalServerError,
				"the runtime is selected for this run but could not be saved: "+err.Error())
			return
		}
	}
	d, _ := reg.Default()
	name := ""
	if d != nil {
		name = d.Name
	}
	writeJSON(w, http.StatusOK, map[string]any{"selected": req.Name, "default": name})
}

// LoadSelectedRuntime reads a persisted runtime choice.
// SetRuntimeReloader supplies how to rediscover installed runtimes, for
// POST /api/v1/runtimes/rescan.
func (s *Server) SetRuntimeReloader(fn func() error) { s.reloadRuntimes = fn }

// handleRuntimeRescan picks up runtimes installed or removed since start.
func (s *Server) handleRuntimeRescan(w http.ResponseWriter, _ *http.Request) {
	if !s.requireSupervisor(w) {
		return
	}
	if s.reloadRuntimes == nil {
		writeError(w, http.StatusNotImplemented, "this node cannot rescan runtimes")
		return
	}
	if err := s.reloadRuntimes(); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	// A persisted selection naming a runtime that is gone would make every
	// later start fail to pick a default; forget it and select automatically.
	if s.runtimeStore != "" {
		if sel := LoadSelectedRuntime(s.runtimeStore); sel != "" {
			if _, ok := s.sup.Runtimes().Lookup(sel); !ok {
				_ = os.Remove(s.runtimeStore)
			}
		}
	}
	s.handleRuntimes(w, nil)
}

func LoadSelectedRuntime(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var v struct {
		Runtime string `json:"runtime"`
	}
	if json.Unmarshal(b, &v) != nil {
		return ""
	}
	return v.Runtime
}

// handlePreferred reports or sets the preferred node for model resolution.
//
// Mirrors LM Link's preferred device: "When the same model is available on
// multiple devices in the link, LM Link uses the preferred device to load and
// use the model." Fallback is not specified there, so ModelFabric degrades to its
// normal load-based choice rather than failing — a preference should never
// make a request impossible.
func (s *Server) handlePreferred(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		writeJSON(w, http.StatusOK, map[string]any{"preferred_node": s.m.Preferred()})
		return
	}
	var req struct {
		Node string `json:"node"`
	}
	if err := decodeBody(w, r, &req, 1<<20); err != nil {
		writeError(w, http.StatusBadRequest, "parse request body: "+err.Error())
		return
	}
	if req.Node != "" {
		known := req.Node == s.m.State().Node
		for _, p := range s.m.Peers() {
			if p.Node == req.Node {
				known = true
			}
		}
		if !known {
			writeError(w, http.StatusNotFound,
				"no node named "+req.Node+" in the mesh; run `mfsh status` to see the nodes")
			return
		}
	}
	s.prefMu.Lock()
	defer s.prefMu.Unlock()
	if err := s.persistPreferred(req.Node); err != nil {
		s.log.Warn("could not persist preferred node", "err", err)
		writeError(w, http.StatusInternalServerError, "could not save preferred node: "+err.Error())
		return
	}
	s.m.SetPreferred(req.Node)
	writeJSON(w, http.StatusOK, map[string]any{"preferred_node": req.Node})
}

// SetPreferredStore sets where the preferred node is persisted, so the choice
// outlives a restart without editing the operator's config file.
func (s *Server) SetPreferredStore(path string) { s.prefStore = path }

func (s *Server) persistPreferred(node string) error {
	if s.prefStore == "" {
		return nil
	}
	if node == "" {
		err := os.Remove(s.prefStore)
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	return writeState(s.prefStore, map[string]string{"preferred_node": node})
}

// LoadPreferred reads a persisted preference, returning "" when none is set.
func LoadPreferred(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var v struct {
		Node string `json:"preferred_node"`
	}
	if json.Unmarshal(b, &v) != nil {
		return ""
	}
	return v.Node
}

// handleRescan re-indexes the models directories, so a model that `mfsh get`
// just downloaded is loadable without restarting the node.
func (s *Server) handleRescan(w http.ResponseWriter, _ *http.Request) {
	if !s.requireSupervisor(w) {
		return
	}
	if err := s.sup.Rescan(); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"models": len(s.sup.Catalog().Models())})
}
