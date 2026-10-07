package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/gregbugaj/modelfabric/internal/router"
)

// The node's own view of what it is generating. The tap itself lives in
// internal/tokentap, because the engine shim needs the same thing on whichever
// machine actually runs the model.

// tapTokens wraps responses for watchers and returns a completion callback.
// With no watchers both are no-ops costing one atomic read. Every inference
// listener, including public_listen, must call it.
func (s *Server) tapTokens(w http.ResponseWriter, r *http.Request) (http.ResponseWriter, func()) {
	if !s.tokens.Active() || !inferencePath(r) {
		return w, func() {}
	}
	trace := router.TraceOf(r).TraceID
	if trace == "" {
		trace = r.Header.Get(router.TraceHeader)
	}
	model := peekRequest(r).Model
	return s.tokens.Wrap(w, trace, model)
}

func (s *Server) handleTokenStream(w http.ResponseWriter, r *http.Request) {
	rc := http.NewResponseController(w)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	// Flush the header immediately, or a client connecting to an idle node
	// cannot tell a working stream from a hung one until the first token.
	_ = rc.Flush()

	ch, cancel := s.tokens.Subscribe(256)
	defer cancel()

	keepalive := time.NewTicker(15 * time.Second)
	defer keepalive.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case e := <-ch:
			b, _ := json.Marshal(e)
			if _, err := fmt.Fprintf(w, "data: %s\n\n", b); err != nil {
				return
			}
			if rc.Flush() != nil {
				return
			}
		case <-keepalive.C:
			if _, err := fmt.Fprint(w, ": keepalive\n\n"); err != nil {
				return
			}
			if rc.Flush() != nil {
				return
			}
		}
	}
}
