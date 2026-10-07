package router

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gregbugaj/modelfabric/internal/config"
	"github.com/gregbugaj/modelfabric/internal/mesh"
)

func routerWith(t *testing.T, upstreams ...*httptest.Server) *Router {
	t.Helper()
	m := mesh.New(config.Default(), "self")
	for i, u := range upstreams {
		e := mesh.NewEngine(string(rune('a'+i)), u.URL) // a, b, ... in order
		e.MarkReady("m")
		m.RegisterEngine(e)
	}
	return New(m, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func post(r *Router) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"m","messages":[{"content":"hi"}]}`))
	r.Forward(rec, req, "/v1/chat/completions")
	return rec
}

func TestRetriesServerErrorBeforeCommit(t *testing.T) {
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer bad.Close()
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer good.Close()

	rec := post(routerWith(t, bad, good))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "ok") {
		t.Fatalf("expected the retry to succeed, got %d %q", rec.Code, rec.Body.String())
	}
}

// Once response headers have been sent the response is committed. Retrying
// would write a second set of headers and a second body into the same reply.
func TestNoRetryAfterHeadersCommitted(t *testing.T) {
	dies := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "1000") // promise a body...
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		hj, _ := w.(http.Hijacker)
		conn, _, _ := hj.Hijack()
		conn.Close() // ...and die before sending it
	}))
	defer dies.Close()
	var second atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		second.Add(1)
		_, _ = io.WriteString(w, `{"second":true}`)
	}))
	defer other.Close()

	rec := post(routerWith(t, dies, other))
	if n := second.Load(); n != 0 {
		t.Fatalf("retried after headers were committed (%d calls to the second upstream)", n)
	}
	if strings.Contains(rec.Body.String(), "second") {
		t.Fatal("a second response body was appended to a committed one")
	}
}

func TestRouteEventsCarryNoContent(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"secret-completion":"x"}`)
	}))
	defer up.Close()
	r := routerWith(t, up)
	var got []Event
	r.OnRoute = func(e Event) { got = append(got, e) }
	post(r)
	if len(got) != 1 || got[0].Model != "m" || got[0].Status != 200 || got[0].BytesOut == 0 {
		t.Fatalf("event = %+v", got)
	}
	if got[0].ReqBody != "" || got[0].RespBody != "" {
		t.Fatalf("route events must not carry request or response content with capture off: %+v", got[0])
	}
}

// A request sharing a prefix with one still in flight follows it: the prefix
// is recorded when the first is placed, not when it finishes. Otherwise every
// request arriving during a long prefill is placed without a hint.
func TestAffinityFollowsInFlightPrefix(t *testing.T) {
	release := make(chan struct{})
	var hits [2]atomic.Int32
	handler := func(i int) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) {
			hits[i].Add(1)
			<-release
			_, _ = io.WriteString(w, `{"ok":true}`)
		}
	}
	a := httptest.NewServer(handler(0))
	defer a.Close()
	b := httptest.NewServer(handler(1))
	defer b.Close()
	r := routerWith(t, a, b)
	r.EnablePrefixAffinity()

	send := func(q string) chan struct{} {
		done := make(chan struct{})
		go func() {
			defer close(done)
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
				strings.NewReader(`{"model":"m","messages":[{"content":"`+doc(3000)+`"},{"content":"`+q+`"}]}`))
			r.Forward(httptest.NewRecorder(), req, "/v1/chat/completions")
		}()
		return done
	}
	first := send("question one")
	for hits[0].Load()+hits[1].Load() < 1 {
		runtime.Gosched()
	}
	second := send("question two")
	for hits[0].Load()+hits[1].Load() < 2 {
		runtime.Gosched()
	}
	close(release)
	<-first
	<-second
	if hits[0].Load() != 2 && hits[1].Load() != 2 {
		t.Fatalf("requests sharing a document split across engines: a=%d b=%d", hits[0].Load(), hits[1].Load())
	}
}

func TestJITLoadsThenRoutes(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer up.Close()
	r := routerWith(t)
	var gotTTL int
	r.JIT = func(_ context.Context, model string, ttl int) (bool, error) {
		gotTTL = ttl
		e := mesh.NewEngine("jit-1", up.URL)
		e.MarkReady(model)
		r.m.RegisterEngine(e)
		return true, nil
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"m","ttl":300,"messages":[{"content":"hi"}]}`))
	r.Forward(rec, req, "/v1/chat/completions")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "ok") {
		t.Fatalf("got %d %q", rec.Code, rec.Body.String())
	}
	if gotTTL != 300 {
		t.Errorf("request ttl not passed to JIT: %d", gotTTL)
	}
}

// When JIT does not apply the response is the ordinary 404, and a forwarded
// request never loads anything: only the node the client called decides.
func TestJITNotAttemptedOrForwardedKeeps404(t *testing.T) {
	r := routerWith(t)
	calls := 0
	r.JIT = func(context.Context, string, int) (bool, error) { calls++; return false, nil }
	if rec := post(r); rec.Code != http.StatusNotFound {
		t.Fatalf("got %d, want 404", rec.Code)
	}
	if calls != 1 {
		t.Fatalf("JIT consulted %d times", calls)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"m","messages":[{"content":"hi"}]}`))
	req.Header.Set(HopHeader, "1")
	rec := httptest.NewRecorder()
	r.Forward(rec, req, "/v1/chat/completions")
	if calls != 1 || rec.Code != http.StatusNotFound {
		t.Fatalf("forwarded request consulted JIT (calls=%d, code=%d)", calls, rec.Code)
	}
}

func TestJITFailureIs503(t *testing.T) {
	r := routerWith(t)
	r.JIT = func(context.Context, string, int) (bool, error) { return true, errors.New("out of VRAM") }
	rec := post(r)
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "out of VRAM") {
		t.Fatalf("got %d %q", rec.Code, rec.Body.String())
	}
}

func TestWithoutJITBlocksLoad(t *testing.T) {
	r := routerWith(t)
	calls := 0
	r.JIT = func(context.Context, string, int) (bool, error) { calls++; return true, nil }
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"m","messages":[{"content":"hi"}]}`))
	req = req.WithContext(WithoutJIT(req.Context()))
	rec := httptest.NewRecorder()
	r.Forward(rec, req, "/v1/chat/completions")
	if calls != 0 || rec.Code != http.StatusNotFound {
		t.Fatalf("tailnet request consulted JIT (calls=%d, code=%d)", calls, rec.Code)
	}
}

// Forwarding must strip caller credentials before dispatch to engines or peers.
func TestClientCredentialsAreNotForwarded(t *testing.T) {
	src := http.Header{}
	src.Set("Authorization", "Bearer user-secret")
	src.Set("Cookie", "session=abc")
	src.Set("X-Api-Key", "user-secret")
	src.Set("X-Request-Id", "keep-me")
	src.Set("Connection", "keep-alive")
	dst := http.Header{}
	copyRequestHeaders(dst, src)
	for _, gone := range []string{"Authorization", "Cookie", "X-Api-Key", "Connection"} {
		if dst.Get(gone) != "" {
			t.Errorf("%s was forwarded: %q", gone, dst.Get(gone))
		}
	}
	if dst.Get("X-Request-Id") != "keep-me" {
		t.Error("ordinary headers should still be forwarded")
	}
}

// 8KB cut a multimodal request off before anything worth reading: a body
// carrying a base64 image runs to megabytes, and 8KB did not reach the end of
// the message list on an ordinary tool-calling exchange.
func TestDefaultBodyCapHoldsARealisticPrompt(t *testing.T) {
	if DefaultBodyCap != 32<<10 {
		t.Fatalf("the default body cap is 32KB, got %d", DefaultBodyCap)
	}
	b := BodyLog{Enabled: true}
	if got := b.Cap(); got != DefaultBodyCap {
		t.Errorf("an unset Max takes the default, got %d", got)
	}
	body := make([]byte, 20<<10)
	if s, cut := b.Clip(body); cut || len(s) != len(body) {
		t.Errorf("20KB fits in 32KB: cut=%v len=%d", cut, len(s))
	}
	big := make([]byte, 40<<10)
	if s, cut := b.Clip(big); !cut || len(s) != DefaultBodyCap {
		t.Errorf("40KB must be cut to the cap: cut=%v len=%d", cut, len(s))
	}
	if got := (BodyLog{Enabled: true, Max: 4096}).Cap(); got != 4096 {
		t.Errorf("an explicit Max stands, got %d", got)
	}
}
