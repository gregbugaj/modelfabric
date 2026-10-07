package router

import (
	"bytes"
	"encoding/json"
	"strings"
)

// prefillStream observes SSE without changing what is forwarded. Headers,
// keepalives and role-only chunks can arrive while an engine is still queued;
// only generated output releases its estimated prefill load.
type prefillStream struct {
	line, data []byte
	overflow   bool
}

func (s *prefillStream) Add(chunk []byte) bool {
	const limit = 64 << 10
	for _, b := range chunk {
		if b != '\n' {
			if len(s.line) < limit {
				s.line = append(s.line, b)
			} else {
				s.overflow = true
			}
			continue
		}
		line := bytes.TrimSuffix(s.line, []byte{'\r'})
		if len(line) == 0 {
			generated := !s.overflow && generatedOutput(s.data)
			s.data, s.line, s.overflow = s.data[:0], s.line[:0], false
			if generated {
				return true
			}
		} else {
			if data, ok := bytes.CutPrefix(line, []byte("data:")); ok && !s.overflow {
				if len(s.data)+len(data)+1 <= limit {
					s.data = append(s.data, data...)
					s.data = append(s.data, '\n')
				} else {
					s.overflow = true
				}
			}
			s.line = s.line[:0]
		}
	}
	return false
}

func generatedOutput(data []byte) bool {
	var event struct {
		Type    string          `json:"type"`
		Delta   json.RawMessage `json:"delta"`
		Choices []struct {
			Text  string                     `json:"text"`
			Delta map[string]json.RawMessage `json:"delta"`
		} `json:"choices"`
	}
	if json.Unmarshal(data, &event) != nil {
		return false
	}
	for _, choice := range event.Choices {
		if choice.Text != "" {
			return true
		}
		for _, key := range []string{"content", "reasoning_content", "reasoning", "tool_calls", "function_call", "audio"} {
			if nonemptyDelta(choice.Delta[key]) {
				return true
			}
		}
	}
	if strings.HasPrefix(event.Type, "response.") && strings.HasSuffix(event.Type, ".delta") {
		return nonemptyDelta(event.Delta)
	}
	if event.Type == "content_block_delta" {
		var delta map[string]json.RawMessage
		if json.Unmarshal(event.Delta, &delta) == nil {
			for _, key := range []string{"text", "thinking", "partial_json"} {
				if nonemptyDelta(delta[key]) {
					return true
				}
			}
		}
	}
	return false
}

func nonemptyDelta(raw json.RawMessage) bool {
	s := string(bytes.TrimSpace(raw))
	return s != "" && s != "null" && s != `""` && s != "[]" && s != "{}"
}
