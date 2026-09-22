package server

import "net/http"

// handleDoctor runs this node's health checks and returns them.
//
// It is a management route, so over the tailnet it reaches only a same-owner
// device: the report names filesystem paths, process names and whatever else
// holds the GPU. That is the point of it — the case it exists for is asking a
// *peer* what is wrong with it, which the CLI cannot do at all — but it is
// not something every tailnet member should read.
//
// The checks are run per request rather than cached. A diagnosis is about the
// state right now, and a stale one is worse than none: the thing being
// diagnosed is usually changing while you look at it.
func (s *Server) handleDoctor(w http.ResponseWriter, r *http.Request) {
	if s.Doctor == nil {
		writeError(w, http.StatusNotImplemented, "this node does not run health checks")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"checks": s.Doctor()})
}
