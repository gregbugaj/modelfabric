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

// A vision model keeps its MTP head on "auto".
//
// It did not, for a while: llama.cpp b11040 failed the whole request with
// "failed to process mtmd chunk" when it drafted a prompt carrying an image, so
// ModelFabric turned speculation off for any model with a projector. That defect is
// gone from the builds this fleet runs — re-tested 2026-09-24 at the benchmark's
// own context and slots with the projector loaded, two concurrent image requests
// of 986KB and 827KB succeeded with drafting live throughout, on CUDA and on
// Metal — and the guard applied to every default load of a model that has a
// projector. Whether drafting pays is about the work, not the projector: 66
// tok/s decoding against 134 on a 5090 where the output was predictable, and a
// little worse than off where it was not. Either way it is not this code's
// decision; -spec off is.
func TestAutoSpeculationStaysOnForVisionModels(t *testing.T) {
	a := llamaCPP{}.Apply(&Definition{Name: "llama.cpp", Engine: "llama.cpp"}, visionModel(), Requested{})
	if !a.Speculative || a.SpecType != "draft-mtp" {
		t.Errorf("auto should use the MTP head on a vision model too: %+v", a)
	}
}

// Slots are the other vision default. -c is per-slot × parallel, so the four
// slots a text model gets ask the GPU for four times the KV cache — and an
// image request is serialised on the vision encoder regardless, so the slots
// buy little. One slot is the default; the per-request context is untouched.
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

// A text model keeps the runtime's slots. This is about images, not the family.
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

// -parallel is a deliberate choice, and ModelFabric does not overrule one. Someone
// who has the memory for four vision slots can still ask for them.
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
