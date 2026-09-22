package chatapi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gregbugaj/modelfabric/internal/mcp"
)

// fakeEngine answers /v1/responses as llama.cpp does: no memory, function
// tools only, and a response.created event with no output list.
type fakeEngine struct {
	bodies []map[string]any
	n      int
}

func (f *fakeEngine) infer(_ context.Context, body []byte) (*Upstream, error) {
	var req map[string]any
	json.Unmarshal(body, &req)
	f.bodies = append(f.bodies, req)
	f.n++
	id := "resp_" + string(rune('a'+f.n-1))
	if _, has := req["previous_response_id"]; has {
		return &Upstream{Status: 400, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"error":{"message":"llama.cpp does not support 'previous_response_id'."}}`))}, nil
	}
	input := req["input"].([]any)
	last := input[len(input)-1].(map[string]any)
	var output string
	if req["tools"] != nil && last["type"] != "function_call_output" {
		output = `[{"type":"function_call","id":"fc_1","call_id":"call_1","name":"get_weather","arguments":"{\"city\":\"Paris\"}","status":"completed"}]`
	} else {
		text := "saw " + string(rune('0'+len(input))) + " items"
		if last["type"] == "function_call_output" {
			text = "it is " + last["output"].(string)
		}
		output = `[{"type":"message","id":"msg_1","role":"assistant","status":"completed","content":[{"type":"output_text","text":"` + text + `"}]}]`
	}
	resp := `{"id":"` + id + `","object":"response","status":"completed","output":` + output + `,"usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15}}`
	if req["stream"] == true {
		sse := "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"" + id + "\",\"status\":\"in_progress\"}}\n\n" +
			"event: response.output_item.added\ndata: {\"type\":\"response.output_item.added\",\"item\":{\"id\":\"msg_1\",\"type\":\"message\"}}\n\n" +
			"event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"hi\",\"item_id\":\"msg_1\"}\n\n" +
			"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":" + resp + "}\n\n"
		return &Upstream{Status: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(sse))}, nil
	}
	return &Upstream{Status: 200, Header: http.Header{"Content-Type": {"application/json"}, "X-Fabric-Node": {"gpu-01"}}, Body: io.NopCloser(strings.NewReader(resp))}, nil
}

func responsesRunner(f *fakeEngine, m *fakeMCP, allow bool) *Runner {
	return &Runner{
		InferResponses:  f.infer,
		ResponseStore:   NewStore(10, time.Hour),
		AllowEphemeral:  func() bool { return allow },
		AllowConfigured: func() bool { return allow },
		Servers: func() (map[string]mcp.Server, error) {
			return map[string]mcp.Server{"weather": {URL: "http://unused"}}, nil
		},
		Dial: func(context.Context, mcp.Server) (mcp.Client, error) { return m, nil },
	}
}

func do(r *Runner, body string) (*httptest.ResponseRecorder, map[string]any) {
	rec := httptest.NewRecorder()
	r.Responses(context.Background(), []byte(body), rec)
	var out map[string]any
	json.Unmarshal(rec.Body.Bytes(), &out)
	return rec, out
}

func text(resp map[string]any) string {
	out := resp["output"].([]any)
	last := out[len(out)-1].(map[string]any)
	return last["content"].([]any)[0].(map[string]any)["text"].(string)
}

// llama.cpp answers previous_response_id with a 400. The layer keeps each
// response's items and puts them back in front of the follow-up, so the
// engine never sees the field.
func TestResponsesAreContinuedByID(t *testing.T) {
	f := &fakeEngine{}
	r := responsesRunner(f, nil, false)
	rec, first := do(r, `{"model":"m","input":"My colour is teal."}`)
	if rec.Code != 200 || text(first) != "saw 1 items" || rec.Header().Get("X-Fabric-Node") != "gpu-01" {
		t.Fatalf("first: %d %s", rec.Code, rec.Body)
	}
	_, second := do(r, `{"model":"m","input":"What is it?","previous_response_id":"resp_a"}`)
	if text(second) != "saw 3 items" {
		t.Fatalf("follow-up %s; want the question, the answer and the new question", text(second))
	}
	if _, sent := f.bodies[1]["previous_response_id"]; sent {
		t.Fatal("previous_response_id reached the engine, which refuses it")
	}
	// And the follow-up's own id continues in turn.
	_, third := do(r, `{"model":"m","input":"And again?","previous_response_id":"resp_b"}`)
	if text(third) != "saw 5 items" {
		t.Fatalf("third %s", text(third))
	}
}

func TestResponsesStoreRules(t *testing.T) {
	tests := []struct {
		name, first, follow string
		want                int
	}{
		{name: "store false keeps nothing, so it cannot be continued", first: `{"model":"m","input":"hi","store":false}`,
			follow: `{"model":"m","input":"x","previous_response_id":"resp_a"}`, want: 404},
		{name: "an unknown id is refused, not started afresh", first: `{"model":"m","input":"hi"}`,
			follow: `{"model":"m","input":"x","previous_response_id":"resp_zzz"}`, want: 404},
		{name: "an MCP server while the switch is off", first: `{"model":"m","input":"hi"}`,
			follow: `{"model":"m","input":"x","tools":[{"type":"mcp","server_label":"wx","server_url":"http://x"}]}`, want: 403},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := responsesRunner(&fakeEngine{}, &fakeMCP{}, false)
			do(r, tc.first)
			if rec, _ := do(r, tc.follow); rec.Code != tc.want {
				t.Fatalf("got %d %s, want %d", rec.Code, rec.Body, tc.want)
			}
		})
	}
}

// The OpenAI SDK's responses.stream() appends to response.output from the
// first event and crashed, because llama.cpp's response.created has none;
// past that it indexes each delta by output_index and content_index, which
// llama.cpp does not send either. The layer adds them, and the finished
// response is still stored.
func TestStreamGetsItsOutputListAndIsStored(t *testing.T) {
	r := responsesRunner(&fakeEngine{}, nil, false)
	rec, _ := do(r, `{"model":"m","input":"hi","stream":true}`)
	body := rec.Body.String()
	if !strings.Contains(body, `"output":[]`) || !strings.Contains(body, `"response.created"`) {
		t.Fatalf("response.created has no output list:\n%s", body)
	}
	// The helper indexes by these, and llama.cpp sends none of them.
	for _, want := range []string{`"delta":"hi"`, `"item_id":"msg_1"`, `"output_index":0`, `"content_index":0`, `"sequence_number":2`} {
		if !strings.Contains(body, want) {
			t.Fatalf("the stream has no %s:\n%s", want, body)
		}
	}
	if _, second := do(r, `{"model":"m","input":"more","previous_response_id":"resp_a"}`); text(second) != "saw 3 items" {
		t.Fatalf("a streamed response was not stored: %v", second)
	}
}

// The engine knows function tools only. An MCP server's tools are offered as
// functions, run here, and reported as mcp_call items; the caller is never
// asked to run them.
func TestResponsesRunMCPTools(t *testing.T) {
	const tools = `[{"type":"mcp","server_label":"wx","server_url":"http://x","allowed_tools":["get_weather"]}]`
	tests := []struct {
		name   string
		stream bool
	}{{name: "as one JSON body"}, {name: "as a stream", stream: true}}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f, m := &fakeEngine{}, &fakeMCP{}
			r := responsesRunner(f, m, true)
			body := `{"model":"m","input":"Weather in Paris?","tools":` + tools
			if tc.stream {
				body += `,"stream":true`
			}
			rec, resp := do(r, body+`}`)
			if rec.Code != 200 || len(m.calls) != 1 {
				t.Fatalf("%d %s, tool ran %d times", rec.Code, rec.Body, len(m.calls))
			}
			if tc.stream {
				s := rec.Body.String()
				for _, want := range []string{"event: response.created", `"type":"mcp_call"`, "event: response.output_text.delta", `"delta":"it is 18C and clear"`, "event: response.completed"} {
					if !strings.Contains(s, want) {
						t.Errorf("stream has no %s:\n%s", want, s)
					}
				}
				return
			}
			var kinds []string
			for _, it := range resp["output"].([]any) {
				kinds = append(kinds, it.(map[string]any)["type"].(string))
			}
			if strings.Join(kinds, ",") != "mcp_list_tools,mcp_call,message" {
				t.Fatalf("output %v", kinds)
			}
			call := resp["output"].([]any)[1].(map[string]any)
			if call["server_label"] != "wx" || call["name"] != "get_weather" || call["output"] != "18C and clear" {
				t.Fatalf("mcp_call %v", call)
			}
			if text(resp) != "it is 18C and clear" {
				t.Fatalf("answer %q", text(resp))
			}
			offered := f.bodies[0]["tools"].([]any)
			if len(offered) != 1 || offered[0].(map[string]any)["type"] != "function" {
				t.Fatalf("the engine was offered %v; want the one allowed tool as a function", offered)
			}
			// Continuing it replays what the engine needs: its own call and
			// the result, not the mcp_call shown to the caller.
			_, next := do(r, `{"model":"m","input":"Thanks","previous_response_id":"resp_b"}`)
			if text(next) != "saw 5 items" {
				t.Fatalf("continued: %s", text(next))
			}
		})
	}
}
