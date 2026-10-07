package router

import (
	"encoding/json"
	"testing"

	"github.com/gregbugaj/modelfabric/internal/mesh"
)

// An engine that cannot be told to answer to our model id is sent its own id
// instead; and nothing else about the request may change on the way.
func TestServedBodyRewritesOnlyTheModel(t *testing.T) {
	body := []byte(`{"model":"qwen/qwen3.8-27b","messages":[{"role":"user","content":"hi"}],` +
		`"stream":true,"temperature":0.7,"max_tokens":40}`)

	out := servedBody(mesh.Candidate{Local: true, ServedModel: "default_model"}, body)
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("rewritten body is not valid JSON: %v", err)
	}
	if got["model"] != "default_model" {
		t.Errorf("model = %v, want default_model", got["model"])
	}
	if got["stream"] != true || got["temperature"] != 0.7 || got["max_tokens"] != float64(40) {
		t.Errorf("other fields were disturbed: %v", got)
	}
	if msgs, ok := got["messages"].([]any); !ok || len(msgs) != 1 {
		t.Errorf("messages were disturbed: %v", got["messages"])
	}
}

// Every other candidate gets the bytes exactly as they arrived: a peer is sent
// the catalog key, because its own router resolves and rewrites in turn.
func TestServedBodyLeavesOtherCandidatesAlone(t *testing.T) {
	body := []byte(`{"model":"qwen/qwen3.8-27b","messages":[]}`)
	for _, c := range []struct {
		name string
		cand mesh.Candidate
	}{
		{"peer", mesh.Candidate{Node: "minion"}},
		{"local llama.cpp", mesh.Candidate{Local: true}},
		{"already correct", mesh.Candidate{Local: true, ServedModel: "qwen/qwen3.8-27b"}},
	} {
		if got := servedBody(c.cand, body); string(got) != string(body) {
			t.Errorf("%s: body was rewritten to %s", c.name, got)
		}
	}
}

// A body ModelFabric cannot parse is forwarded untouched: the engine's own error
// beats one invented here.
func TestServedBodyPassesThroughUnparsableBodies(t *testing.T) {
	body := []byte(`not json at all`)
	if got := servedBody(mesh.Candidate{Local: true, ServedModel: "default_model"}, body); string(got) != string(body) {
		t.Errorf("body was altered to %s", got)
	}
}
