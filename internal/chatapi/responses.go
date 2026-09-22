package chatapi

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// The Responses layer: what /v1/responses needs that the engine does not do.
//
//   - previous_response_id. llama.cpp keeps no conversation and answers 400.
//     Each response's items are stored here, and a follow-up is sent to the
//     engine with them put back in front of the new input.
//   - MCP tools ("type": "mcp"). The engine only knows function tools, so the
//     server's tools are offered as functions, and the calls the model makes
//     are run here and reported as mcp_call items.
//   - response.created without an output list. The OpenAI SDK's stream helper
//     appends to it and crashed on the first event; it is added.
//
// A request that needs none of this is passed through as it arrives, byte for
// byte apart from that one patch: this is the path Codex streams on.

// Upstream is the engine's answer to one /v1/responses call.
type Upstream struct {
	Status int
	Header http.Header
	Body   io.ReadCloser
}

// Responses answers POST /v1/responses.
func (r *Runner) Responses(ctx context.Context, raw []byte, w http.ResponseWriter) {
	var body map[string]json.RawMessage
	if err := json.Unmarshal(raw, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "the request body is not JSON: "+err.Error())
		return
	}
	var model string
	json.Unmarshal(body["model"], &model)
	stream := string(bytes.TrimSpace(body["stream"])) == "true"
	store := string(bytes.TrimSpace(body["store"])) != "false"
	// The engine is never asked to store: this layer does.
	delete(body, "store")

	input, err := inputItems(body["input"])
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	var prevID string
	json.Unmarshal(body["previous_response_id"], &prevID)
	if prevID != "" {
		prev, ok := r.ResponseStore.Get(prevID)
		if !ok {
			writeErr(w, http.StatusNotFound, fmt.Sprintf(
				"no stored response %q on this node: it was never stored here, has expired, or the node restarted; send the conversation again without previous_response_id", prevID))
			return
		}
		input = append(prev, input...)
		delete(body, "previous_response_id")
	}

	mcpSpecs, rest, err := splitTools(body["tools"])
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if len(mcpSpecs) > 0 {
		r.responsesWithTools(ctx, body, model, input, mcpSpecs, rest, stream, store, w)
		return
	}

	body["input"], _ = json.Marshal(input)
	out, _ := json.Marshal(body)
	up, err := r.InferResponses(ctx, out)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	defer up.Body.Close()
	copyHeader(w, up.Header)
	w.WriteHeader(up.Status)
	keep := func(resp map[string]any) {
		id, _ := resp["id"].(string)
		items, _ := resp["output"].([]any)
		if !store || id == "" || resp["status"] != "completed" {
			return
		}
		r.ResponseStore.PutID(id, model, append(input, asMaps(items)...))
	}
	if up.Status != http.StatusOK {
		io.Copy(w, up.Body)
		return
	}
	if !strings.HasPrefix(up.Header.Get("Content-Type"), "text/event-stream") {
		b, _ := io.ReadAll(up.Body)
		w.Write(b)
		var resp map[string]any
		if json.Unmarshal(b, &resp) == nil {
			keep(resp)
		}
		return
	}
	relayStream(w, up.Body, keep)
}

// relayStream passes an event stream on, event by event, filling in what
// llama.cpp leaves out and the OpenAI SDK's stream helper needs: the output
// list on the opening events (it appends to it), and the output_index,
// content_index and sequence_number that say where each piece belongs (it
// indexes by them, and crashed on None). Nothing the engine did send is
// changed. The finished response is handed to keep.
func relayStream(w http.ResponseWriter, body io.Reader, keep func(map[string]any)) {
	flusher, _ := w.(http.Flusher)
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 64<<10), 32<<20)
	seq := 0
	index := map[string]int{} // item id -> its place in the output
	for sc.Scan() {
		line := sc.Bytes()
		if data, ok := bytes.CutPrefix(line, []byte("data: ")); ok {
			var ev map[string]any
			if json.Unmarshal(data, &ev) == nil && ev != nil {
				if patchEvent(ev, &seq, index) {
					if b, err := json.Marshal(ev); err == nil {
						line = append([]byte("data: "), b...)
					}
				}
				if resp, ok := ev["response"].(map[string]any); ok && ev["type"] == "response.completed" {
					keep(resp)
				}
			}
		}
		w.Write(line)
		w.Write([]byte("\n"))
		if len(line) == 0 && flusher != nil {
			flusher.Flush()
		}
	}
	if flusher != nil {
		flusher.Flush()
	}
}

// patchEvent adds the fields an event is missing and reports whether it
// added any.
func patchEvent(ev map[string]any, seq *int, index map[string]int) bool {
	changed := false
	set := func(k string, v any) {
		if _, has := ev[k]; !has {
			ev[k], changed = v, true
		}
	}
	kind, _ := ev["type"].(string)
	set("sequence_number", *seq)
	*seq++
	switch kind {
	case "response.created", "response.in_progress":
		if resp, ok := ev["response"].(map[string]any); ok && resp["output"] == nil {
			resp["output"], changed = []any{}, true
		}
	case "response.output_item.added", "response.output_item.done":
		item, _ := ev["item"].(map[string]any)
		id, _ := item["id"].(string)
		if _, seen := index[id]; !seen {
			index[id] = len(index)
		}
		set("output_index", index[id])
	default:
		if id, ok := ev["item_id"].(string); ok {
			if i, seen := index[id]; seen {
				set("output_index", i)
			}
			// llama.cpp gives an item one content part.
			set("content_index", 0)
		}
	}
	return changed
}

// inputItems is the request's input as a list of items: a bare string is one
// user message.
func inputItems(raw json.RawMessage) ([]map[string]any, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, fmt.Errorf("input is missing")
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return []map[string]any{{"role": "user", "content": text}}, nil
	}
	var items []map[string]any
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, fmt.Errorf("input must be a string or a list of items")
	}
	return items, nil
}

func asMaps(items []any) []map[string]any {
	out := make([]map[string]any, 0, len(items))
	for _, it := range items {
		if m, ok := it.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

// splitTools separates the MCP servers a request names from the tools the
// engine understands.
func splitTools(raw json.RawMessage) (mcpSpecs []json.RawMessage, rest []json.RawMessage, err error) {
	if len(bytes.TrimSpace(raw)) == 0 || string(bytes.TrimSpace(raw)) == "null" {
		return nil, nil, nil
	}
	var tools []json.RawMessage
	if err := json.Unmarshal(raw, &tools); err != nil {
		return nil, nil, fmt.Errorf("tools must be a list")
	}
	for _, t := range tools {
		var kind struct {
			Type string `json:"type"`
		}
		json.Unmarshal(t, &kind)
		if kind.Type == "mcp" {
			mcpSpecs = append(mcpSpecs, t)
		} else {
			rest = append(rest, t)
		}
	}
	return mcpSpecs, rest, nil
}

// integrationOf turns a Responses MCP tool into the integration /api/v1/chat
// takes, so both endpoints resolve servers, and are refused, by one rule. A
// server_url is a server named in the request; without one, server_label
// names a server in mcp.json.
func integrationOf(spec json.RawMessage) (json.RawMessage, string, error) {
	var t struct {
		Label   string            `json:"server_label"`
		URL     string            `json:"server_url"`
		Allowed json.RawMessage   `json:"allowed_tools"`
		Headers map[string]string `json:"headers"`
	}
	if err := json.Unmarshal(spec, &t); err != nil || t.Label == "" {
		return nil, "", fmt.Errorf("an mcp tool needs a server_label")
	}
	// OpenAI also allows {"tool_names": [...]} here.
	var allowed []string
	if json.Unmarshal(t.Allowed, &allowed) != nil {
		var filter struct {
			Names []string `json:"tool_names"`
		}
		json.Unmarshal(t.Allowed, &filter)
		allowed = filter.Names
	}
	in := map[string]any{"allowed_tools": allowed}
	if t.URL != "" {
		in["type"], in["server_label"], in["server_url"], in["headers"] = "ephemeral_mcp", t.Label, t.URL, t.Headers
	} else {
		in["type"], in["id"] = "plugin", "mcp/"+strings.TrimPrefix(t.Label, "mcp/")
	}
	b, _ := json.Marshal(in)
	return b, t.Label, nil
}

// responsesWithTools runs the tool loop for a request that names MCP servers.
// Each model call is made without streaming, since the model's tool calls
// have to be read whole before they can be run; a caller that asked for a
// stream is sent the finished response as one.
func (r *Runner) responsesWithTools(ctx context.Context, body map[string]json.RawMessage, model string,
	input []map[string]any, specs, rest []json.RawMessage, stream, store bool, w http.ResponseWriter) {
	fail := func(err error) {
		if e, ok := err.(*Error); ok {
			writeErr(w, e.Status, e.Msg)
			return
		}
		writeErr(w, http.StatusBadGateway, err.Error())
	}
	var integrations []json.RawMessage
	labels := map[string]string{} // provider key -> server_label, as the caller named it
	for _, s := range specs {
		in, label, err := integrationOf(s)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		integrations = append(integrations, in)
		labels[label] = label
		labels["mcp/"+strings.TrimPrefix(label, "mcp/")] = label
	}
	tools, closeAll, err := r.tools(ctx, integrations)
	defer closeAll()
	if err != nil {
		fail(err)
		return
	}
	labelOf := func(p Provider) string { return labels[or(p.ServerLabel, p.PluginID)] }

	// What the model is offered: the caller's own tools, then each server's
	// as functions.
	offered := append([]json.RawMessage(nil), rest...)
	listed := map[string][]map[string]any{}
	for _, name := range sortedNames(tools) {
		t := tools[name]
		params := t.def.InputSchema
		if len(params) == 0 {
			params = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		fn, _ := json.Marshal(map[string]any{"type": "function", "name": name, "description": t.def.Description, "parameters": params})
		offered = append(offered, fn)
		l := labelOf(t.provider)
		listed[l] = append(listed[l], map[string]any{"name": name, "description": t.def.Description, "input_schema": params})
	}
	body["tools"], _ = json.Marshal(offered)
	body["stream"] = json.RawMessage("false")

	// shown is what the caller sees; input grows with what the engine needs
	// to continue (its own function_call items and their outputs).
	var shown []any
	for _, s := range specs {
		_, label, _ := integrationOf(s)
		shown = append(shown, map[string]any{"type": "mcp_list_tools", "id": "mcpl_" + label, "server_label": label, "tools": listed[label]})
	}
	var final map[string]any
	var header http.Header
	outTokens := 0
	for round := 0; ; round++ {
		if round == maxRounds {
			writeErr(w, http.StatusBadGateway, fmt.Sprintf("the model called tools %d times in a row without answering; stopped", maxRounds))
			return
		}
		body["input"], _ = json.Marshal(input)
		out, _ := json.Marshal(body)
		up, err := r.InferResponses(ctx, out)
		if err != nil {
			fail(err)
			return
		}
		b, _ := io.ReadAll(up.Body)
		up.Body.Close()
		if up.Status != http.StatusOK {
			copyHeader(w, up.Header)
			w.WriteHeader(up.Status)
			w.Write(b)
			return
		}
		var resp map[string]any
		if err := json.Unmarshal(b, &resp); err != nil {
			writeErr(w, http.StatusBadGateway, "the engine's response was not JSON")
			return
		}
		final, header = resp, up.Header
		if u, ok := resp["usage"].(map[string]any); ok {
			if n, ok := u["output_tokens"].(float64); ok {
				outTokens += int(n)
			}
		}
		items := asMaps(anyList(resp["output"]))
		input = append(input, items...)
		ran := false
		theirs := false
		for _, it := range items {
			name, _ := it["name"].(string)
			t, isMCP := tools[name]
			if it["type"] != "function_call" {
				shown = append(shown, it)
				continue
			}
			if !isMCP {
				// The caller's own function: theirs to run, so the loop ends
				// and the call is handed back.
				shown = append(shown, it)
				theirs = true
				continue
			}
			ran = true
			args, _ := it["arguments"].(string)
			callID, _ := it["call_id"].(string)
			id, _ := it["id"].(string)
			call := map[string]any{"type": "mcp_call", "id": "mcp_" + strings.TrimPrefix(id, "fc_"), "server_label": labelOf(t.provider),
				"name": name, "arguments": args, "status": "completed"}
			result := ""
			rawArgs := json.RawMessage(or(strings.TrimSpace(args), "{}"))
			if !json.Valid(rawArgs) {
				result = "error: the arguments were not valid JSON"
				call["error"], call["status"] = "the arguments were not valid JSON", "failed"
			} else if text, err := t.client.CallTool(ctx, name, rawArgs); err != nil {
				result = "error: " + err.Error()
				call["error"], call["status"] = err.Error(), "failed"
			} else {
				if len(text) > maxToolOutput {
					text = text[:maxToolOutput] + fmt.Sprintf("\n[output cut at %d bytes of %d]", maxToolOutput, len(text))
				}
				result = text
				call["output"] = text
			}
			shown = append(shown, call)
			input = append(input, map[string]any{"type": "function_call_output", "call_id": callID, "output": result})
		}
		if !ran || theirs {
			break
		}
	}
	final["output"] = shown
	if u, ok := final["usage"].(map[string]any); ok {
		if in, ok := u["input_tokens"].(float64); ok {
			u["output_tokens"], u["total_tokens"] = outTokens, int(in)+outTokens
		}
	}
	if id, _ := final["id"].(string); store && id != "" && final["status"] == "completed" {
		r.ResponseStore.PutID(id, model, input)
	}
	for k, v := range header {
		if strings.HasPrefix(k, "X-Fabric-") {
			w.Header()[k] = v
		}
	}
	if !stream {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(final)
		return
	}
	replay(w, final)
}

func anyList(v any) []any {
	l, _ := v.([]any)
	return l
}

func sortedNames(tools map[string]tool) []string {
	names := make([]string, 0, len(tools))
	for n := range tools {
		names = append(names, n)
	}
	// Sorted for the same reason as on /api/v1/chat: the tool list is prompt.
	sortStrings(names)
	return names
}

// replay sends a finished response as the event stream a caller asked for:
// the same events, in the same order, with each item's text in one piece.
func replay(w http.ResponseWriter, final map[string]any) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	seq := 0
	send := func(event string, fields map[string]any) {
		fields["type"], fields["sequence_number"] = event, seq
		seq++
		b, _ := json.Marshal(fields)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b)
	}
	opening := map[string]any{}
	for k, v := range final {
		opening[k] = v
	}
	opening["status"], opening["output"] = "in_progress", []any{}
	delete(opening, "usage")
	send("response.created", map[string]any{"response": opening})
	send("response.in_progress", map[string]any{"response": opening})
	for i, it := range anyList(final["output"]) {
		item, _ := it.(map[string]any)
		send("response.output_item.added", map[string]any{"output_index": i, "item": item})
		if item["type"] == "message" {
			for j, c := range anyList(item["content"]) {
				part, _ := c.(map[string]any)
				text, _ := part["text"].(string)
				id := item["id"]
				send("response.content_part.added", map[string]any{"item_id": id, "output_index": i, "content_index": j, "part": part})
				send("response.output_text.delta", map[string]any{"item_id": id, "output_index": i, "content_index": j, "delta": text})
				send("response.output_text.done", map[string]any{"item_id": id, "output_index": i, "content_index": j, "text": text})
				send("response.content_part.done", map[string]any{"item_id": id, "output_index": i, "content_index": j, "part": part})
			}
		}
		send("response.output_item.done", map[string]any{"output_index": i, "item": item})
	}
	send("response.completed", map[string]any{"response": final})
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

func copyHeader(w http.ResponseWriter, h http.Header) {
	for k, v := range h {
		switch k {
		case "Content-Length", "Connection", "Transfer-Encoding":
		default:
			w.Header()[k] = v
		}
	}
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"code": status, "message": msg, "type": "modelfabric_error"}})
}
