package runtime

import (
	"testing"

	"github.com/gregbugaj/modelfabric/internal/catalog"
)

func visionModel() catalog.Model {
	return catalog.Model{Key: "q", Format: "gguf", DraftLayers: 3, Projector: "/models/mmproj.gguf"}
}

func textModel() catalog.Model {
	return catalog.Model{Key: "q", Format: "gguf", DraftLayers: 3}
}

// A vision model keeps its MTP head on auto. The former blanket disable
// worked around llama.cpp's image-drafting mtmd chunk defect; older builds
// can still use spec_mode off.
func TestAutoSpeculationStaysOnForVisionModels(t *testing.T) {
	a := llamaCPP{}.Apply(&Definition{Name: "llama.cpp", Engine: "llama.cpp"}, visionModel(), Requested{})
	if !a.Speculative || a.SpecType != "draft-mtp" {
		t.Errorf("auto should use the MTP head on a vision model too: %+v", a)
	}
}

// Vision defaults to one slot: KV allocation scales with parallel, while
// the vision encoder processes images serially. Per-request context is unchanged.
func TestVisionModelsDefaultToOneSlot(t *testing.T) {
	d := &Definition{Name: "llama.cpp", Engine: "llama.cpp", Parallel: 4, ContextLength: 8192}
	a := llamaCPP{}.Apply(d, visionModel(), Requested{})
	if a.Parallel != 1 {
		t.Errorf("a vision model should default to 1 slot, got %d", a.Parallel)
	}
	if a.ContextLength != 8192 {
		t.Errorf("per-request context must not change with the slot count: %d", a.ContextLength)
	}
	if a.SlotsClamped == "" {
		t.Error("the reason should be recorded, or a lowered slot count looks like a bug")
	}
	if got := (llamaCPP{}).applyKV(a); got != 8192 {
		t.Errorf("-c should be one slot's worth: got %d, want 8192", got)
	}
}

func TestTextModelsKeepTheirSlots(t *testing.T) {
	d := &Definition{Name: "llama.cpp", Engine: "llama.cpp", Parallel: 4, ContextLength: 8192}
	a := llamaCPP{}.Apply(d, textModel(), Requested{})
	if a.Parallel != 4 {
		t.Errorf("a text model should keep 4 slots, got %d", a.Parallel)
	}
	if a.SlotsClamped != "" {
		t.Errorf("nothing was clamped, but a reason was recorded: %q", a.SlotsClamped)
	}
}

func TestExplicitParallelOverridesTheVisionDefault(t *testing.T) {
	four := 4
	d := &Definition{Name: "llama.cpp", Engine: "llama.cpp", Parallel: 1, ContextLength: 8192}
	a := llamaCPP{}.Apply(d, visionModel(), Requested{Settings: Settings{Parallel: &four}})
	if a.Parallel != 4 {
		t.Errorf("-parallel 4 should be honoured on a vision model, got %d", a.Parallel)
	}
	if a.SlotsClamped != "" {
		t.Errorf("an explicit -parallel was not clamped, so no reason belongs: %q", a.SlotsClamped)
	}
}
