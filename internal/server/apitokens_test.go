package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gregbugaj/modelfabric/internal/nodekey"
	"github.com/gregbugaj/modelfabric/internal/router"
)

func withToken(t *testing.T, f *frontFixture, name string) (*nodekey.Store, string) {
	t.Helper()
	store := nodekey.Tokens(t.TempDir())
	secret, _, err := store.Create(name)
	if err != nil {
		t.Fatal(err)
	}
	f.srv.SetTokens(store)
	return store, secret
}

// One key per node meant revoking any client revoked them all. A named token
// opens the same doors for inference as the node key, and stops at revoke.
func TestNamedTokensOnTheFrontDoors(t *testing.T) {
	tests := []struct {
		name    string
		handler func(*Server) http.Handler
		revoke  bool
		want    int
	}{
		{name: "loopback with require_api_key accepts a token", handler: (*Server).FrontHandler, want: 200},
		{name: "public listener accepts a token", handler: (*Server).PublicHandler, want: 200},
		{name: "loopback refuses a revoked token", handler: (*Server).FrontHandler, revoke: true, want: 401},
		{name: "public listener refuses a revoked token", handler: (*Server).PublicHandler, revoke: true, want: 401},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newFrontFixture(t, false, true)
			store, secret := withToken(t, f, "laptop")
			if tc.revoke {
				if _, err := store.Revoke("laptop"); err != nil {
					t.Fatal(err)
				}
			}
			if rec := call(tc.handler(f.srv), secret, ""); rec.Code != tc.want {
				t.Fatalf("got %d %s, want %d", rec.Code, rec.Body, tc.want)
			}
		})
	}
}

// The node key also proves a request came from this node, which is what lets
// it keep the forwarding marker. A token is someone else's credential: with it
// the marker must be stripped, or any app holding one could skip the
// scheduler as if the mesh had already forwarded its request.
func TestNamedTokenCannotClaimToBeTheNode(t *testing.T) {
	f := newFrontFixture(t, true, false)
	_, secret := withToken(t, f, "laptop")
	rec := call(f.srv.FrontHandler(), secret, router.HopHeader)
	if rec.Code != 200 {
		t.Fatalf("got %d %s", rec.Code, rec.Body)
	}
	if f.engineHits != 0 || len(f.schedAuth) != 1 {
		t.Fatalf("a token kept the forwarding marker (engine %d, scheduler %d)", f.engineHits, len(f.schedAuth))
	}
}

func TestTokensAPI(t *testing.T) {
	do := func(h http.Handler, method, path, body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(method, path, strings.NewReader(body)))
		return rec
	}
	f := newFrontFixture(t, false, true)
	f.srv.SetTokens(nodekey.Tokens(t.TempDir()))
	h := f.srv.Handler()

	created := do(h, http.MethodPost, "/api/v1/tokens", `{"name":"laptop"}`)
	if created.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", created.Code, created.Body)
	}
	var c struct{ ID, Token string }
	json.Unmarshal(created.Body.Bytes(), &c)
	if !strings.HasPrefix(c.Token, "sk-mfsh-") || c.ID == "" {
		t.Fatalf("create answered %s", created.Body)
	}

	tests := []struct {
		name, method, path, body string
		want                     int
		check                    func(t *testing.T, body string)
	}{
		{name: "the list shows the token but never its secret or hash", method: "GET", path: "/api/v1/tokens", want: 200,
			check: func(t *testing.T, body string) {
				if !strings.Contains(body, `"laptop"`) || strings.Contains(body, c.Token) || strings.Contains(body, `"hash"`) {
					t.Fatalf("list = %s", body)
				}
				if !strings.Contains(body, `"node_key":"`+testKey[len(testKey)-4:]+`"`) {
					t.Fatalf("the node key is not listed by its hint: %s", body)
				}
			}},
		{name: "a duplicate name is a bad request", method: "POST", path: "/api/v1/tokens", body: `{"name":"LAPTOP"}`, want: 400},
		{name: "a blank name is a bad request", method: "POST", path: "/api/v1/tokens", body: `{"name":""}`, want: 400},
		{name: "revoking an unknown token is not found", method: "POST", path: "/api/v1/tokens/revoke", body: `{"id":"nope"}`, want: 404},
		{name: "revoke by id", method: "POST", path: "/api/v1/tokens/revoke", body: `{"id":"` + c.ID + `"}`, want: 200},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := do(h, tc.method, tc.path, tc.body)
			if rec.Code != tc.want {
				t.Fatalf("got %d %s, want %d", rec.Code, rec.Body, tc.want)
			}
			if tc.check != nil {
				tc.check(t, rec.Body.String())
			}
		})
	}
	if rec := call(f.srv.FrontHandler(), c.Token, ""); rec.Code != 401 {
		t.Fatalf("revoked through the API, the token still answered %d", rec.Code)
	}
}

func TestTokensAPIWithoutAStore(t *testing.T) {
	f := newFrontFixture(t, false, false)
	rec := httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/tokens", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("got %d, want 404", rec.Code)
	}
}

func TestRotatingTheNodeKey(t *testing.T) {
	f := newFrontFixture(t, false, true)
	home := t.TempDir()
	f.srv.SetAuth(func() (string, error) { return nodekey.Key(home) }, true)
	f.srv.SetKeyHome(home)
	_, token := withToken(t, f, "laptop")
	old, err := nodekey.Key(home)
	if err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/key/rotate", nil))
	var got struct{ Key, Hint string }
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &got) != nil || got.Key == "" || got.Key == old {
		t.Fatalf("rotate: %d %s", rec.Code, rec.Body)
	}
	for _, tc := range []struct {
		name string
		key  string
		want int
	}{
		{"the old key is refused", old, 401},
		{"the new key is accepted", got.Key, 200},
		{"a named token still works", token, 200},
	} {
		if rec := call(f.srv.FrontHandler(), tc.key, ""); rec.Code != tc.want {
			t.Errorf("%s: got %d, want %d", tc.name, rec.Code, tc.want)
		}
	}

	rec = httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/key", nil))
	if !strings.Contains(rec.Body.String(), `"home":"`+home+`"`) {
		t.Fatalf("key info: %s", rec.Body)
	}
}

// /api/v1/chat is the one route under /api/v1 an app calls: a key protects it
// like /v1, the public listener serves it, and its model calls go through the
// front door. The MCP switches are off until saved on.
func TestChatEndpointIsInferenceNotManagement(t *testing.T) {
	f := newFrontFixture(t, false, true)
	_, token := withToken(t, f, "app")
	post := func(h http.Handler, key, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/chat", strings.NewReader(body))
		if key != "" {
			req.Header.Set("Authorization", "Bearer "+key)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	const hello = `{"model":"m","input":"hi"}`
	const withMCP = `{"model":"m","input":"hi","integrations":[{"type":"ephemeral_mcp","server_label":"x","server_url":"http://127.0.0.1:1/mcp"}]}`
	tests := []struct {
		name    string
		handler http.Handler
		key     string
		body    string
		want    int
		says    string
	}{
		{name: "loopback without a key is refused when one is required", handler: f.srv.FrontHandler(), body: hello, want: 401},
		{name: "the public listener without a key is refused", handler: f.srv.PublicHandler(), body: hello, want: 401},
		{name: "the public listener serves it to a token, and says there is no front door to call", handler: f.srv.PublicHandler(), key: token, body: hello, want: 503, says: "no front door"},
		{name: "an MCP server in the request is refused while the switch is off", handler: f.srv.FrontHandler(), key: token, body: withMCP, want: 403, says: "mcp_allow_ephemeral"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := post(tc.handler, tc.key, tc.body)
			if rec.Code != tc.want || !strings.Contains(rec.Body.String(), tc.says) {
				t.Fatalf("got %d %s, want %d saying %q", rec.Code, rec.Body, tc.want, tc.says)
			}
		})
	}
}

// Reveal shows the node key where `mfsh key` would print it, and nowhere else.
func TestRevealingTheNodeKey(t *testing.T) {
	f := newFrontFixture(t, false, true)
	home := t.TempDir()
	f.srv.SetAuth(func() (string, error) { return nodekey.Key(home) }, true)
	f.srv.SetKeyHome(home)
	want, err := nodekey.Key(home)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name    string
		handler http.Handler
		method  string
		status  int
		shown   bool
	}{
		{"on the node's own management address", f.srv.Handler(), http.MethodPost, 200, true},
		{"a GET is not how a secret is asked for", f.srv.Handler(), http.MethodGet, 405, false},
		// Over the tailnet it is refused before anyone's identity is looked
		// at: the owner's other devices included.
		{"over the mesh, from any device", f.srv.PeerHandler(), http.MethodPost, 403, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(c.method, "/api/v1/key/reveal", nil)
			req.RemoteAddr = "100.64.0.9:40000"
			c.handler.ServeHTTP(rec, req)
			if rec.Code != c.status {
				t.Fatalf("status %d, want %d: %s", rec.Code, c.status, rec.Body)
			}
			if got := strings.Contains(rec.Body.String(), want); got != c.shown {
				t.Errorf("key in the answer: %v, want %v", got, c.shown)
			}
			if c.shown && rec.Header().Get("Cache-Control") != "no-store" {
				t.Error("an answer holding the key must not be cached")
			}
		})
	}
}
