package runtime

import (
	"slices"
	"testing"

	"github.com/gregbugaj/modelfabric/internal/catalog"
)

// A thinking model spends most of a classification's tokens before it answers.
// These are llama.cpp launch flags: the server accepts reasoning_budget in a
// request body and ignores it, so a per-request "control" silently does
// nothing — measured as an unordered sweep (budget 0 gave more reasoning than
// budget 512). Set at launch, it holds: 48 kept reasoning to 165-179
// characters over three runs where -1 gave 718-1105.
func TestReasoningFlagsReachTheEngine(t *testing.T) {
	budget := 48
	on, low := "on", "low"
	d := &Definition{Name: "llama.cpp", Engine: "llama.cpp", ContextLength: 8192, Parallel: 1}
	a := llamaCPP{}.Apply(d, catalog.Model{Key: "q", Format: "gguf"}, Requested{
		Settings: Settings{Reasoning: &on, ReasoningEffort: &low, ReasoningBudget: &budget},
	})
	argv := llamaCPP{}.Argv(d, catalog.Model{Key: "q", Format: "gguf", Path: "/m.gguf"}, a, "127.0.0.1", 18000)

	for _, want := range [][2]string{{"--reasoning", "on"}, {"--reasoning-effort", "low"}, {"--reasoning-budget", "48"}} {
		i := slices.Index(argv, want[0])
		if i < 0 || i+1 >= len(argv) || argv[i+1] != want[1] {
			t.Errorf("%s %s missing from argv: %v", want[0], want[1], argv)
		}
	}
}

// Unset means the engine's own default, which is the model template's. A
// budget of 0 is a real choice (end thinking at once) and must not be read as
// "not set" — which a plain int would have done.
func TestReasoningUnsetAddsNoFlags(t *testing.T) {
	d := &Definition{Name: "llama.cpp", Engine: "llama.cpp", ContextLength: 8192, Parallel: 1}
	m := catalog.Model{Key: "q", Format: "gguf", Path: "/m.gguf"}
	a := llamaCPP{}.Apply(d, m, Requested{})
	argv := llamaCPP{}.Argv(d, m, a, "127.0.0.1", 18000)
	for _, f := range []string{"--reasoning", "--reasoning-effort", "--reasoning-budget"} {
		if slices.Contains(argv, f) {
			t.Errorf("%s was passed although nothing asked for it: %v", f, argv)
		}
	}

	zero := 0
	a = llamaCPP{}.Apply(d, m, Requested{Settings: Settings{ReasoningBudget: &zero}})
	argv = llamaCPP{}.Argv(d, m, a, "127.0.0.1", 18000)
	i := slices.Index(argv, "--reasoning-budget")
	if i < 0 || argv[i+1] != "0" {
		t.Errorf("a budget of 0 must reach the engine: %v", argv)
	}
}

func TestReasoningSettingsAreValidated(t *testing.T) {
	bad := "sometimes"
	if err := (Settings{Reasoning: &bad}).Validate(); err == nil {
		t.Error("an unknown reasoning mode should be refused")
	}
	// The levels belong to the model's chat template, so ModelFabric checks the
	// shape and leaves the vocabulary to the model: llama.cpp's own help lists
	// "minimal", which this 27B rejects with a Jinja exception.
	effort := "quite a lot"
	if err := (Settings{ReasoningEffort: &effort}).Validate(); err == nil {
		t.Error("an effort level with spaces is not a level")
	}
	for _, ok := range []string{"xhigh", "medium", "low"} {
		if err := (Settings{ReasoningEffort: &ok}).Validate(); err != nil {
			t.Errorf("%q is a level this model takes: %v", ok, err)
		}
	}
	neg := -5
	if err := (Settings{ReasoningBudget: &neg}).Validate(); err == nil {
		t.Error("a budget below -1 should be refused")
	}
	ok := 128
	if err := (Settings{ReasoningBudget: &ok}).Validate(); err != nil {
		t.Errorf("a token budget is valid: %v", err)
	}
}
