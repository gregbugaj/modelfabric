package server

import (
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
)

// Browsers, and the pages in them.
//
// "Loopback-only" keeps other machines out; it does not keep out a web page,
// because the page runs in a browser on this machine. Any site could POST to
// 127.0.0.1:1234/api/v1/... with a text/plain body — a "simple" request,
// which browsers send without asking first — and the management API, which
// decodes JSON whatever the content type, did what it said. Measured: a page
// from https://evil.example created a token named "planted-by-a-website".
// The same request to /v1 ran a model, and llama-server echoes the caller's
// origin with Allow-Credentials, so the page could read the reply too.
//
// Two checks close it, for requests that come from a browser at all (they
// carry Origin or Sec-Fetch-Site; curl, the CLI and peers send neither):
//
//   - A request that changes something must come from this node's own pages,
//     or, for inference only, from an origin the operator allowed (CORS).
//   - Anything but inference must be addressed to this machine by a name it
//     answers to. Without that, DNS rebinding — evil.example re-pointed at
//     127.0.0.1 — makes the attacker's page same-origin, and Origin matches.

// fromBrowser reports a request a browser made: every current browser sends
// Sec-Fetch-Site, and Origin on anything cross-origin or unsafe.
func fromBrowser(r *http.Request) bool {
	return r.Header.Get("Origin") != "" || r.Header.Get("Sec-Fetch-Site") != ""
}

// crossSite reports a browser request made by a page from another origin.
func crossSite(r *http.Request) bool {
	o := r.Header.Get("Origin")
	if o == "" {
		// No Origin on a request a browser made means a same-origin GET,
		// unless Fetch Metadata says otherwise.
		return r.Header.Get("Sec-Fetch-Site") == "cross-site"
	}
	u, err := url.Parse(o)
	if err != nil || u.Host == "" {
		return true // "null", a sandboxed frame, or something unparseable
	}
	return !strings.EqualFold(u.Host, r.Host)
}

func unsafeMethod(m string) bool {
	return m != http.MethodGet && m != http.MethodHead && m != http.MethodOptions
}

// apiPath is the OpenAI- and LM Studio-shaped surface apps call: what CORS
// may open, and the only thing a web app on another origin has any business
// reaching.
func apiPath(r *http.Request) bool {
	// /api/v1/chat is the one app route among the management ones.
	return strings.HasPrefix(r.URL.Path, "/v1/") || strings.HasPrefix(r.URL.Path, "/api/v0/") || r.URL.Path == "/api/v1/chat"
}

// localName reports whether host (a Host header) names this machine: an IP
// literal, localhost, or the machine's own hostname. A domain anyone else
// controls is not one, whatever it resolves to today.
func localName(host string) bool {
	h, _, err := net.SplitHostPort(host)
	if err != nil {
		h = host
	}
	h = strings.TrimSuffix(strings.Trim(h, "[]"), ".")
	if net.ParseIP(h) != nil || strings.EqualFold(h, "localhost") || strings.HasSuffix(strings.ToLower(h), ".localhost") {
		return true
	}
	if self, err := os.Hostname(); err == nil {
		short, _, _ := strings.Cut(self, ".")
		if strings.EqualFold(h, self) || strings.EqualFold(h, short) {
			return true
		}
	}
	return false
}

// corsOrigins is the allowed list for /v1: nil is off, "*" is anyone.
type corsOrigins = atomic.Pointer[[]string]

func (s *Server) corsAllows(origin string) bool {
	p := s.cors.Load()
	if p == nil || origin == "" {
		return false
	}
	origin = strings.TrimSuffix(origin, "/")
	for _, o := range *p {
		if o == "*" || strings.EqualFold(strings.TrimSuffix(o, "/"), origin) {
			return true
		}
	}
	return false
}

// SetCORS sets the origins allowed to call /v1 from a browser. Empty is off.
// Safe to call while serving.
func (s *Server) SetCORS(origins []string) {
	if len(origins) == 0 {
		s.cors.Store(nil)
		return
	}
	cp := append([]string(nil), origins...)
	s.cors.Store(&cp)
}

// guardBrowser refuses what a page from elsewhere must not do, and answers a
// CORS preflight for an allowed one. It reports whether it wrote a response.
func (s *Server) guardBrowser(w http.ResponseWriter, r *http.Request) bool {
	if !fromBrowser(r) {
		return false
	}
	api := apiPath(r)
	if !api && !localName(r.Host) {
		writeError(w, http.StatusForbidden, "refused: a browser reached this node as "+r.Host+
			", which is not a name this machine answers to; open it at http://127.0.0.1 or http://localhost")
		return true
	}
	origin := r.Header.Get("Origin")
	allowed := api && s.corsAllows(origin)
	if allowed && r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
		h := w.Header()
		setCORS(h, origin)
		h.Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		if req := r.Header.Get("Access-Control-Request-Headers"); req != "" {
			h.Set("Access-Control-Allow-Headers", req)
		}
		h.Set("Access-Control-Max-Age", "600")
		w.WriteHeader(http.StatusNoContent)
		return true
	}
	if unsafeMethod(r.Method) && crossSite(r) && !allowed {
		msg := "cross-site request refused: a page from " + originOrUnknown(origin) + " may not "
		if api {
			msg += "call this node; to let a web app use /v1, add its origin to cors_origins"
		} else {
			msg += "manage this node"
		}
		writeError(w, http.StatusForbidden, msg)
		return true
	}
	return false
}

func originOrUnknown(o string) string {
	if o == "" {
		return "another site"
	}
	return o
}

func setCORS(h http.Header, origin string) {
	h.Set("Access-Control-Allow-Origin", origin)
	h.Add("Vary", "Origin")
	h.Set("Access-Control-Expose-Headers", "X-Fabric-Node, X-Fabric-Engine, X-Fabric-Via, X-Fabric-Trace")
}

// corsWriter makes ModelFabric the only one deciding CORS: an engine's own
// headers are dropped as the response starts — llama-server echoes any origin
// with credentials allowed — and ModelFabric's are set when the origin is
// allowed. Flush and Unwrap keep streaming working through it.
type corsWriter struct {
	http.ResponseWriter
	origin  string // set only when allowed
	written bool
}

func (c *corsWriter) fix() {
	if c.written {
		return
	}
	c.written = true
	h := c.ResponseWriter.Header()
	for k := range h {
		if strings.HasPrefix(http.CanonicalHeaderKey(k), "Access-Control-") {
			delete(h, k)
		}
	}
	if c.origin != "" {
		setCORS(h, c.origin)
	}
}

func (c *corsWriter) WriteHeader(code int) {
	c.fix()
	c.ResponseWriter.WriteHeader(code)
}

func (c *corsWriter) Write(b []byte) (int, error) {
	c.fix()
	return c.ResponseWriter.Write(b)
}

func (c *corsWriter) Flush() {
	c.fix()
	if f, ok := c.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (c *corsWriter) Unwrap() http.ResponseWriter { return c.ResponseWriter }

// browserSafe wraps a listener's handler with guardBrowser and corsWriter.
func (s *Server) browserSafe(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.guardBrowser(w, r) {
			return
		}
		cw := &corsWriter{ResponseWriter: w}
		if o := r.Header.Get("Origin"); o != "" && apiPath(r) && s.corsAllows(o) {
			cw.origin = o
		}
		next.ServeHTTP(cw, r)
	})
}
