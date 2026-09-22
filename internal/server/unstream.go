package server

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

// Asking the engine to stream even when the caller did not, so that what it is
// writing can be watched, and putting the single reply back together for the
// caller.
//
// A caller that sends "stream": false gets the whole answer in one piece, and
// the tokens never cross the wire separately — the engine generates them, holds
// them, and sends a blob. There is nothing to observe, which is why the live
// view was blank for every such caller (aider's benchmark among them, so the
// panel looked broken during the run it was built for).
//
// ModelFabric is already the proxy in front, so it asks upstream for a stream, reads
// it as it arrives, and hands the caller the one JSON body it expects. This is
// the data path, not a side channel: get the reassembly wrong and a
// non-streaming caller gets a wrong answer. So it is deliberately narrow —
// only OpenAI-shaped chat completions, and anything unexpected is passed
// through untouched rather than guessed at.

// unstreamable reports whether this request can be upgraded, and returns the
// rewritten body.
//
// Only /v1/chat/completions: /v1/messages is Anthropic-shaped and streams
// differently, and embeddings and the rest do not stream at all. A body that
// does not parse is left alone — the engine's own error beats one invented
// here.
func unstreamable(path string, body []byte) ([]byte, bool) {
	if path != "/v1/chat/completions" || len(body) == 0 {
		return body, false
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(body, &m) != nil {
		return body, false
	}
	if raw, ok := m["stream"]; ok {
		var streaming bool
		// A "stream" that is not a bool is something this does not understand.
		if json.Unmarshal(raw, &streaming) != nil || streaming {
			return body, false
		}
	}
	m["stream"] = json.RawMessage(`true`)
	// Without this the final chunk carries no usage, and a caller that had
	// been getting token counts would silently stop — a regression dressed as
	// a feature.
	m["stream_options"] = json.RawMessage(`{"include_usage":true}`)
	out, err := json.Marshal(m)
	if err != nil {
		return body, false
	}
	return out, true
}

// streamChoice accumulates one choice across the chunks.
type streamChoice struct {
	index        int
	role         string
	content      strings.Builder
	reasoning    strings.Builder
	finishReason *string
	logprobs     json.RawMessage
	tools        map[int]*streamTool
	toolOrder    []int
}

type streamTool struct {
	id, kind, name string
	args           strings.Builder
}

// assembleStream reads an OpenAI-style event stream, hands each frame's payload
// to onChunk as it arrives, and returns the non-streaming reply.
//
// onChunk is what makes this live: it runs per frame, while the engine is still
// writing, which is the whole reason for upgrading the request.
func assembleStream(r io.Reader, onChunk func([]byte)) ([]byte, error) {
	sc := bufio.NewScanner(r)
	// Chunks are small, but a single frame carrying a large tool-call argument
	// fragment is not, and a scanner that gives up mid-stream would truncate
	// the answer rather than report it.
	sc.Buffer(make([]byte, 64<<10), 8<<20)

	var head map[string]json.RawMessage
	var usage json.RawMessage
	choices := map[int]*streamChoice{}
	var order []int
	sawChunk := false

	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		payload, ok := strings.CutPrefix(line, "data:")
		if !ok {
			continue
		}
		payload = strings.TrimSpace(payload)
		if payload == "" || payload == "[DONE]" {
			continue
		}
		if onChunk != nil {
			onChunk([]byte(payload))
		}

		var raw map[string]json.RawMessage
		if json.Unmarshal([]byte(payload), &raw) != nil {
			continue
		}
		// An error frame mid-stream is the engine's to report, not this
		// function's to reassemble into an answer.
		if _, bad := raw["error"]; bad {
			return []byte(payload), nil
		}
		sawChunk = true
		if head == nil {
			head = raw
		}
		if u, ok := raw["usage"]; ok && !isJSONNull(u) {
			usage = u
		}

		var chunk struct {
			Choices []struct {
				Index int `json:"index"`
				Delta struct {
					Role             string          `json:"role"`
					Content          string          `json:"content"`
					ReasoningContent string          `json:"reasoning_content"`
					Reasoning        string          `json:"reasoning"`
					ToolCalls        []toolCallDelta `json:"tool_calls"`
				} `json:"delta"`
				FinishReason *string         `json:"finish_reason"`
				Logprobs     json.RawMessage `json:"logprobs"`
			} `json:"choices"`
		}
		if json.Unmarshal([]byte(payload), &chunk) != nil {
			continue
		}
		for _, c := range chunk.Choices {
			ch := choices[c.Index]
			if ch == nil {
				ch = &streamChoice{index: c.Index, tools: map[int]*streamTool{}}
				choices[c.Index] = ch
				order = append(order, c.Index)
			}
			if c.Delta.Role != "" {
				ch.role = c.Delta.Role
			}
			ch.content.WriteString(c.Delta.Content)
			if c.Delta.ReasoningContent != "" {
				ch.reasoning.WriteString(c.Delta.ReasoningContent)
			} else {
				ch.reasoning.WriteString(c.Delta.Reasoning)
			}
			if c.FinishReason != nil {
				ch.finishReason = c.FinishReason
			}
			if len(c.Logprobs) > 0 && !isJSONNull(c.Logprobs) {
				ch.logprobs = c.Logprobs
			}
			for _, t := range c.Delta.ToolCalls {
				tl := ch.tools[t.Index]
				if tl == nil {
					tl = &streamTool{}
					ch.tools[t.Index] = tl
					ch.toolOrder = append(ch.toolOrder, t.Index)
				}
				if t.ID != "" {
					tl.id = t.ID
				}
				if t.Type != "" {
					tl.kind = t.Type
				}
				if t.Function.Name != "" {
					tl.name = t.Function.Name
				}
				tl.args.WriteString(t.Function.Arguments)
			}
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("reading the upstream stream: %w", err)
	}
	if !sawChunk || head == nil {
		return nil, errors.New("the upstream stream carried no chunks")
	}

	// The first chunk's envelope is kept whole, so id, created, model and
	// anything this does not know about survive.
	out := map[string]json.RawMessage{}
	for k, v := range head {
		out[k] = v
	}
	out["object"] = json.RawMessage(`"chat.completion"`)
	delete(out, "usage")
	if len(usage) > 0 {
		out["usage"] = usage
	}

	assembled := make([]map[string]any, 0, len(order))
	for _, i := range order {
		ch := choices[i]
		msg := map[string]any{"role": orDefault(ch.role, "assistant"), "content": ch.content.String()}
		if ch.reasoning.Len() > 0 {
			msg["reasoning_content"] = ch.reasoning.String()
		}
		if len(ch.toolOrder) > 0 {
			calls := make([]map[string]any, 0, len(ch.toolOrder))
			for _, ti := range ch.toolOrder {
				t := ch.tools[ti]
				calls = append(calls, map[string]any{
					"id": t.id, "type": orDefault(t.kind, "function"),
					"function": map[string]any{"name": t.name, "arguments": t.args.String()},
				})
			}
			msg["tool_calls"] = calls
			// OpenAI sends content null, not "", when the reply is only tool
			// calls, and clients branch on it.
			if ch.content.Len() == 0 {
				msg["content"] = nil
			}
		}
		c := map[string]any{"index": ch.index, "message": msg, "finish_reason": ch.finishReason}
		if len(ch.logprobs) > 0 {
			c["logprobs"] = ch.logprobs
		}
		assembled = append(assembled, c)
	}
	cj, err := json.Marshal(assembled)
	if err != nil {
		return nil, err
	}
	out["choices"] = cj
	return json.Marshal(out)
}

type toolCallDelta struct {
	Index    int    `json:"index"`
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

func isJSONNull(b json.RawMessage) bool { return bytes.Equal(bytes.TrimSpace(b), []byte("null")) }

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}
