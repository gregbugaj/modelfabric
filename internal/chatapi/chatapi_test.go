package chatapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/gregbugaj/modelfabric/internal/mcp"
)

// fakeModel answers like an engine's event stream. Asked about the weather
// with a tool on offer it calls the tool; given a tool result it answers from
// it; otherwise it reports how many messages it was sent.
type fakeModel struct {
	bodies  []map[string]any
	loop    bool
	badArgs bool
}

func sse(chunks ...string) io.ReadCloser {
	var b strings.Builder
	for _, c := range chunks {
		fmt.Fprintf(&b, "data: %s\n\n", c)
	}
	b.WriteString("data: [DONE]\n\n")
	return io.NopCloser(strings.NewReader(b.String()))
}

func (f *fakeModel) infer(_ context.Context, body []byte) (io.ReadCloser, string, error) {
	var req map[string]any
	json.Unmarshal(body, &req)
	f.bodies = append(f.bodies, req)
	msgs := req["messages"].([]any)
	last := msgs[len(msgs)-1].(map[string]any)
	usage := `{"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":5},"timings":{"predicted_ms":100}}`
	if req["tools"] != nil && (last["role"] == "user" || f.loop) {
		args := `{\"city\":\"Paris\"}`
		if f.badArgs && last["role"] == "user" {
			args = `{city`
		}
		return sse(
			`{"choices":[{"delta":{"reasoning_content":"need the weather"}}]}`,
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","function":{"name":"get_weather","arguments":""}}]}}]}`,
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"`+args+`"}}]}}]}`,
			usage), "gpu-01", nil
	}
	text := fmt.Sprintf("%d messages", len(msgs))
	if last["role"] == "tool" {
		text = "it is " + last["content"].(string)
	}
	return sse(`{"choices":[{"delta":{"content":"`+text[:2]+`"}}]}`, `{"choices":[{"delta":{"content":"`+text[2:]+`"}}]}`, usage), "gpu-01", nil
}

type fakeMCP struct{ calls []string }

func (m *fakeMCP) ListTools(context.Context) ([]mcp.Tool, error) {
	return []mcp.Tool{{Name: "get_weather"}, {Name: "delete_everything"}}, nil
}
func (m *fakeMCP) CallTool(_ context.Context, name string, args json.RawMessage) (string, error) {
	m.calls = append(m.calls, name+string(args))
	return "18C and clear", nil
}
func (m *fakeMCP) Close() error { return nil }

func runner(f *fakeModel, m *fakeMCP, ephemeral, configured bool) *Runner {
	return &Runner{
		Infer:           f.infer,
		Store:           NewStore(10, time.Hour),
		AllowEphemeral:  func() bool { return ephemeral },
		AllowConfigured: func() bool { return configured },
		Servers: func() (map[string]mcp.Server, error) {
			return map[string]mcp.Server{"weather": {URL: "http://unused"}}, nil
		},
		Dial: func(context.Context, mcp.Server) (mcp.Client, error) { return m, nil },
	}
}

func req(t *testing.T, s string) Request {
	t.Helper()
	var r Request
	if err := json.Unmarshal([]byte(s), &r); err != nil {
		t.Fatal(err)
	}
	return r
}

// A follow-up names the response it continues and sends only the new
// message; the model must still be given everything said before.
func TestAConversationIsContinuedByItsResponseID(t *testing.T) {
	f := &fakeModel{}
	r := runner(f, nil, false, false)
	first, node, err := r.Run(context.Background(), req(t, `{"model":"m","input":"My colour is blue.","system_prompt":"Be brief."}`), nil)
	if err != nil || node != "gpu-01" || !strings.HasPrefix(first.ResponseID, "resp_") {
		t.Fatalf("first: %+v node %q err %v", first, node, err)
	}
	if got := first.Output[0]; got.Type != "message" || got.Content != "2 messages" {
		t.Fatalf("first output %+v", first.Output)
	}
	second, _, err := r.Run(context.Background(), req(t, `{"model":"m","input":"What is it?","previous_response_id":"`+first.ResponseID+`"}`), nil)
	if err != nil || second.Output[0].Content != "4 messages" {
		t.Fatalf("second: %+v %v; want system, user, assistant, user", second.Output, err)
	}
	branch, _, _ := r.Run(context.Background(), req(t, `{"model":"m","input":"Another way.","previous_response_id":"`+first.ResponseID+`"}`), nil)
	if branch.Output[0].Content != "4 messages" {
		t.Fatalf("branch: %+v", branch.Output)
	}
}

func TestStoreAndItsRefusals(t *testing.T) {
	tests := []struct {
		name, body string
		allow      bool
		status     int
		check      func(t *testing.T, resp Response, r *Runner)
	}{
		{name: "store false keeps nothing and gives no id", body: `{"model":"m","input":"hi","store":false}`,
			check: func(t *testing.T, resp Response, r *Runner) {
				if resp.ResponseID != "" || r.Store.Len() != 0 {
					t.Fatalf("id %q, %d stored", resp.ResponseID, r.Store.Len())
				}
			}},
		{name: "an unknown previous_response_id is refused, not started afresh", body: `{"model":"m","input":"hi","previous_response_id":"resp_nope"}`, status: 404},
		{name: "no model", body: `{"input":"hi"}`, status: 400},
		{name: "no input", body: `{"model":"m"}`, status: 400},
		{name: "an ephemeral server while the switch is off", body: `{"model":"m","input":"hi","integrations":[{"type":"ephemeral_mcp","server_label":"x","server_url":"http://x"}]}`, status: 403},
		{name: "allowed_tools naming a tool the server lacks is refused, not ignored", allow: true,
			body: `{"model":"m","input":"hi","integrations":[{"type":"plugin","id":"mcp/weather","allowed_tools":["model_search"]}]}`, status: 400},
		{name: "an mcp.json server while the switch is off", body: `{"model":"m","input":"hi","integrations":["mcp/weather"]}`, status: 403},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := &fakeMCP{}
			r := runner(&fakeModel{}, m, tc.allow, tc.allow)
			resp, _, err := r.Run(context.Background(), req(t, tc.body), nil)
			var e *Error
			if tc.status != 0 {
				if !errors.As(err, &e) || e.Status != tc.status {
					t.Fatalf("got %v, want status %d", err, tc.status)
				}
				if len(m.calls) > 0 {
					t.Fatal("a refused request still reached the MCP server")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			tc.check(t, resp, r)
		})
	}
}

func TestToolsAreRunForTheModel(t *testing.T) {
	tests := []struct {
		name, integration string
		provider          Provider
	}{
		{name: "from a server named in the request", integration: `{"type":"ephemeral_mcp","server_label":"wx","server_url":"http://x","allowed_tools":["get_weather"]}`,
			provider: Provider{Type: "ephemeral_mcp", ServerLabel: "wx"}},
		{name: "from mcp.json, by id", integration: `"mcp/weather"`, provider: Provider{Type: "plugin", PluginID: "mcp/weather"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f, m := &fakeModel{}, &fakeMCP{}
			var events []string
			resp, _, err := runner(f, m, true, true).Run(context.Background(),
				req(t, `{"model":"m","input":"Weather in Paris?","integrations":[`+tc.integration+`]}`),
				func(ev string, _ any) { events = append(events, ev) })
			if err != nil {
				t.Fatal(err)
			}
			var kinds []string
			for _, it := range resp.Output {
				kinds = append(kinds, it.Type)
			}
			if strings.Join(kinds, ",") != "reasoning,tool_call,message" {
				t.Fatalf("output %v", kinds)
			}
			call := resp.Output[1]
			if call.Tool != "get_weather" || string(call.Arguments) != `{"city":"Paris"}` || *call.Output != "18C and clear" || *call.Provider != tc.provider {
				t.Fatalf("tool_call %+v provider %+v", call, call.Provider)
			}
			if resp.Output[2].Content != "it is 18C and clear" {
				t.Fatalf("answer %q", resp.Output[2].Content)
			}
			if len(m.calls) != 1 {
				t.Fatalf("tool ran %d times", len(m.calls))
			}
			if resp.Stats.TotalOutputTokens != 10 || resp.Stats.InputTokens != 10 {
				t.Fatalf("stats %+v; output tokens add across turns, input is the last turn's", resp.Stats)
			}
			got := strings.Join(events, " ")
			for _, want := range []string{"chat.start", "reasoning.start", "reasoning.delta", "reasoning.end", "tool_call.start", "tool_call.arguments", "tool_call.success", "message.start", "message.delta", "message.end", "chat.end"} {
				if !strings.Contains(got, want) {
					t.Errorf("no %s event in: %s", want, got)
				}
			}
			if strings.Contains(tc.integration, "allowed_tools") {
				offered := f.bodies[0]["tools"].([]any)
				if len(offered) != 1 {
					t.Fatalf("allowed_tools did not narrow the tools: %d offered", len(offered))
				}
			}
		})
	}
}

func TestAToolCallThatCannotRunIsToldToTheModel(t *testing.T) {
	f, m := &fakeModel{badArgs: true}, &fakeMCP{}
	resp, _, err := runner(f, m, true, true).Run(context.Background(), req(t, `{"model":"m","input":"Weather?","integrations":["mcp/weather"]}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	if it := resp.Output[1]; it.Type != "invalid_tool_call" || !strings.Contains(it.Reason, "not valid JSON") {
		t.Fatalf("got %+v", it)
	}
	if len(m.calls) != 0 {
		t.Fatal("a call with unreadable arguments reached the server")
	}
	if last := resp.Output[len(resp.Output)-1]; !strings.Contains(last.Content, "error: the arguments were not valid JSON") {
		t.Fatalf("the model was not told: %q", last.Content)
	}
}

// A model that never stops calling tools must not hold an engine for ever.
func TestTheToolLoopIsBounded(t *testing.T) {
	f, m := &fakeModel{loop: true}, &fakeMCP{}
	_, _, err := runner(f, m, true, true).Run(context.Background(), req(t, `{"model":"m","input":"Weather?","integrations":["mcp/weather"]}`), nil)
	var e *Error
	if !errors.As(err, &e) || !strings.Contains(e.Msg, "16 times") || len(m.calls) != maxRounds {
		t.Fatalf("err %v after %d calls", err, len(m.calls))
	}
}

func TestStoreForgets(t *testing.T) {
	s := NewStore(2, time.Hour)
	now := time.Unix(0, 0)
	s.now = func() time.Time { return now }
	a := s.Put("m", nil)
	now = now.Add(time.Minute)
	b := s.Put("m", nil)
	now = now.Add(time.Minute)
	c := s.Put("m", nil)
	if _, ok := s.Get(a); ok {
		t.Fatal("the oldest conversation was kept past the limit")
	}
	now = now.Add(time.Hour)
	if _, ok := s.Get(b); ok {
		t.Fatal("a conversation was kept past its lifetime")
	}
	if _, ok := s.Get(c); !ok {
		t.Fatal("a live conversation was dropped")
	}
}

// context_length is a load setting, so it is honoured when this request is
// what loads the model, and the load is reported: start, end, and the time in
// the stats. A model already served loads nothing and says nothing.
func TestLoadingOnDemandIsReportedAndTakesTheContext(t *testing.T) {
	tests := []struct {
		name   string
		served bool
	}{{name: "nothing serves the model, so it is loaded"}, {name: "the model is already served", served: true}}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := runner(&fakeModel{}, nil, false, false)
			gotCtx := -1
			r.Load = func(_ context.Context, _ string, n int, started func()) (bool, error) {
				if tc.served {
					return false, nil
				}
				gotCtx = n
				started()
				return true, nil
			}
			var events []string
			resp, _, err := r.Run(context.Background(), req(t, `{"model":"m","input":"hi","context_length":8000}`),
				func(ev string, _ any) { events = append(events, ev) })
			if err != nil {
				t.Fatal(err)
			}
			got := strings.Join(events, " ")
			loaded := strings.Contains(got, "chat.start model_load.start model_load.end prompt_processing.start")
			if loaded == tc.served || (resp.Stats.ModelLoadTime != nil) == tc.served {
				t.Fatalf("events: %s; load time %v", got, resp.Stats.ModelLoadTime)
			}
			if !tc.served && gotCtx != 8000 {
				t.Fatalf("the load was given context %d, want 8000", gotCtx)
			}
		})
	}
}
