package server

import (
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
)

// Loopback alone does not block browser attacks: cross-origin text/plain
// POSTs can reach JSON handlers without preflight. Browser mutations therefore
// require the node's own origin or an allowed inference CORS origin.
// Management requests also require a local Host name to prevent DNS rebinding
// from making an attacker-controlled domain appear same-origin.
// These checks apply to requests carrying Origin or Sec-Fetch-Site.

// fromBrowser reports a request a browser made: every current browser sends
// Sec-Fetch-Site, and Origin on anything cross-origin or unsafe.
func fromBrowser(r *http.Request) bool {
	return r.Header.Get("Origin") != "" || r.Header.Get("Sec-Fetch-Site") != ""
}

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

func apiPath(r *http.Request) bool {
	// /api/v1/chat is the one app route among the management ones.
	return strings.HasPrefix(r.URL.Path, "/v1/") || strings.HasPrefix(r.URL.Path, "/api/v0/") || r.URL.Path == "/api/v1/chat"
}

// localName accepts an IP literal, localhost or this machine's hostname.
// Other domains are rejected regardless of their DNS resolution.
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

// guardBrowser enforces browser-origin policy and answers allowed CORS
// preflights. It reports whether it wrote a response.
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

// corsWriter replaces engine CORS headers with this node's policy; llama-server
// otherwise reflects arbitrary origins with credentials allowed. Flush and
// Unwrap preserve streaming.
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
