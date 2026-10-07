package router

import (
	"strings"
	"testing"
)

func TestPrefillStreamWaitsForGeneration(t *testing.T) {
	for _, tc := range []struct {
		name, wire string
		want       bool
	}{
		{"heartbeat does not finish prefill", ": keepalive\n\n", false},
		{"role alone does not finish prefill", "data: {\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n\n", false},
		{"chat content finishes prefill", "data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\n", true},
		{"reasoning finishes prefill", "data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"think\"}}]}\r\n\r\n", true},
		{"tool generation finishes prefill", "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"function\":{\"arguments\":\"{\"}}]}}]}\n\n", true},
		{"completion text finishes prefill", "data: {\"choices\":[{\"text\":\"hello\"}]}\n\n", true},
		{"responses delta finishes prefill", "data: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}\n\n", true},
		{"anthropic delta finishes prefill", "data: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"hello\"}}\n\n", true},
		{"invalid data does not finish prefill", "data: oops\n\n", false},
		{"oversized event recovers at next event", "data: " + strings.Repeat("x", 100<<10) + "\n\ndata: {\"choices\":[{\"text\":\"hello\"}]}\n\n", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Network reads may split a JSON key or the event delimiter anywhere.
			for _, size := range []int{1, 17, len(tc.wire)} {
				var s prefillStream
				got := false
				for i := 0; i < len(tc.wire); i += size {
					got = s.Add([]byte(tc.wire[i:min(i+size, len(tc.wire))])) || got
				}
				if got != tc.want {
					t.Errorf("chunk size %d: generated=%v, want %v", size, got, tc.want)
				}
			}
		})
	}
}
