package router

import (
	"encoding/json"
	"testing"

	"github.com/gregbugaj/modelfabric/internal/mesh"
)

type msg struct {
	Content json.RawMessage `json:"content"`
}

func msgs(raw ...string) []msg {
	out := make([]msg, 0, len(raw))
	for _, r := range raw {
		out = append(out, msg{Content: json.RawMessage(r)})
	}
	return out
}

// Engines loaded with vision disabled retain the model name but fail image
// requests with an mtmd chunk error; routing must exclude them for images.
func TestCarriesImage(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   []msg
		want bool
	}{
		{"plain text", msgs(`"hello"`), false},
		{"text parts only", msgs(`[{"type":"text","text":"hi"}]`), false},
		{"openai image_url", msgs(`[{"type":"text"},{"type":"image_url","image_url":{"url":"data:..."}}]`), true},
		{"anthropic image", msgs(`[{"type":"image","source":{}}]`), true},
		{"responses input_image", msgs(`[{"type":"input_image"}]`), true},
		{"image in a later turn", msgs(`"hi"`, `[{"type":"image_url"}]`), true},
		{"no messages", nil, false},
		// Unparseable content is read as text: that only widens where the
		// request may go, and a text request on a vision engine works fine.
		{"malformed content", msgs(`{"not":"parts"}`), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := make([]struct {
				Content json.RawMessage `json:"content"`
			}, len(tc.in))
			for i, m := range tc.in {
				in[i].Content = m.Content
			}
			if got := carriesImage(in); got != tc.want {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

// The fleet is mixed on purpose: text-only engines speculate and decode twice
// as fast, so a coding workload wants them, while images need one engine that
// kept its projector. Routing is what makes that safe.
func TestSplitVisionKeepsOnlyCapableEngines(t *testing.T) {
	in := []mesh.Candidate{
		{Node: "xpredator", Name: "inst-1", NoVision: true},
		{Node: "helion", Name: "helion"},
		{Node: "minion", Name: "inst-2", NoVision: true},
	}
	kept, dropped := splitVision(in)
	if len(kept) != 1 || kept[0].Node != "helion" {
		t.Errorf("only the engine with a projector may serve an image: %+v", kept)
	}
	if len(dropped) != 2 || dropped[0] != "xpredator/inst-1" || dropped[1] != "minion/inst-2" {
		t.Errorf("dropped engines should be named: %v", dropped)
	}
}

func TestSplitVisionCanDropEverything(t *testing.T) {
	kept, dropped := splitVision([]mesh.Candidate{{Node: "a", NoVision: true}})
	if len(kept) != 0 || len(dropped) != 1 {
		t.Errorf("kept=%v dropped=%v", kept, dropped)
	}
}
