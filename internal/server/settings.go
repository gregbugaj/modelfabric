package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/gregbugaj/modelfabric/internal/runtime"
	"github.com/gregbugaj/modelfabric/internal/supervisor"
)

// Per-model defaults and inference presets.
//
//	GET    /api/v1/model-defaults?model=<key>   the model's saved defaults
//	PUT    /api/v1/model-defaults?model=<key>   replace them ({preset, settings})
//	DELETE /api/v1/model-defaults?model=<key>   clear them
//	GET    /api/v1/presets                      every preset
//	PUT    /api/v1/presets/{name}               save one ({description, settings})
//	DELETE /api/v1/presets/{name}
//	POST   /api/v1/presets/import?name=<name>   body: an LM Studio preset file
//	GET    /api/v1/settings/fields              every setting's name, for forms
//	GET    /api/v1/vision-defaults              this node's settings for image models
//	PUT    /api/v1/vision-defaults              replace them (a bare Settings object)

func (s *Server) registerSettings(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/model-defaults", s.handleGetDefaults)
	mux.HandleFunc("PUT /api/v1/model-defaults", s.handlePutDefaults)
	mux.HandleFunc("DELETE /api/v1/model-defaults", s.handleDeleteDefaults)
	mux.HandleFunc("GET /api/v1/vision-defaults", s.handleGetVisionDefaults)
	mux.HandleFunc("PUT /api/v1/vision-defaults", s.handlePutVisionDefaults)
	mux.HandleFunc("GET /api/v1/presets", s.handlePresets)
	mux.HandleFunc("PUT /api/v1/presets/{name}", s.handlePutPreset)
	mux.HandleFunc("DELETE /api/v1/presets/{name}", s.handleDeletePreset)
	mux.HandleFunc("POST /api/v1/presets/import", s.handleImportPreset)
	mux.HandleFunc("GET /api/v1/settings/fields", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"fields": runtime.SettingsFields(), "spec_modes": runtime.SpecModes,
			"cache_types": runtime.CacheTypes, "reasoning_modes": runtime.ReasoningModes,
		})
	})
}

// modelKey resolves the ?model= argument through the catalog, so a short
// name and the full key address the same defaults.
func (s *Server) modelKey(w http.ResponseWriter, r *http.Request) (string, bool) {
	if !s.requireSupervisor(w) {
		return "", false
	}
	ref := r.URL.Query().Get("model")
	if ref == "" {
		writeError(w, http.StatusBadRequest, "name the model: ?model=<key>")
		return "", false
	}
	m, err := s.sup.Catalog().Resolve(ref)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return "", false
	}
	return m.Key, true
}

func (s *Server) handleGetDefaults(w http.ResponseWriter, r *http.Request) {
	key, ok := s.modelKey(w, r)
	if !ok {
		return
	}
	d, err := s.sup.Defaults(key)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"model": key, "defaults": d})
}

func (s *Server) handlePutDefaults(w http.ResponseWriter, r *http.Request) {
	key, ok := s.modelKey(w, r)
	if !ok {
		return
	}
	var d supervisor.ModelDefaults
	if err := decodeStrict(r, &d); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.sup.SetDefaults(key, d); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	saved, err := s.sup.Defaults(key)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"model": key, "defaults": saved})
}

func (s *Server) handleDeleteDefaults(w http.ResponseWriter, r *http.Request) {
	key, ok := s.modelKey(w, r)
	if !ok {
		return
	}
	if err := s.sup.SetDefaults(key, supervisor.ModelDefaults{}); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"model": key, "defaults": supervisor.ModelDefaults{}})
}

func (s *Server) handlePresets(w http.ResponseWriter, _ *http.Request) {
	if !s.requireSupervisor(w) {
		return
	}
	presets, err := s.sup.Presets()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if presets == nil {
		presets = []supervisor.Preset{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"presets": presets})
}

func (s *Server) handlePutPreset(w http.ResponseWriter, r *http.Request) {
	if !s.requireSupervisor(w) {
		return
	}
	var p supervisor.Preset
	if err := decodeStrict(r, &p); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	p.Name = r.PathValue("name")
	if err := s.sup.SavePreset(p); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (s *Server) handleDeletePreset(w http.ResponseWriter, r *http.Request) {
	if !s.requireSupervisor(w) {
		return
	}
	if err := s.sup.DeletePreset(r.PathValue("name")); err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleImportPreset(w http.ResponseWriter, r *http.Request) {
	if !s.requireSupervisor(w) {
		return
	}
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	p, skipped, err := supervisor.ImportLMStudioPreset(data, r.URL.Query().Get("name"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.sup.SavePreset(p); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"preset": p, "skipped": skipped})
}

// decodeStrict rejects unknown fields and requires exactly one JSON document,
// preventing trailing values or junk from being ignored.
func decodeStrict(r *http.Request, v any) error {
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		return fmt.Errorf("the request body must be one JSON object")
	}
	return nil
}

func (s *Server) handleGetVisionDefaults(w http.ResponseWriter, _ *http.Request) {
	if !s.requireSupervisor(w) {
		return
	}
	v, err := s.sup.VisionDefaults()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"node": s.m.State().Node, "settings": v,
		// What the node would use if this were cleared, so the dashboard can
		// show "back to the default" without knowing what the default is.
		"default": supervisor.DefaultVisionSettings(),
	})
}

func (s *Server) handlePutVisionDefaults(w http.ResponseWriter, r *http.Request) {
	if !s.requireSupervisor(w) {
		return
	}
	var v runtime.Settings
	if err := decodeStrict(r, &v); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.sup.SetVisionDefaults(v); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.handleGetVisionDefaults(w, r)
}
