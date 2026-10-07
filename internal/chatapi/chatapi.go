// Package chatapi implements POST /api/v1/chat with conversation continuation
// and MCP tool execution. Model calls use the local inference front door
// for routing, scheduling, and accounting.
package chatapi

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/gregbugaj/modelfabric/internal/mcp"
)

// maxRounds bounds the tool loop. A model that keeps calling tools without
// answering would otherwise hold an engine slot until someone noticed.
const maxRounds = 16

// maxToolOutput is what one tool's output may add to the conversation. A
// page of HTML returned by a browser tool once filled a context on its own.
const maxToolOutput = 64 << 10

type Request struct {
	Model              string            `json:"model"`
	Input              json.RawMessage   `json:"input"`
	SystemPrompt       string            `json:"system_prompt"`
	Integrations       []json.RawMessage `json:"integrations"`
	Stream             bool              `json:"stream"`
	Temperature        *float64          `json:"temperature"`
	TopP               *float64          `json:"top_p"`
	TopK               *int              `json:"top_k"`
	MinP               *float64          `json:"min_p"`
	RepeatPenalty      *float64          `json:"repeat_penalty"`
	MaxOutputTokens    *int              `json:"max_output_tokens"`
	Reasoning          string            `json:"reasoning"`
	ContextLength      *int              `json:"context_length"`
	Store              *bool             `json:"store"`
	PreviousResponseID string            `json:"previous_response_id"`
}

type Item struct {
	Type      string          `json:"type"` // message | reasoning | tool_call | invalid_tool_call
	Content   string          `json:"content,omitempty"`
	Tool      string          `json:"tool,omitempty"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
	Output    *string         `json:"output,omitempty"`
	Provider  *Provider       `json:"provider_info,omitempty"`
	Reason    string          `json:"reason,omitempty"`
	Metadata  *Invalid        `json:"metadata,omitempty"`
}

type Provider struct {
	Type        string `json:"type"` // ephemeral_mcp | plugin
	ServerLabel string `json:"server_label,omitempty"`
	PluginID    string `json:"plugin_id,omitempty"`
}

type Invalid struct {
	Type      string    `json:"type"`
	ToolName  string    `json:"tool_name"`
	Arguments any       `json:"arguments,omitempty"`
	Provider  *Provider `json:"provider_info,omitempty"`
}

// Stats are the response's counters. A figure the engine did not report is
// left out rather than estimated.
type Stats struct {
	InputTokens           int      `json:"input_tokens"`
	TotalOutputTokens     int      `json:"total_output_tokens"`
	ReasoningOutputTokens *int     `json:"reasoning_output_tokens,omitempty"`
	TokensPerSecond       float64  `json:"tokens_per_second,omitempty"`
	TimeToFirstToken      float64  `json:"time_to_first_token_seconds"`
	ModelLoadTime         *float64 `json:"model_load_time_seconds,omitempty"`
}

// Response is the body answered, and the result of a stream's chat.end.
type Response struct {
	ModelInstanceID string `json:"model_instance_id"`
	Output          []Item `json:"output"`
	Stats           Stats  `json:"stats"`
	ResponseID      string `json:"response_id,omitempty"`
}

type Error struct {
	Status int
	Msg    string
}

func (e *Error) Error() string { return e.Msg }

func bad(format string, a ...any) *Error {
	return &Error{Status: http.StatusBadRequest, Msg: fmt.Sprintf(format, a...)}
}

type Runner struct {
	// Infer sends one /v1/chat/completions body through the front door and
	// returns its event stream and the node that is serving it.
	Infer func(ctx context.Context, body []byte) (stream io.ReadCloser, node string, err error)
	// InferResponses sends one /v1/responses body the same way, past the
	// Responses layer (responses.go), and returns the engine's answer.
	InferResponses func(ctx context.Context, body []byte) (*Upstream, error)
	Store          *Store
	// ResponseStore holds /v1/responses conversations. Kept apart from
	// Store: its items are Responses items, not chat messages, and an id
	// from one endpoint must not be continued on the other.
	ResponseStore *Store
	// AllowEphemeral and AllowConfigured are the two switches: MCP servers
	// named in the request, and those in mcp.json. Both are off unless the
	// node's owner turned them on, since either has this node reach out, or
	// start a process, on a caller's say-so.
	AllowEphemeral  func() bool
	AllowConfigured func() bool
	// Servers reads mcp.json, each time: it is edited while the node runs.
	Servers func() (map[string]mcp.Server, error)
	// Load loads the model when nothing in the mesh serves it and loading on
	// demand is on, with the context length the request asked for. loaded is
	// false when there was nothing to do: the model is already served, or
	// this node does not load on demand, and the request goes on as it would
	// have. started is called once a load is certain, before waiting for
	// it. nil on a node that never loads.
	Load func(ctx context.Context, model string, contextLength int, started func()) (loaded bool, err error)
	// Dial opens a session with a server, whether named in the request or
	// read from mcp.json. Tests replace it.
	Dial func(ctx context.Context, s mcp.Server) (mcp.Client, error)
}

// Emit receives a stream's events as they happen; nil when not streaming.
type Emit func(event string, data any)

type tool struct {
	client   mcp.Client
	provider Provider
	def      mcp.Tool
}

// Run answers one request. node is the machine that served the last model
// call, empty when that is not known.
func (r *Runner) Run(ctx context.Context, req Request, emit Emit) (resp Response, node string, err error) {
	if emit == nil {
		emit = func(string, any) {}
	}
	if req.Model == "" {
		return resp, "", bad("name the model: {\"model\": ..., \"input\": ...}")
	}
	user, err := userMessage(req.Input)
	if err != nil {
		return resp, "", err
	}
	var messages []map[string]any
	if req.PreviousResponseID != "" {
		prev, ok := r.Store.Get(req.PreviousResponseID)
		if !ok {
			return resp, "", &Error{Status: http.StatusNotFound, Msg: fmt.Sprintf(
				"no stored response %q on this node: it was never stored here, has expired, or the node restarted; send the conversation again without previous_response_id", req.PreviousResponseID)}
		}
		messages = prev
	}
	if req.SystemPrompt != "" {
		// A system prompt applies to this request: it replaces an earlier
		// one rather than stacking a second.
		if len(messages) > 0 && messages[0]["role"] == "system" {
			messages = messages[1:]
		}
		messages = append([]map[string]any{{"role": "system", "content": req.SystemPrompt}}, messages...)
	}
	messages = append(messages, user)

	tools, closeAll, err := r.tools(ctx, req.Integrations)
	defer closeAll()
	if err != nil {
		return resp, "", err
	}

	resp.ModelInstanceID = req.Model
	resp.Output = []Item{}
	emit("chat.start", map[string]any{"type": "chat.start", "model_instance_id": req.Model})
	if r.Load != nil {
		// context_length is a load setting: it can only be honoured when
		// this request is what loads the model.
		ctxLen := 0
		if req.ContextLength != nil {
			ctxLen = *req.ContextLength
		}
		began := time.Now()
		loaded, err := r.Load(ctx, req.Model, ctxLen, func() {
			emit("model_load.start", map[string]any{"type": "model_load.start", "model_instance_id": req.Model})
		})
		if err != nil {
			return resp, "", &Error{Status: http.StatusServiceUnavailable, Msg: fmt.Sprintf("loading %q on demand failed: %v", req.Model, err)}
		}
		if loaded {
			secs := round3(time.Since(began).Seconds())
			resp.Stats.ModelLoadTime = &secs
			emit("model_load.end", map[string]any{"type": "model_load.end", "model_instance_id": req.Model, "load_time_seconds": secs})
		}
	}
	start := time.Now()
	var predictedMs float64
	for round := 0; ; round++ {
		if round == maxRounds {
			return resp, node, &Error{Status: http.StatusBadGateway, Msg: fmt.Sprintf(
				"the model called tools %d times in a row without answering; stopped", maxRounds)}
		}
		body := r.body(req, messages, tools)
		emit("prompt_processing.start", map[string]any{"type": "prompt_processing.start"})
		stream, n, err := r.Infer(ctx, body)
		if err != nil {
			return resp, node, err
		}
		if n != "" {
			node = n
		}
		t, err := readTurn(stream, emit, func() {
			if resp.Stats.TimeToFirstToken == 0 {
				resp.Stats.TimeToFirstToken = round3(time.Since(start).Seconds())
			}
		})
		stream.Close()
		if err != nil {
			return resp, node, err
		}
		resp.Stats.InputTokens = t.promptTokens
		resp.Stats.TotalOutputTokens += t.outTokens
		predictedMs += t.predictedMs
		if t.reasoningTokens != nil {
			sum := *t.reasoningTokens
			if resp.Stats.ReasoningOutputTokens != nil {
				sum += *resp.Stats.ReasoningOutputTokens
			}
			resp.Stats.ReasoningOutputTokens = &sum
		}
		if t.reasoning != "" {
			resp.Output = append(resp.Output, Item{Type: "reasoning", Content: t.reasoning})
		}
		if t.content != "" {
			resp.Output = append(resp.Output, Item{Type: "message", Content: t.content})
		}
		if len(t.calls) == 0 {
			messages = append(messages, map[string]any{"role": "assistant", "content": t.content})
			break
		}
		assistant := map[string]any{"role": "assistant", "content": t.content, "tool_calls": t.wire()}
		messages = append(messages, assistant)
		for _, c := range t.calls {
			item, result := r.callTool(ctx, c, tools, emit)
			resp.Output = append(resp.Output, item)
			messages = append(messages, map[string]any{"role": "tool", "tool_call_id": c.id, "content": result})
		}
	}
	if predictedMs > 0 {
		resp.Stats.TokensPerSecond = float64(resp.Stats.TotalOutputTokens) / (predictedMs / 1000)
	}
	if req.Store == nil || *req.Store {
		resp.ResponseID = r.Store.Put(req.Model, messages)
	}
	emit("chat.end", map[string]any{"type": "chat.end", "result": resp})
	return resp, node, nil
}

func round3(v float64) float64 { return float64(int(v*1000+0.5)) / 1000 }

func userMessage(raw json.RawMessage) (map[string]any, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, bad("input is missing: send the user's message as \"input\"")
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return map[string]any{"role": "user", "content": text}, nil
	}
	var parts []struct {
		Type    string `json:"type"`
		Content string `json:"content"`
		DataURL string `json:"data_url"`
	}
	if err := json.Unmarshal(raw, &parts); err != nil || len(parts) == 0 {
		return nil, bad("input must be a string, or a list of {\"type\": \"text\", \"content\"} and {\"type\": \"image\", \"data_url\"} parts")
	}
	var out []map[string]any
	for _, p := range parts {
		switch p.Type {
		case "text", "message":
			out = append(out, map[string]any{"type": "text", "text": p.Content})
		case "image":
			out = append(out, map[string]any{"type": "image_url", "image_url": map[string]string{"url": p.DataURL}})
		default:
			return nil, bad("input part type %q is not known: use text or image", p.Type)
		}
	}
	return map[string]any{"role": "user", "content": out}, nil
}

func (r *Runner) tools(ctx context.Context, integrations []json.RawMessage) (map[string]tool, func(), error) {
	var clients []mcp.Client
	closeAll := func() {
		for _, c := range clients {
			c.Close()
		}
	}
	out := map[string]tool{}
	for _, raw := range integrations {
		var in struct {
			Type         string            `json:"type"`
			ID           string            `json:"id"`
			ServerLabel  string            `json:"server_label"`
			ServerURL    string            `json:"server_url"`
			AllowedTools []string          `json:"allowed_tools"`
			Headers      map[string]string `json:"headers"`
		}
		// "mcp/playwright" on its own is a plugin with every tool allowed.
		if err := json.Unmarshal(raw, &in.ID); err == nil {
			in.Type = "plugin"
		} else if err := json.Unmarshal(raw, &in); err != nil {
			return nil, closeAll, bad("an integration must be a plugin id or an object: %v", err)
		}
		var (
			c        mcp.Client
			err      error
			provider Provider
			what     string
		)
		switch in.Type {
		case "ephemeral_mcp":
			if !r.AllowEphemeral() {
				return nil, closeAll, &Error{Status: http.StatusForbidden, Msg: "MCP servers named in a request are switched off on this node; turn on \"Allow per-request MCPs\" in Server settings (mcp_allow_ephemeral)"}
			}
			if in.ServerURL == "" || in.ServerLabel == "" {
				return nil, closeAll, bad("an ephemeral_mcp integration needs server_label and server_url")
			}
			provider, what = Provider{Type: "ephemeral_mcp", ServerLabel: in.ServerLabel}, in.ServerLabel
			c, err = r.Dial(ctx, mcp.Server{URL: in.ServerURL, Headers: in.Headers})
		case "plugin":
			if !r.AllowConfigured() {
				return nil, closeAll, &Error{Status: http.StatusForbidden, Msg: "MCP servers from mcp.json are switched off on this node; turn on \"Allow calling servers from mcp.json\" in Server settings (mcp_allow_configured)"}
			}
			name, ok := strings.CutPrefix(in.ID, "mcp/")
			if !ok {
				return nil, closeAll, bad("plugin %q is not known: only MCP servers from mcp.json are available, as \"mcp/<name>\"", in.ID)
			}
			servers, lerr := r.Servers()
			if lerr != nil {
				return nil, closeAll, &Error{Status: http.StatusInternalServerError, Msg: lerr.Error()}
			}
			s, ok := servers[name]
			if !ok {
				return nil, closeAll, bad("mcp.json has no server %q; it has: %s", name, or(strings.Join(mcp.Names(servers), ", "), "none"))
			}
			provider, what = Provider{Type: "plugin", PluginID: in.ID}, in.ID
			c, err = r.Dial(ctx, s)
		default:
			return nil, closeAll, bad("integration type %q is not known: use ephemeral_mcp or plugin", in.Type)
		}
		if err != nil {
			return nil, closeAll, &Error{Status: http.StatusBadGateway, Msg: fmt.Sprintf("could not reach MCP server %s: %v", what, err)}
		}
		clients = append(clients, c)
		defs, err := c.ListTools(ctx)
		if err != nil {
			return nil, closeAll, &Error{Status: http.StatusBadGateway, Msg: fmt.Sprintf("MCP server %s did not list its tools: %v", what, err)}
		}
		allowed := map[string]bool{}
		for _, a := range in.AllowedTools {
			allowed[a] = true
		}
		// Report allowed_tools entries absent from the server; otherwise a renamed
		// tool silently leaves the model without the requested capability.
		offered := map[string]bool{}
		for _, d := range defs {
			offered[d.Name] = true
		}
		for _, a := range in.AllowedTools {
			if !offered[a] {
				names := make([]string, 0, len(defs))
				for _, d := range defs {
					names = append(names, d.Name)
				}
				return nil, closeAll, bad("MCP server %s has no tool %q; it offers: %s", what, a, or(strings.Join(names, ", "), "none"))
			}
		}
		for _, d := range defs {
			if len(allowed) > 0 && !allowed[d.Name] {
				continue
			}
			if _, dup := out[d.Name]; dup {
				return nil, closeAll, bad("two integrations offer a tool named %q; use allowed_tools to pick one", d.Name)
			}
			p := provider
			out[d.Name] = tool{client: c, provider: p, def: d}
		}
	}
	return out, closeAll, nil
}

func sortStrings(s []string) { sort.Strings(s) }

func or(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func (r *Runner) body(req Request, messages []map[string]any, tools map[string]tool) []byte {
	b := map[string]any{
		"model":          req.Model,
		"messages":       messages,
		"stream":         true,
		"stream_options": map[string]any{"include_usage": true},
	}
	set := func(k string, v any, ok bool) {
		if ok {
			b[k] = v
		}
	}
	set("temperature", req.Temperature, req.Temperature != nil)
	set("top_p", req.TopP, req.TopP != nil)
	set("top_k", req.TopK, req.TopK != nil)
	set("min_p", req.MinP, req.MinP != nil)
	set("repeat_penalty", req.RepeatPenalty, req.RepeatPenalty != nil)
	set("max_tokens", req.MaxOutputTokens, req.MaxOutputTokens != nil)
	switch req.Reasoning {
	case "off":
		b["chat_template_kwargs"] = map[string]any{"enable_thinking": false}
	case "on":
		b["chat_template_kwargs"] = map[string]any{"enable_thinking": true}
	case "low", "medium", "high":
		b["chat_template_kwargs"] = map[string]any{"enable_thinking": true, "reasoning_effort": req.Reasoning}
	}
	if len(tools) > 0 {
		names := make([]string, 0, len(tools))
		for n := range tools {
			names = append(names, n)
		}
		// Sorted: the tool list is part of the prompt, and an order that
		// changed between requests would defeat the prompt cache.
		sort.Strings(names)
		var defs []map[string]any
		for _, n := range names {
			d := tools[n].def
			params := d.InputSchema
			if len(params) == 0 {
				params = json.RawMessage(`{"type":"object","properties":{}}`)
			}
			defs = append(defs, map[string]any{"type": "function", "function": map[string]any{
				"name": d.Name, "description": d.Description, "parameters": params}})
		}
		b["tools"] = defs
	}
	out, _ := json.Marshal(b)
	return out
}

type call struct {
	id, name, args string
}

type turn struct {
	reasoning, content string
	calls              []call
	promptTokens       int
	outTokens          int
	reasoningTokens    *int
	predictedMs        float64
}

func (t turn) wire() []map[string]any {
	out := make([]map[string]any, len(t.calls))
	for i, c := range t.calls {
		out[i] = map[string]any{"id": c.id, "type": "function", "function": map[string]any{"name": c.name, "arguments": c.args}}
	}
	return out
}

func readTurn(stream io.Reader, emit Emit, first func()) (turn, error) {
	var t turn
	state := "" // which of reasoning / message is open in the stream
	open := func(s string) {
		if state == s {
			return
		}
		if state != "" {
			emit(state+".end", map[string]any{"type": state + ".end"})
		}
		if state == "" {
			emit("prompt_processing.end", map[string]any{"type": "prompt_processing.end"})
		}
		if state = s; s != "" && s != "done" {
			emit(s+".start", map[string]any{"type": s + ".start"})
		}
	}
	sc := bufio.NewScanner(stream)
	sc.Buffer(make([]byte, 64<<10), 8<<20)
	for sc.Scan() {
		line, ok := strings.CutPrefix(sc.Text(), "data: ")
		if !ok || line == "[DONE]" {
			continue
		}
		var ev struct {
			Choices []struct {
				Delta struct {
					Content   string `json:"content"`
					Reasoning string `json:"reasoning_content"`
					ToolCalls []struct {
						Index    int    `json:"index"`
						ID       string `json:"id"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
			} `json:"choices"`
			Usage *struct {
				Prompt     int `json:"prompt_tokens"`
				Completion int `json:"completion_tokens"`
				Details    *struct {
					Reasoning *int `json:"reasoning_tokens"`
				} `json:"completion_tokens_details"`
			} `json:"usage"`
			Timings *struct {
				PredictedMs float64 `json:"predicted_ms"`
			} `json:"timings"`
			Error *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal([]byte(line), &ev) != nil {
			continue
		}
		if ev.Error != nil {
			return t, &Error{Status: http.StatusBadGateway, Msg: ev.Error.Message}
		}
		if ev.Usage != nil {
			t.promptTokens, t.outTokens = ev.Usage.Prompt, ev.Usage.Completion
			if ev.Usage.Details != nil {
				t.reasoningTokens = ev.Usage.Details.Reasoning
			}
		}
		if ev.Timings != nil {
			t.predictedMs = ev.Timings.PredictedMs
		}
		for _, c := range ev.Choices {
			d := c.Delta
			if d.Reasoning != "" {
				first()
				open("reasoning")
				t.reasoning += d.Reasoning
				emit("reasoning.delta", map[string]any{"type": "reasoning.delta", "content": d.Reasoning})
			}
			if d.Content != "" {
				first()
				open("message")
				t.content += d.Content
				emit("message.delta", map[string]any{"type": "message.delta", "content": d.Content})
			}
			for _, tc := range d.ToolCalls {
				first()
				for len(t.calls) <= tc.Index {
					t.calls = append(t.calls, call{})
				}
				if tc.Index < 0 {
					continue
				}
				c := &t.calls[tc.Index]
				if tc.ID != "" {
					c.id = tc.ID
				}
				c.name += tc.Function.Name
				c.args += tc.Function.Arguments
			}
		}
	}
	open("done")
	if err := sc.Err(); err != nil {
		return t, err
	}
	return t, nil
}

// callTool runs one call the model asked for and returns its output item and
// the text handed back to the model. A call that cannot be run is reported to
// the model too, so it can correct itself instead of the request failing.
func (r *Runner) callTool(ctx context.Context, c call, tools map[string]tool, emit Emit) (Item, string) {
	invalid := func(reason string, args any, p *Provider) (Item, string) {
		meta := &Invalid{Type: "invalid_tool_call", ToolName: c.name, Arguments: args, Provider: p}
		emit("tool_call.failure", map[string]any{"type": "tool_call.failure", "reason": reason, "metadata": meta})
		return Item{Type: "invalid_tool_call", Reason: reason, Metadata: meta}, "error: " + reason
	}
	t, ok := tools[c.name]
	if !ok {
		return invalid(fmt.Sprintf("the model called %q, which is not one of the tools offered", c.name), c.args, nil)
	}
	p := t.provider
	emit("tool_call.start", map[string]any{"type": "tool_call.start", "tool": c.name, "provider_info": p})
	args := json.RawMessage(strings.TrimSpace(c.args))
	if len(args) == 0 {
		args = json.RawMessage("{}")
	}
	if !json.Valid(args) {
		return invalid("the arguments were not valid JSON", c.args, &p)
	}
	emit("tool_call.arguments", map[string]any{"type": "tool_call.arguments", "tool": c.name, "arguments": args, "provider_info": p})
	out, err := t.client.CallTool(ctx, c.name, args)
	if err != nil {
		var e *Error
		if errors.As(err, &e) {
			err = errors.New(e.Msg)
		}
		return invalid(err.Error(), args, &p)
	}
	if len(out) > maxToolOutput {
		out = out[:maxToolOutput] + fmt.Sprintf("\n[output cut at %d bytes of %d]", maxToolOutput, len(out))
	}
	emit("tool_call.success", map[string]any{"type": "tool_call.success", "tool": c.name, "arguments": args, "output": out, "provider_info": p})
	return Item{Type: "tool_call", Tool: c.name, Arguments: args, Output: &out, Provider: &p}, out
}
