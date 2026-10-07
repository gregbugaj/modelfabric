package server

import "net/http"

// handleDoctor runs uncached health checks. Reports include filesystem paths
// and process details, so peer access requires the same Tailscale owner.
func (s *Server) handleDoctor(w http.ResponseWriter, r *http.Request) {
	if s.Doctor == nil {
		writeError(w, http.StatusNotImplemented, "this node does not run health checks")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"checks": s.Doctor()})
}
