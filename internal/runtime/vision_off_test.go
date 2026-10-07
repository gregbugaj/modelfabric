package runtime

import (
	"slices"
	"testing"

	"github.com/gregbugaj/modelfabric/internal/catalog"
)

func off() *bool { v := false; return &v }

// Disabling vision omits the projector's VRAM allocation and restores the
// runtime's slot count. Speculation is controlled independently.
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
	if a.Parallel != 4 {
		t.Errorf("a text-only load keeps the runtime's slots, got %d", a.Parallel)
	}
	if a.SlotsClamped != "" {
		t.Errorf("nothing was clamped: %q", a.SlotsClamped)
	}
	if argv := (llamaCPP{}).Argv(d, m, a, "127.0.0.1", 18000); slices.Contains(argv, "--mmproj") {
		t.Errorf("--mmproj was passed on a text-only load: %v", argv)
	}
}

func TestVisionUnsetStillServesImages(t *testing.T) {
	d := &Definition{Name: "llama.cpp", Engine: "llama.cpp", ContextLength: 8192, Parallel: 4}
	m := catalog.Model{Key: "q", Format: "gguf", Path: "/m.gguf", Projector: "/mmproj.gguf", DraftLayers: 3}
	a := llamaCPP{}.Apply(d, m, Requested{})
	if !a.Vision || !a.Speculative || a.Parallel != 1 {
		t.Errorf("the default is image work with drafting: %+v", a)
	}
	if argv := (llamaCPP{}).Argv(d, m, a, "127.0.0.1", 18000); !slices.Contains(argv, "--mmproj") {
		t.Errorf("--mmproj belongs on an image load: %v", argv)
	}
}

func TestVisionOffOnATextModelChangesNothing(t *testing.T) {
	d := &Definition{Name: "llama.cpp", Engine: "llama.cpp", ContextLength: 8192, Parallel: 4}
	m := catalog.Model{Key: "q", Format: "gguf", Path: "/m.gguf", DraftLayers: 3}
	a := llamaCPP{}.Apply(d, m, Requested{Settings: Settings{Vision: off()}})
	if a.Vision || !a.Speculative || a.Parallel != 4 {
		t.Errorf("a text model is already text: %+v", a)
	}
}

// VisionSkipped is what routing keys on, and it must separate "declined a
// projector" from "never had one" - otherwise every text engine in the mesh
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
