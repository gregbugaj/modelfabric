package server

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gregbugaj/modelfabric/internal/config"
	"github.com/gregbugaj/modelfabric/internal/mesh"
	"github.com/gregbugaj/modelfabric/internal/router"
)

func streamingEngine(t *testing.T, asked *bool) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var req map[string]any
		_ = json.Unmarshal(b, &req)
		*asked = req["stream"] == true

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		for _, frame := range []string{
			`{"id":"c1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","content":"hi"}}]}`,
			`{"id":"c1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":" there"}}]}`,
			`{"id":"c1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		} {
			io.WriteString(w, "data: "+frame+"\n\n")
			w.(http.Flusher).Flush()
		}
		io.WriteString(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(srv.Close)
	return srv
}

func routerFixture(t *testing.T, engine *httptest.Server) *Server {
	t.Helper()
	m := mesh.New(config.Default(), "self")
	e := mesh.NewEngine("e", engine.URL)
	e.MarkReady("m")
	m.RegisterEngine(e)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	s := New(m, router.New(m, log), nil, log, nil)
	s.SetAuth(func() (string, error) { return testKey, nil }, false)
	return s
}

func TestRouterUpgradesNonStreamingWhileSomeoneIsWatching(t *testing.T) {
	var askedForStream bool
	s := routerFixture(t, streamingEngine(t, &askedForStream))

	events, stop := s.tokens.Subscribe(16)
	defer stop()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"m","stream":false,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	s.FrontHandler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if !askedForStream {
		t.Error("the engine was not asked for a stream, so there was nothing to watch")
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type %q, want application/json: the caller did not ask for a stream", ct)
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("caller got something that is not JSON: %v\n%s", err, rec.Body)
	}
	if out["object"] != "chat.completion" {
		t.Errorf("object = %v, want chat.completion (not a chunk)", out["object"])
	}
	msg := firstMessage(t, out)
	if msg["content"] != "hi there" {
		t.Errorf("content = %q, want the reassembled %q", msg["content"], "hi there")
	}

	deadline := time.After(2 * time.Second)
	var text strings.Builder
	for text.Len() < len("hi there") {
		select {
		case ev := <-events:
			text.WriteString(ev.Text)
		case <-deadline:
			t.Fatalf("the tap saw %q, want the reply as it was written", text.String())
		}
	}
	if !strings.Contains(text.String(), "hi") {
		t.Errorf("the tap saw %q", text.String())
	}
}

func TestNoUpgradeWhenNobodyIsWatching(t *testing.T) {
	var askedForStream bool
	s := routerFixture(t, streamingEngine(t, &askedForStream))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"m","stream":false,"messages":[]}`))
	req.Header.Set("Content-Type", "application/json")
	s.FrontHandler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if askedForStream {
		t.Error("the request was upgraded although nothing was watching")
	}
}

// A streaming caller is left alone: it already gets frames as they arrive, and
// reassembling them would be the opposite of what it asked for.
func TestAStreamingCallerIsNotReassembled(t *testing.T) {
	var askedForStream bool
	s := routerFixture(t, streamingEngine(t, &askedForStream))
	// Watching, so the upgrade would fire if it did not check for stream:true.
	_, stop := s.tokens.Subscribe(16)
	defer stop()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"m","stream":true,"messages":[]}`))
	req.Header.Set("Content-Type", "application/json")
	s.FrontHandler().ServeHTTP(rec, req)

	if !strings.Contains(rec.Body.String(), "data: ") {
		t.Errorf("a streaming caller should receive frames, got: %s", rec.Body)
	}
}
