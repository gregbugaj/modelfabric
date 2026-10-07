package router

import (
	"encoding/json"
	"testing"

	"github.com/gregbugaj/modelfabric/internal/mesh"
)

func TestConstrainedOutput(t *testing.T) {
	for _, c := range []struct {
		name, format, grammar, schema string
		want                          bool
	}{
		{name: "nothing asked for"},
		{name: "plain text", format: "text"},
		{name: "json_object", format: "json_object", want: true},
		{name: "json_schema", format: "json_schema", want: true},
		{name: "llama.cpp grammar", grammar: `root ::= "yes" | "no"`, want: true},
		{name: "llama.cpp json_schema", schema: `{"type":"object"}`, want: true},
		{name: "empty schema", schema: `  `},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := constrainedOutput(c.format, c.grammar, json.RawMessage(c.schema))
			if got != c.want {
				t.Errorf("constrainedOutput(%q, %q, %q) = %v, want %v", c.format, c.grammar, c.schema, got, c.want)
			}
		})
	}
}

// The case this exists for: a mixed fleet must route around the engine that
// would answer with free prose, not fail and not silently use it.
func TestSplitConstrainedPrefersACapableEngine(t *testing.T) {
	in := []mesh.Candidate{
		{Node: "helion", Name: "inst-mlx", NoConstrainedDecoding: true},
		{Node: "xpredator", Name: "inst-llama"},
		{Node: "minion", Name: "inst-mlx2", NoConstrainedDecoding: true},
	}
	kept, dropped := splitConstrained(in)
	if len(kept) != 1 || kept[0].Node != "xpredator" {
		t.Fatalf("kept %+v, want only xpredator", kept)
	}
	if len(dropped) != 2 {
		t.Fatalf("dropped %v, want both mlx engines named for the error", dropped)
	}
	if dropped[0] != "helion/inst-mlx" {
		t.Errorf("dropped[0] = %q, want node/engine", dropped[0])
	}
	peerOnly, peerDropped := splitConstrained([]mesh.Candidate{
		{Node: "helion", Name: "helion", NoConstrainedDecoding: true},
	})
	if len(peerOnly) != 0 || len(peerDropped) != 1 || peerDropped[0] != "helion" {
		t.Errorf("peer dropped as %v, want just [helion]", peerDropped)
	}
}

func TestSplitConstrainedLlamaOnlyIsUntouched(t *testing.T) {
	in := []mesh.Candidate{{Node: "a", Name: "1"}, {Node: "b", Name: "2"}}
	kept, dropped := splitConstrained(in)
	if len(kept) != 2 || len(dropped) != 0 {
		t.Errorf("kept %d dropped %v, want all kept", len(kept), dropped)
	}
}
