package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/gregbugaj/modelfabric/internal/devlog"
)

// The developer log over HTTP. Both routes are under /api/v1, so over the
// tailnet they answer only a device Tailscale reports as this node's owner,
// like the rest of management (see peer.go), and another node's dashboard
// reaches them through /api/v1/nodes/{node}/devlog.
//
// What is served is what the log holds: sizes, counts and the engines' own
// lines always, and request and response bodies only for entries written
// while body capture was on.

type devlogRecent struct {
	Node string `json:"node"`
	// Capture says whether bodies are being recorded now, so a reader can
	// tell "no body" from "capture is off".
	Capture bool           `json:"capture"`
	Entries []devlog.Entry `json:"entries"`
}

// handleDevlog serves the entries held, oldest first. ?limit bounds how many
// (default 500); ?level is the most detail wanted: error, warn, info (the
// default) or debug; ?after is the last sequence number the reader has.
func (s *Server) handleDevlog(w http.ResponseWriter, r *http.Request) {
	limit := 500
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			writeError(w, http.StatusBadRequest, "limit must be a positive number")
			return
		}
		limit = min(n, devlog.DefaultKeep)
	}
	level, ok := devlogLevel(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "level must be one of error, warn, info, debug")
		return
	}
	entries := s.dev.Recent(limit, level)
	// ?after=N leaves out what a polling reader already has. A dashboard
	// following several nodes polls the others this way and streams only one:
	// a browser allows six connections to one address, and a stream per node
	// used them all (see web/devlog.js).
	if v := r.URL.Query().Get("after"); v != "" {
		after, err := strconv.ParseUint(v, 10, 64)
		if err != nil {
			writeError(w, http.StatusBadRequest, "after must be a sequence number")
			return
		}
		kept := entries[:0:0]
		for _, e := range entries {
			if e.Seq > after {
				kept = append(kept, e)
			}
		}
		entries = kept
	}
	on, _, _, _ := s.traffic.settings()
	writeJSON(w, http.StatusOK, devlogRecent{Node: s.nodeName(), Capture: on, Entries: entries})
}

// handleDevlogStream serves entries as server-sent events as they are
// written. ?backlog=1 replays what is held first; ?level as for handleDevlog.
func (s *Server) handleDevlogStream(w http.ResponseWriter, r *http.Request) {
	level, ok := devlogLevel(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "level must be one of error, warn, info, debug")
		return
	}
	rc := http.NewResponseController(w)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	// At once, so an idle log still opens the stream for the reader.
	_ = rc.Flush()

	ch, backlog, cancel := s.dev.Subscribe()
	defer cancel()
	send := func(e devlog.Entry) bool {
		if !devlog.Shows(e.Level, level) {
			return true
		}
		b, _ := json.Marshal(e)
		if _, err := fmt.Fprintf(w, "data: %s\n\n", b); err != nil {
			return false
		}
		return rc.Flush() == nil
	}
	if r.URL.Query().Get("backlog") == "1" {
		for _, e := range backlog {
			if !send(e) {
				return
			}
		}
	}
	keepalive := time.NewTicker(15 * time.Second)
	defer keepalive.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case e := <-ch:
			if !send(e) {
				return
			}
		case <-keepalive.C:
			if _, err := fmt.Fprint(w, ": keepalive\n\n"); err != nil || rc.Flush() != nil {
				return
			}
		}
	}
}

func devlogLevel(r *http.Request) (string, bool) {
	switch v := r.URL.Query().Get("level"); v {
	case "":
		return devlog.Info, true
	case devlog.Error, devlog.Warn, devlog.Info, devlog.Debug:
		return v, true
	}
	return "", false
}

// nodeName is this node's name, "" for a server built without a mesh.
func (s *Server) nodeName() string {
	if s.m == nil {
		return ""
	}
	return s.m.State().Node
}
