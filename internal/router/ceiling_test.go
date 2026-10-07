package router

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func limitOf(t *testing.T, body []byte) (int, bool) {
	t.Helper()
	var f map[string]json.RawMessage
	if err := json.Unmarshal(body, &f); err != nil {
		t.Fatalf("result is not JSON: %s", body)
	}
	raw, ok := f["max_tokens"]
	if !ok {
		return 0, false
	}
	var n int
	if err := json.Unmarshal(raw, &n); err != nil {
		return 0, false
	}
	return n, true
}

// Requests without an output limit need a ceiling; otherwise generation
// can consume the full context window after the caller stops waiting.
func TestRequestWithNoLimitGetsOne(t *testing.T) {
	got, ok := limitOf(t, capOutput([]byte(`{"model":"m","messages":[]}`), 4096))
	if !ok {
		t.Fatal("no ceiling was applied to a request that named none")
	}
	if got != 4096 {
		t.Errorf("max_tokens = %d, want 4096", got)
	}
}

// Treat max_tokens:null as unset so schema-generated clients receive the
// default output ceiling.
func TestExplicitNullCountsAsAbsent(t *testing.T) {
	got, ok := limitOf(t, capOutput([]byte(`{"model":"m","max_tokens":null}`), 999))
	if !ok || got != 999 {
		t.Errorf("max_tokens = %d (present=%v), want 999: null means unset", got, ok)
	}
}

// A client that states a limit has made a decision. Overriding it would make
// ModelFabric lie about what it ran.
func TestAStatedLimitIsNeverOverridden(t *testing.T) {
	for _, body := range []string{
		`{"model":"m","max_tokens":10}`,
		`{"model":"m","max_completion_tokens":10}`,
		`{"model":"m","n_predict":10}`,
	} {
		out := capOutput([]byte(body), 4096)
		if string(out) != body {
			t.Errorf("body was rewritten:\n  in  %s\n  out %s", body, out)
		}
	}
}

// Zero restores the old behaviour for an operator who wants it, and a body that
// cannot be parsed is left for the engine to reject in its own words.
func TestCapOutputLeavesWhatItCannotOrShouldNotTouch(t *testing.T) {
	body := []byte(`{"model":"m"}`)
	if out := capOutput(body, 0); string(out) != string(body) {
		t.Errorf("a zero ceiling should change nothing, got %s", out)
	}
	bad := []byte(`not json at all`)
	if out := capOutput(bad, 4096); string(out) != string(bad) {
		t.Errorf("a malformed body should pass through untouched, got %s", out)
	}
}

// A top-level null unmarshals into a nil map without an error. Adding a limit
// used to panic before the engine could reject the invalid request itself.
func TestCapOutputLeavesNonObjectsUntouched(t *testing.T) {
	for _, body := range []string{"null", " null \n", "[]", `"text"`, "42", "true"} {
		t.Run(body, func(t *testing.T) {
			if out := capOutput([]byte(body), 128); string(out) != body {
				t.Fatalf("non-object changed to %s", out)
			}
		})
	}
}

// Only generating paths. An embedding has no output length to bound, and
// inserting a field into one would be ModelFabric inventing part of the request.
func TestOnlyGeneratingPathsAreCapped(t *testing.T) {
	for path, want := range map[string]bool{
		"/v1/chat/completions":     true,
		"/v1/completions":          true,
		"/v1/responses":            true,
		"/v1/messages":             true,
		"/v1/embeddings":           false,
		"/v1/rerank":               false,
		"/v1/audio/transcriptions": false,
	} {
		if got := generates(path); got != want {
			t.Errorf("generates(%q) = %v, want %v", path, got, want)
		}
	}
}

func TestTheEngineReceivesTheCeiling(t *testing.T) {
	seen := make(chan []byte, 1)
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		b := make([]byte, req.ContentLength)
		_, _ = req.Body.Read(b)
		seen <- b
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer engine.Close()

	r := routerWith(t, engine)
	r.MaxOutputTokens = 1234
	rec := httptest.NewRecorder()
	r.Forward(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"m","messages":[{"content":"hi"}]}`)), "/v1/chat/completions")

	select {
	case body := <-seen:
		if got, ok := limitOf(t, body); !ok || got != 1234 {
			t.Errorf("the engine received max_tokens=%d (present=%v), want 1234", got, ok)
		}
	default:
		t.Fatalf("the engine was never called: %d %s", rec.Code, rec.Body)
	}
}
