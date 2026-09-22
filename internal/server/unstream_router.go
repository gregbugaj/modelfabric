package server

import (
	"bytes"
	"io"
	"net/http"
	"strconv"
)

// Asking the engine for a stream even when the caller did not, on ModelFabric's own
// router path.
//
// Otherwise a client that sends "stream": false (aider does, and so does anything built
// on a plain OpenAI SDK call) produces nothing to watch: the engine writes one
// JSON body at the end, and a live view of the reply is a live view of nothing
// happening for two minutes.
//
// The order of the wrapping is the whole trick. The router writes an event
// stream, the token tap reads it as it passes, and only then is it reassembled
// into the single body the caller asked for:
//
//	router ──SSE──► tap (publishes tokens live) ──► unstreamer ──JSON──► caller
//
// Wrapped the other way round the tap would see one finished JSON body, which is
// exactly the nothing this exists to fix.

// maxAssemble bounds what is held to reassemble one answer. Measured on a real
// run, an event stream is about 98% envelope: 588,697 bytes carried 11,727 bytes
// of text. Even a very long reply stays well under this, and a stream that
// somehow does not is passed through untouched rather than buffered without
// limit — a runaway generation must not become a runaway allocation.
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
	// raw is set when reassembly is impossible — an oversized stream, or a
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
		// Give up on reassembly rather than grow without bound: flush what was
		// held and stream the rest. The caller asked for one body and gets a
		// stream, which is worse than promised but better than a node that
		// falls over.
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

// finish assembles what was collected and writes the caller's single body.
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

// unstreamed upgrades a non-streaming request so the reply can be watched as it
// is written, and returns the writer to hand the router plus a finish func.
//
// When there is nothing to upgrade — a streaming request, a path that does not
// generate, a body that cannot be parsed — the request and writer are returned
// untouched, so the ordinary path costs one function call.
func (s *Server) unstreamed(w http.ResponseWriter, r *http.Request) (http.ResponseWriter, func()) {
	// Only worth doing when somebody is watching. The upgrade is cheap but not
	// free: it buffers the whole answer, which changes when the caller's first
	// byte arrives, and a path that behaves differently when observed is one
	// whose bugs appear only when observed — so this is the one place that
	// trade is made deliberately.
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
