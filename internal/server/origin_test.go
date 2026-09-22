package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gregbugaj/modelfabric/internal/nodekey"
)

type browserReq struct {
	method, path, host, origin, site, ctype, body string
	preflight                                     bool
}

func (b browserReq) do(h http.Handler) *httptest.ResponseRecorder {
	if b.body == "" && b.method == http.MethodPost {
		b.body = `{"model":"m","messages":[],"name":"t"}`
	}
	req := httptest.NewRequest(b.method, b.path, strings.NewReader(b.body))
	req.Host = b.host
	if b.origin != "" {
		req.Header.Set("Origin", b.origin)
	}
	if b.site != "" {
		req.Header.Set("Sec-Fetch-Site", b.site)
	}
	if b.ctype != "" {
		req.Header.Set("Content-Type", b.ctype)
	}
	if b.preflight {
		req.Header.Set("Access-Control-Request-Method", "POST")
		req.Header.Set("Access-Control-Request-Headers", "authorization, content-type")
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

const local = "127.0.0.1:1234"

// Measured before this guard: a page from https://evil.example POSTed
// text/plain to /api/v1/tokens and got 201 — any site could manage the node.
func TestBrowserGuard(t *testing.T) {
	tests := []struct {
		name string
		cors []string
		req  browserReq
		want int
		// acao is the Access-Control-Allow-Origin the response must carry; "-" none.
		acao string
	}{
		{name: "a cross-site page cannot manage the node",
			req:  browserReq{method: "POST", path: "/api/v1/tokens", host: local, origin: "https://evil.example", site: "cross-site", ctype: "text/plain"},
			want: 403, acao: "-"},
		{name: "the dashboard, same origin, can",
			req:  browserReq{method: "POST", path: "/api/v1/tokens", host: local, origin: "http://" + local, site: "same-origin", ctype: "application/json"},
			want: 201, acao: "-"},
		{name: "a client that is not a browser is not asked where it came from",
			req:  browserReq{method: "POST", path: "/api/v1/tokens", host: local, ctype: "application/json"},
			want: 201, acao: "-"},
		{name: "DNS rebinding: same-origin by a name that is not this machine is refused",
			req:  browserReq{method: "POST", path: "/api/v1/tokens", host: "evil.example:1234", origin: "http://evil.example:1234", site: "same-origin"},
			want: 403, acao: "-"},
		{name: "DNS rebinding: reading the dashboard by that name is refused too",
			req:  browserReq{method: "GET", path: "/api/v1/tokens", host: "evil.example:1234", site: "same-origin"},
			want: 403, acao: "-"},
		{name: "localhost is this machine",
			req:  browserReq{method: "GET", path: "/z/mesh", host: "localhost:1234", site: "same-origin"},
			want: 200, acao: "-"},
		{name: "with CORS off, a cross-site page cannot run a model",
			req:  browserReq{method: "POST", path: "/v1/chat/completions", host: local, origin: "http://localhost:3000", site: "same-site", ctype: "text/plain"},
			want: 403, acao: "-"},
		{name: "with CORS off, the engine's echoed origin is not passed on",
			req:  browserReq{method: "POST", path: "/v1/chat/completions", host: local, origin: "http://" + local, site: "same-origin", ctype: "application/json"},
			want: 200, acao: "-"},
		{name: "an allowed origin can call /v1, and gets exactly one allow-origin",
			cors: []string{"http://localhost:3000"},
			req:  browserReq{method: "POST", path: "/v1/chat/completions", host: local, origin: "http://localhost:3000", site: "same-site", ctype: "application/json"},
			want: 200, acao: "http://localhost:3000"},
		// /api/v1/chat is an app's route that happens to live beside the
		// management ones: an origin without leave is still refused there, and
		// one with it gets through the guard (the fixture has no front door
		// for the chat to call, which is the 503).
		{name: "an allowed origin can call /api/v1/chat",
			cors: []string{"http://localhost:3000"},
			req:  browserReq{method: "POST", path: "/api/v1/chat", host: local, origin: "http://localhost:3000", site: "same-site", ctype: "application/json", body: `{"model":"m","input":"hi"}`},
			want: 503, acao: "http://localhost:3000"},
		{name: "an origin that is not allowed cannot call /api/v1/chat",
			cors: []string{"http://localhost:3000"},
			req:  browserReq{method: "POST", path: "/api/v1/chat", host: local, origin: "https://evil.example", site: "cross-site", ctype: "application/json"},
			want: 403, acao: "-"},
		{name: "an allowed origin still cannot manage the node",
			cors: []string{"*"},
			req:  browserReq{method: "POST", path: "/api/v1/tokens", host: local, origin: "http://localhost:3000", site: "same-site"},
			want: 403, acao: "-"},
		{name: "a listed origin does not open the door to another",
			cors: []string{"http://localhost:3000"},
			req:  browserReq{method: "POST", path: "/v1/chat/completions", host: local, origin: "https://evil.example", site: "cross-site"},
			want: 403, acao: "-"},
		{name: "preflight from an allowed origin is answered",
			cors: []string{"*"},
			req:  browserReq{method: "OPTIONS", path: "/v1/chat/completions", host: local, origin: "https://app.example", site: "cross-site", preflight: true},
			want: 204, acao: "https://app.example"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newFrontFixture(t, false, false)
			f.srv.SetTokens(nodekey.Tokens(t.TempDir()))
			f.srv.SetCORS(tc.cors)
			rec := tc.req.do(f.srv.FrontHandler())
			if rec.Code != tc.want {
				t.Fatalf("got %d %s, want %d", rec.Code, rec.Body, tc.want)
			}
			got := rec.Header().Values("Access-Control-Allow-Origin")
			switch {
			case tc.acao == "-" && len(got) != 0:
				t.Fatalf("Access-Control-Allow-Origin = %v, want none", got)
			case tc.acao != "-" && (len(got) != 1 || got[0] != tc.acao):
				t.Fatalf("Access-Control-Allow-Origin = %v, want exactly %q", got, tc.acao)
			}
			if c := rec.Header().Get("Access-Control-Allow-Credentials"); c != "" {
				t.Fatalf("Allow-Credentials %q leaked through", c)
			}
		})
	}
}

// The dashboard manages peers through /api/v1/nodes. Its Origin is this
// node's; forwarded, the peer would see a foreign origin and refuse the
// dashboard's every action. The browser was already checked here.
func TestNodeProxyDropsBrowserHeaders(t *testing.T) {
	h := http.Header{}
	for _, k := range []string{"Origin", "Referer", "Sec-Fetch-Site", "Sec-Fetch-Mode", "Content-Type"} {
		h.Set(k, "x")
	}
	dropBrowserHeaders(h)
	if len(h) != 1 || h.Get("Content-Type") == "" {
		t.Fatalf("left %v, want only Content-Type", h)
	}
}

func TestLocalName(t *testing.T) {
	for host, want := range map[string]bool{
		"127.0.0.1:1234": true, "[::1]:1234": true, "localhost:1234": true, "app.localhost": true,
		"100.64.0.7:1234": true, "evil.example:1234": false, "evil.example": false,
	} {
		if got := localName(host); got != want {
			t.Errorf("localName(%q) = %v, want %v", host, got, want)
		}
	}
}
