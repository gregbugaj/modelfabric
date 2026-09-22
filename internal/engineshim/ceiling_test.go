package engineshim

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
)

func request(t *testing.T, path, body string) *http.Request {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, path, bytes.NewReader([]byte(body)))
	r.ContentLength = int64(len(body))
	return r
}

func bodyOf(t *testing.T, r *http.Request) map[string]any {
	t.Helper()
	b, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("body is not JSON after rewriting: %s", b)
	}
	// Content-Length must describe what the proxy will actually send, or the
	// engine reads a truncated body and fails on valid JSON.
	if got := int64(len(b)); r.ContentLength != got {
		t.Errorf("ContentLength = %d but body is %d bytes", r.ContentLength, got)
	}
	if h := r.Header.Get("Content-Length"); h != "" && h != strconv.Itoa(len(b)) {
		t.Errorf("Content-Length header %q disagrees with the %d-byte body", h, len(b))
	}
	return out
}

// The path the incident took: the shim was dialled directly, so the router
// never saw the request. A ceiling that lives only in the router would have
// missed exactly the case it was written for.
func TestShimCapsARequestThatNamesNoLimit(t *testing.T) {
	r := request(t, "/v1/chat/completions", `{"model":"m","messages":[]}`)
	if !capOutput(r, 2048) {
		t.Fatal("no ceiling applied")
	}
	if got := bodyOf(t, r)["max_tokens"]; got != float64(2048) {
		t.Errorf("max_tokens = %v, want 2048", got)
	}
}

func TestShimLeavesAStatedLimitAlone(t *testing.T) {
	for _, body := range []string{
		`{"model":"m","max_tokens":7}`,
		`{"model":"m","max_completion_tokens":7}`,
		`{"model":"m","n_predict":7}`,
	} {
		r := request(t, "/v1/chat/completions", body)
		if capOutput(r, 2048) {
			t.Errorf("rewrote a request that already stated a limit: %s", body)
		}
		// And it is still readable, unchanged.
		b, _ := io.ReadAll(r.Body)
		if string(b) != body {
			t.Errorf("body changed:\n  in  %s\n  out %s", body, b)
		}
	}
}

// "max_tokens": null means unset, and it is what a client generated from an
// OpenAI schema sends.
func TestShimTreatsNullAsUnset(t *testing.T) {
	r := request(t, "/v1/chat/completions", `{"model":"m","max_tokens":null}`)
	if !capOutput(r, 512) {
		t.Fatal("null was read as a stated limit")
	}
	if got := bodyOf(t, r)["max_tokens"]; got != float64(512) {
		t.Errorf("max_tokens = %v, want 512", got)
	}
}

// Whatever happens, the proxy must still be able to read the body. A shim that
// consumed a request it decided not to rewrite would break every request it
// left alone.
func TestShimAlwaysLeavesTheBodyReadable(t *testing.T) {
	for _, body := range []string{`not json`, `{"model":"m"}`, `{"model":"m","max_tokens":5}`} {
		r := request(t, "/v1/chat/completions", body)
		capOutput(r, 1024)
		b, err := io.ReadAll(r.Body)
		if err != nil || len(b) == 0 {
			t.Errorf("body unreadable after capOutput for %q: %v", body, err)
		}
	}
}

func TestShimCapsOnlyGeneratingPaths(t *testing.T) {
	for path, want := range map[string]bool{
		"/v1/chat/completions":     true,
		"/v1/completions":          true,
		"/v1/messages":             true,
		"/v1/responses":            true,
		"/v1/embeddings":           false,
		"/metrics":                 false,
		"/v1/audio/transcriptions": false,
	} {
		if got := generates(path); got != want {
			t.Errorf("generates(%q) = %v, want %v", path, got, want)
		}
	}
	// Prefixed by whoever routed it: a scheduler passes paths through, but
	// the suffix is what identifies the call.
	if !generates("/openai/v1/chat/completions") {
		t.Error("a prefixed chat path should still be recognised")
	}
}

// Off by default, so an operator who wants the old behaviour gets it exactly.
func TestShimZeroCeilingChangesNothing(t *testing.T) {
	r := request(t, "/v1/chat/completions", `{"model":"m"}`)
	if capOutput(r, 0) {
		t.Error("a zero ceiling should not rewrite anything")
	}
}

// The shim can be dialled without the router first validating a model name.
// A JSON null must reach the engine intact instead of panicking in the shim.
func TestShimLeavesNonObjectsUntouched(t *testing.T) {
	for _, body := range []string{"null", " null \n", "[]", `"text"`, "42", "true"} {
		t.Run(body, func(t *testing.T) {
			r := request(t, "/v1/chat/completions", body)
			if capOutput(r, 128) {
				t.Error("non-object was rewritten")
			}
			got, err := io.ReadAll(r.Body)
			if err != nil || string(got) != body {
				t.Fatalf("body=%q, err=%v", got, err)
			}
		})
	}
}
