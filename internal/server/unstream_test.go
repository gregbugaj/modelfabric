package server

import (
	"encoding/json"
	"net/url"
	"strings"
	"testing"
)

func sse(payloads ...string) string {
	var b strings.Builder
	for _, p := range payloads {
		b.WriteString("data: " + p + "\n\n")
	}
	b.WriteString("data: [DONE]\n\n")
	return b.String()
}

func assemble(t *testing.T, stream string) map[string]any {
	t.Helper()
	out, err := assembleStream(strings.NewReader(stream), nil)
	if err != nil {
		t.Fatalf("assembleStream: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("assembled body is not JSON: %v\n%s", err, out)
	}
	return m
}

func firstMessage(t *testing.T, m map[string]any) map[string]any {
	t.Helper()
	ch, ok := m["choices"].([]any)
	if !ok || len(ch) == 0 {
		t.Fatalf("no choices in %v", m)
	}
	msg, ok := ch[0].(map[string]any)["message"].(map[string]any)
	if !ok {
		t.Fatalf("choice has no message: %v", ch[0])
	}
	return msg
}

// Only a request that can be reassembled may be upgraded. Anything else must
// reach the engine exactly as the caller wrote it.
func TestUnstreamableOnlyUpgradesWhatItUnderstands(t *testing.T) {
	cases := []struct {
		name, path, body string
		want             bool
	}{
		{"plain chat completion", "/v1/chat/completions", `{"model":"m","messages":[]}`, true},
		{"explicitly not streaming", "/v1/chat/completions", `{"model":"m","stream":false}`, true},
		{"already streaming", "/v1/chat/completions", `{"model":"m","stream":true}`, false},
		{"anthropic shape", "/v1/messages", `{"model":"m","messages":[]}`, false},
		{"embeddings", "/v1/embeddings", `{"model":"m","input":"x"}`, false},
		{"completions", "/v1/completions", `{"model":"m","prompt":"x"}`, false},
		{"unparsable", "/v1/chat/completions", `{not json`, false},
		{"empty", "/v1/chat/completions", ``, false},
		{"stream is not a bool", "/v1/chat/completions", `{"model":"m","stream":"yes"}`, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out, ok := unstreamable(c.path, []byte(c.body))
			if ok != c.want {
				t.Fatalf("upgraded = %v, want %v", ok, c.want)
			}
			if !ok {
				if string(out) != c.body {
					t.Errorf("a request that was not upgraded was altered:\n got %s\nwant %s", out, c.body)
				}
				return
			}
			var m map[string]any
			if err := json.Unmarshal(out, &m); err != nil {
				t.Fatalf("upgraded body is not JSON: %v", err)
			}
			if m["stream"] != true {
				t.Errorf("stream = %v, want true", m["stream"])
			}
			// Without include_usage the final chunk carries no token counts,
			// and a caller that had been getting them would silently stop.
			so, _ := m["stream_options"].(map[string]any)
			if so == nil || so["include_usage"] != true {
				t.Errorf("stream_options = %v, want include_usage true", m["stream_options"])
			}
			if m["model"] != "m" {
				t.Errorf("model was lost: %v", m["model"])
			}
		})
	}
}

func TestAssembleJoinsContentAndReasoning(t *testing.T) {
	m := assemble(t, sse(
		`{"id":"x","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"role":"assistant","reasoning_content":"think"}}]}`,
		`{"id":"x","choices":[{"index":0,"delta":{"reasoning_content":"ing"}}]}`,
		`{"id":"x","choices":[{"index":0,"delta":{"content":"Hello"}}]}`,
		`{"id":"x","choices":[{"index":0,"delta":{"content":" world"},"finish_reason":"stop"}]}`,
		`{"id":"x","choices":[],"usage":{"prompt_tokens":7,"completion_tokens":4,"total_tokens":11}}`,
	))
	if m["object"] != "chat.completion" {
		t.Errorf("object = %v, want chat.completion", m["object"])
	}
	// The envelope must survive: clients key off id and model.
	if m["id"] != "x" || m["model"] != "m" {
		t.Errorf("envelope lost: id=%v model=%v", m["id"], m["model"])
	}
	msg := firstMessage(t, m)
	if msg["content"] != "Hello world" {
		t.Errorf("content = %q, want %q", msg["content"], "Hello world")
	}
	if msg["reasoning_content"] != "thinking" {
		t.Errorf("reasoning = %q, want %q", msg["reasoning_content"], "thinking")
	}
	if msg["role"] != "assistant" {
		t.Errorf("role = %v, want assistant", msg["role"])
	}
	ch := m["choices"].([]any)[0].(map[string]any)
	if ch["finish_reason"] != "stop" {
		t.Errorf("finish_reason = %v, want stop", ch["finish_reason"])
	}
	u, ok := m["usage"].(map[string]any)
	if !ok || u["completion_tokens"] != float64(4) {
		t.Fatalf("usage = %v, want completion_tokens 4", m["usage"])
	}
}

// Tool call arguments arrive as fragments across chunks and are worthless
// unless joined in order.
func TestAssembleJoinsToolCalls(t *testing.T) {
	m := assemble(t, sse(
		`{"id":"x","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"run","arguments":"{\"cmd\""}}]}}]}`,
		`{"id":"x","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":":\"ls\"}"}}]}}]}`,
		`{"id":"x","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
	))
	msg := firstMessage(t, m)
	calls, ok := msg["tool_calls"].([]any)
	if !ok || len(calls) != 1 {
		t.Fatalf("tool_calls = %v, want one", msg["tool_calls"])
	}
	c := calls[0].(map[string]any)
	if c["id"] != "call_1" || c["type"] != "function" {
		t.Errorf("call identity lost: %v", c)
	}
	fn := c["function"].(map[string]any)
	if fn["name"] != "run" {
		t.Errorf("name = %v, want run", fn["name"])
	}
	if fn["arguments"] != `{"cmd":"ls"}` {
		t.Errorf("arguments = %q, want %q", fn["arguments"], `{"cmd":"ls"}`)
	}
	// OpenAI sends null, not "", when a reply is only tool calls, and callers
	// branch on it.
	if msg["content"] != nil {
		t.Errorf("content = %v, want null for a tool-call-only reply", msg["content"])
	}
}

func TestAssembleKeepsChoicesApart(t *testing.T) {
	m := assemble(t, sse(
		`{"id":"x","choices":[{"index":0,"delta":{"content":"first"}},{"index":1,"delta":{"content":"second"}}]}`,
		`{"id":"x","choices":[{"index":1,"delta":{"content":" more"}}]}`,
	))
	ch := m["choices"].([]any)
	if len(ch) != 2 {
		t.Fatalf("got %d choices, want 2", len(ch))
	}
	got := map[float64]string{}
	for _, c := range ch {
		cm := c.(map[string]any)
		got[cm["index"].(float64)] = cm["message"].(map[string]any)["content"].(string)
	}
	if got[0] != "first" || got[1] != "second more" {
		t.Errorf("choices crossed: %v", got)
	}
}

// The watcher is fed while the engine is still writing — that is the entire
// reason for upgrading the request.
func TestAssembleFeedsTheWatcherPerChunk(t *testing.T) {
	var seen []string
	_, err := assembleStream(strings.NewReader(sse(
		`{"id":"x","choices":[{"index":0,"delta":{"content":"a"}}]}`,
		`{"id":"x","choices":[{"index":0,"delta":{"content":"b"}}]}`,
	)), func(p []byte) { seen = append(seen, string(p)) })
	if err != nil {
		t.Fatalf("assembleStream: %v", err)
	}
	if len(seen) != 2 {
		t.Fatalf("watcher saw %d chunks, want 2", len(seen))
	}
	if !strings.Contains(seen[0], `"a"`) || !strings.Contains(seen[1], `"b"`) {
		t.Errorf("watcher saw the wrong chunks: %v", seen)
	}
}

// An error the engine reports mid-stream is its own to deliver. Reassembling
// it into an answer would turn a failure into a plausible wrong reply.
func TestAssemblePassesAnUpstreamErrorThrough(t *testing.T) {
	out, err := assembleStream(strings.NewReader(sse(
		`{"id":"x","choices":[{"index":0,"delta":{"content":"partial"}}]}`,
		`{"error":{"message":"context window exceeded","type":"invalid_request_error"}}`,
	)), nil)
	if err != nil {
		t.Fatalf("assembleStream: %v", err)
	}
	if !strings.Contains(string(out), "context window exceeded") {
		t.Fatalf("the engine's error did not survive: %s", out)
	}
}

func TestAssembleRefusesAnEmptyStream(t *testing.T) {
	if _, err := assembleStream(strings.NewReader("data: [DONE]\n\n"), nil); err == nil {
		t.Fatal("a stream with no chunks assembled into something")
	}
}

// End to end through the scheduler path: a non-streaming request must reach
// upstream as a streamed one, and come back to the caller as one JSON body.
func TestUpstreamUpgradesNonStreamingRequests(t *testing.T) {
	f := newFrontFixture(t, true, false)
	var gotUpstream string
	// An upstream that records the body it was sent and answers with an event
	// stream, as an engine does for a streamed request.
	up := newRecordingUpstream(t, &gotUpstream)
	u, err := url.Parse("http://" + up)
	if err != nil {
		t.Fatal(err)
	}
	f.srv.scheduler = func(string) (*url.URL, bool) { return u, true }

	rec := call(f.srv.FrontHandler(), testKey, "")
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if gotUpstream == "" {
		t.Fatal("upstream received no body")
	}
	var sent map[string]any
	if err := json.Unmarshal([]byte(gotUpstream), &sent); err != nil {
		t.Fatalf("upstream body is not JSON: %v\n%s", err, gotUpstream)
	}
	if sent["stream"] != true {
		t.Fatalf("upstream stream = %v, want true — the request was not upgraded", sent["stream"])
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("caller got something that is not JSON: %v\n%s", err, rec.Body.String())
	}
	if out["object"] != "chat.completion" {
		t.Errorf("caller got object %v, want chat.completion", out["object"])
	}
	msg := firstMessage(t, out)
	if msg["content"] != "hi there" {
		t.Errorf("content = %v, want %q", msg["content"], "hi there")
	}
}
