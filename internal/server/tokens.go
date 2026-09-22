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

// tapTokens wraps w so a watcher sees the reply, and returns the function that
// closes out the request. Both are no-ops when nobody is watching, which costs
// one atomic read.
//
// Every listener that serves inference has to call this. The tap first went
// only into FrontHandler, on the reasoning that it is where the proxied path
// and ModelFabric's own router meet — true, and still blind to `public_listen`,
// which is its own handler and is how every caller from outside the machine
// arrives.
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

// handleTokenStream serves live output as server-sent events.
//
// SSE rather than a WebSocket, deliberately. Measured on a real run, 98% of
// this stream is the JSON envelope repeated per token and 2% is the text — so
// the win is in coalescing deltas, not in the transport, whose framing differs
// by a handful of bytes. SSE also needs no dependency: Go's standard library
// has no WebSocket server, and ModelFabric ships as one binary with none.
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
