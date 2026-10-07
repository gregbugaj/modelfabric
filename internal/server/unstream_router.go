package server

import (
	"bytes"
	"io"
	"net/http"
	"strconv"
)

// The router path upgrades non-streaming requests for live token capture.
// The tap must read SSE before reassembly into JSON:
//
// router -> tap -> unstreamer -> caller

// maxAssemble bounds response buffering. Oversized streams pass through
// without reassembly to avoid unbounded allocation.
const maxAssemble = 32 << 20

// unstreamWriter collects an event stream and answers with one JSON body.
//
// Headers are held back until the end: the caller must see
// "Content-Type: application/json" and a Content-Length describing the
// assembled body, not the text/event-stream the engine sent.
type unstreamWriter struct {
	http.ResponseWriter
	buf    bytes.Buffer
	status int
	// raw is set when reassembly is impossible - an oversized stream, or a
	// non-2xx whose body is the engine's own error to deliver verbatim. From
	// then on bytes go straight to the caller.
	raw bool
}

func (u *unstreamWriter) WriteHeader(status int) {
	u.status = status
	// An error is the engine's to report in its own words and shape. Only a
	// success is an event stream worth reassembling.
	if status < 200 || status > 299 {
		u.raw = true
		u.ResponseWriter.WriteHeader(status)
	}
}

func (u *unstreamWriter) Write(p []byte) (int, error) {
	if u.raw {
		return u.ResponseWriter.Write(p)
	}
	if u.buf.Len()+len(p) > maxAssemble {
		u.raw = true
		u.ResponseWriter.Header().Set("Content-Type", "text/event-stream")
		u.ResponseWriter.WriteHeader(u.statusOr(http.StatusOK))
		if _, err := u.ResponseWriter.Write(u.buf.Bytes()); err != nil {
			return 0, err
		}
		u.buf.Reset()
		return u.ResponseWriter.Write(p)
	}
	return u.buf.Write(p)
}

// Flush is a no-op while reassembling: there is nothing to flush to a caller
// that is waiting for one body, and the router flushes every chunk. Without
// this, http.NewResponseController finds the wrapped writer's Flush and pushes
// out headers before the Content-Type has been corrected.
func (u *unstreamWriter) Flush() {
	if u.raw {
		http.NewResponseController(u.ResponseWriter).Flush()
	}
}

func (u *unstreamWriter) statusOr(def int) int {
	if u.status != 0 {
		return u.status
	}
	return def
}

func (u *unstreamWriter) finish() {
	if u.raw {
		return
	}
	if u.buf.Len() == 0 {
		// Nothing arrived: the router already failed and wrote its own error, or
		// the caller went away. Either way there is nothing to assemble.
		return
	}
	body, err := assembleStream(bytes.NewReader(u.buf.Bytes()), nil)
	if err != nil {
		// Unassemblable: hand back what the engine actually sent rather than an
		// invented answer or an empty 200.
		u.ResponseWriter.Header().Set("Content-Type", "text/event-stream")
		u.ResponseWriter.WriteHeader(u.statusOr(http.StatusOK))
		_, _ = u.ResponseWriter.Write(u.buf.Bytes())
		return
	}
	h := u.ResponseWriter.Header()
	h.Set("Content-Type", "application/json")
	h.Set("Content-Length", strconv.Itoa(len(body)))
	// The engine's streaming framing does not describe this body.
	h.Del("Transfer-Encoding")
	u.ResponseWriter.WriteHeader(u.statusOr(http.StatusOK))
	_, _ = u.ResponseWriter.Write(body)
}

// unstreamed upgrades eligible non-streaming requests and returns a writer
// and completion function. Ineligible requests and writers pass through unchanged.
func (s *Server) unstreamed(w http.ResponseWriter, r *http.Request) (http.ResponseWriter, func()) {
	// Upgrade only while watched to avoid buffering otherwise. This changes
	// when the caller receives its first byte.
	if !s.tokens.Active() {
		return w, func() {}
	}
	// A whole body: the upgrade rewrites it, and a prefix is not a JSON
	// document to rewrite.
	peek := peekRequest(r)
	if peek.Model == "" || !peek.Whole {
		return w, func() {}
	}
	body := peek.Body
	upgraded, ok := unstreamable(r.URL.Path, body)
	if !ok {
		return w, func() {}
	}
	r.Body = io.NopCloser(bytes.NewReader(upgraded))
	r.ContentLength = int64(len(upgraded))
	// Deleted rather than corrected: the transport sets it from ContentLength,
	// and a stale header here described the body before the upgrade.
	r.Header.Del("Content-Length")
	u := &unstreamWriter{ResponseWriter: w}
	return u, u.finish
}
