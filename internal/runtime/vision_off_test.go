package runtime

import (
	"slices"
	"testing"

	"github.com/gregbugaj/modelfabric/internal/catalog"
)

func off() *bool { v := false; return &v }

// A vision-capable model used for text pays for the projector anyway: its
// weights sit in VRAM unused, and the engine takes one slot rather than the
// runtime's. An agentic coding run sends no images, so it should be able to say
// so.
//
// This used to buy back speculation as well — ModelFabric turned drafting off for any
// model with a projector, worth 66 tok/s against 134 on a 5090 — but a vision
// load keeps its MTP head now (see vision_spec_test.go), so what -vision off
// saves is the projector's memory and the slots.
func TestVisionOffLoadsTextOnly(t *testing.T) {
	d := &Definition{Name: "llama.cpp", Engine: "llama.cpp", ContextLength: 8192, Parallel: 4}
	m := catalog.Model{Key: "q", Format: "gguf", Path: "/m.gguf", Projector: "/mmproj.gguf", DraftLayers: 3}

	a := llamaCPP{}.Apply(d, m, Requested{Settings: Settings{Vision: off()}})
	if a.Vision {
		t.Error("the instance was asked to serve text only")
	}
	if !a.Speculative || a.SpecType != "draft-mtp" {
		t.Errorf("auto should use the MTP head on a text-only load: %+v", a)
	}
	// And the slots are the runtime's again, not the one image work gets.
	if a.Parallel != 4 {
		t.Errorf("a text-only load keeps the runtime's slots, got %d", a.Parallel)
	}
	if a.SlotsClamped != "" {
		t.Errorf("nothing was clamped: %q", a.SlotsClamped)
	}
	// The projector must not reach the engine, or it loads anyway.
	if argv := (llamaCPP{}).Argv(d, m, a, "127.0.0.1", 18000); slices.Contains(argv, "--mmproj") {
		t.Errorf("--mmproj was passed on a text-only load: %v", argv)
	}
}

// Unset keeps every load that came before it working the same way.
func TestVisionUnsetStillServesImages(t *testing.T) {
	d := &Definition{Name: "llama.cpp", Engine: "llama.cpp", ContextLength: 8192, Parallel: 4}
	m := catalog.Model{Key: "q", Format: "gguf", Path: "/m.gguf", Projector: "/mmproj.gguf", DraftLayers: 3}
	a := llamaCPP{}.Apply(d, m, Requested{})
	// Images, one slot, and — unlike before 2026-09-25 — its MTP head.
	if !a.Vision || !a.Speculative || a.Parallel != 1 {
		t.Errorf("the default is image work with drafting: %+v", a)
	}
	if argv := (llamaCPP{}).Argv(d, m, a, "127.0.0.1", 18000); !slices.Contains(argv, "--mmproj") {
		t.Errorf("--mmproj belongs on an image load: %v", argv)
	}
}

// A model with no projector is unaffected either way.
func TestVisionOffOnATextModelChangesNothing(t *testing.T) {
	d := &Definition{Name: "llama.cpp", Engine: "llama.cpp", ContextLength: 8192, Parallel: 4}
	m := catalog.Model{Key: "q", Format: "gguf", Path: "/m.gguf", DraftLayers: 3}
	a := llamaCPP{}.Apply(d, m, Requested{Settings: Settings{Vision: off()}})
	if a.Vision || !a.Speculative || a.Parallel != 4 {
		t.Errorf("a text model is already text: %+v", a)
	}
}

// VisionSkipped is what routing keys on, and it must separate "declined a
// projector" from "never had one" — otherwise every text engine in the mesh
// looks like one that lost a capability, and an image request finds nowhere
// to go.
func TestVisionSkippedOnlyWhenAProjectorWasDeclined(t *testing.T) {
	d := &Definition{Name: "llama.cpp", Engine: "llama.cpp", ContextLength: 8192, Parallel: 4}
	withProjector := catalog.Model{Key: "q", Format: "gguf", Path: "/m.gguf", Projector: "/mmproj.gguf"}
	textOnly := catalog.Model{Key: "q", Format: "gguf", Path: "/m.gguf"}

	if a := (llamaCPP{}).Apply(d, withProjector, Requested{Settings: Settings{Vision: off()}}); a.VisionSkipped == "" {
		t.Error("a declined projector must be recorded")
	}
	if a := (llamaCPP{}).Apply(d, withProjector, Requested{}); a.VisionSkipped != "" {
		t.Errorf("nothing was declined: %q", a.VisionSkipped)
	}
	if a := (llamaCPP{}).Apply(d, textOnly, Requested{Settings: Settings{Vision: off()}}); a.VisionSkipped != "" {
		t.Errorf("a model with no projector declined nothing: %q", a.VisionSkipped)
	}
}
