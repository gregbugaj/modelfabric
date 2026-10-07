package supervisor

import (
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gregbugaj/modelfabric/internal/catalog"
	"github.com/gregbugaj/modelfabric/internal/runtime"
)

func ptr[T any](v T) *T { return &v }

func newTestSupervisor(t *testing.T) *Supervisor {
	return &Supervisor{cfg: Config{DataDir: t.TempDir()}}
}

// Lowest first: the model's default preset, its default settings, a preset
// named at load, settings given at load.
func TestSettingsPrecedence(t *testing.T) {
	s := newTestSupervisor(t)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(s.SavePreset(Preset{Name: "calm", Settings: runtime.Settings{Temperature: ptr(0.2), TopK: ptr(10)}}))
	must(s.SavePreset(Preset{Name: "wild", Settings: runtime.Settings{Temperature: ptr(1.5)}}))
	must(s.SetDefaults("qwen/x", ModelDefaults{Preset: "calm",
		Settings: runtime.Settings{TopK: ptr(20), ContextLength: ptr(16384)}}))

	got, err := s.resolveSettings("qwen/x", "", runtime.Settings{})
	must(err)
	if *got.Temperature != 0.2 || *got.TopK != 20 || *got.ContextLength != 16384 {
		t.Fatalf("defaults: %+v", got)
	}
	got, err = s.resolveSettings("qwen/x", "wild", runtime.Settings{ContextLength: ptr(4096)})
	must(err)
	if *got.Temperature != 1.5 || *got.TopK != 20 || *got.ContextLength != 4096 {
		t.Fatalf("load-time preset and settings: %+v", got)
	}
	if _, err := s.resolveSettings("qwen/x", "missing", runtime.Settings{}); err == nil {
		t.Fatal("a missing preset was silently dropped")
	}
	must(s.SetDefaults("qwen/x", ModelDefaults{}))
	d, err := s.Defaults("qwen/x")
	if err != nil {
		t.Fatal(err)
	}
	if d.Preset != "" || !d.Settings.IsZero() {
		t.Fatalf("defaults not cleared: %+v", d)
	}
}

func TestPresetsHoldInferenceOnly(t *testing.T) {
	s := newTestSupervisor(t)
	err := s.SavePreset(Preset{Name: "bad", Settings: runtime.Settings{Temperature: ptr(0.5), ContextLength: ptr(8192)}})
	if err == nil || !strings.Contains(err.Error(), "context_length") {
		t.Fatalf("load setting accepted in a preset: %v", err)
	}
	if err := s.SavePreset(Preset{Name: "../escape", Settings: runtime.Settings{Temperature: ptr(0.5)}}); err == nil {
		t.Fatal("path-like preset name accepted")
	}
}

func TestImportLMStudioPreset(t *testing.T) {
	lm := `{"name":"Creative","operation":{"fields":[
		{"key":"llm.prediction.temperature","value":1.1},
		{"key":"llm.prediction.topKSampling","value":50},
		{"key":"llm.prediction.minPSampling","value":{"checked":false,"value":0.05}},
		{"key":"llm.prediction.topPSampling","value":{"checked":true,"value":0.9}},
		{"key":"llm.prediction.systemPrompt","value":"You are a poet."}]}}`
	p, skipped, err := ImportLMStudioPreset([]byte(lm), "")
	if err != nil {
		t.Fatal(err)
	}
	st := p.Settings
	if p.Name != "Creative" || *st.Temperature != 1.1 || *st.TopK != 50 || *st.TopP != 0.9 {
		t.Fatalf("imported %+v", st)
	}
	// Unchecked means the sampler is off: min-p 0, not LM Studio's stored 0.05.
	if st.MinP == nil || *st.MinP != 0 {
		t.Fatalf("unchecked min_p = %v, want 0", st.MinP)
	}
	if len(skipped) != 1 || skipped[0] != "llm.prediction.systemPrompt" {
		t.Fatalf("skipped = %v", skipped)
	}
}

func TestEntrypointRefusesLoads(t *testing.T) {
	s := newTestSupervisor(t)
	s.cfg.Entrypoint, s.cfg.JIT = true, true
	if _, _, err := s.Load(LoadRequest{Model: "anything"}); err == nil || !strings.Contains(err.Error(), "entrypoint") {
		t.Fatalf("Load on an entrypoint = %v, want a refusal naming the role", err)
	}
	if s.JITEnabled() {
		t.Fatal("JIT must be off on an entrypoint")
	}
}

// Corrupt state used to read as absent state: a defaults file that would not
// parse dropped the operator's saved settings, an unreadable presets directory
// showed as "no presets", and a hand-edited preset carrying load-only settings
// was merged into a load that never asked for them.
func TestCorruptSettingsAreReportedNotIgnored(t *testing.T) {
	s := newTestSupervisor(t)
	dir := s.cfg.DataDir

	if err := os.MkdirAll(filepath.Join(dir, "model-defaults"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "model-defaults", url.PathEscape("qwen/bad")+".json"), []byte("{oops"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Defaults("qwen/bad"); err == nil {
		t.Error("a corrupt defaults file was treated as no defaults")
	}

	if err := os.MkdirAll(filepath.Join(dir, "presets"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "presets", "stale.json"),
		[]byte(`{"name":"stale","settings":{"context_length":4096}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Preset("stale"); err == nil {
		t.Error("a preset carrying a load-only setting was accepted")
	}
	if _, err := s.Presets(); err == nil {
		t.Error("listing presets hid the bad one")
	}
}

// Deleting a preset a model still names used to break that model's next load,
// because resolveSettings treats a missing named preset as an error.
func TestDeletingAPresetInUseIsRefused(t *testing.T) {
	s := newTestSupervisor(t)
	if err := s.SavePreset(Preset{Name: "creative", Settings: runtime.Settings{Temperature: ptr(0.9)}}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetDefaults("qwen/x", ModelDefaults{Preset: "creative"}); err != nil {
		t.Fatal(err)
	}
	err := s.DeletePreset("creative")
	if err == nil {
		t.Fatal("deleted a preset that a model's defaults name")
	}
	if !strings.Contains(err.Error(), "qwen/x") {
		t.Errorf("the refusal should name the model: %v", err)
	}
	if err := s.SetDefaults("qwen/x", ModelDefaults{}); err != nil {
		t.Fatal(err)
	}
	if err := s.DeletePreset("creative"); err != nil {
		t.Errorf("an unreferenced preset should delete: %v", err)
	}
}

func visionSupervisor(t *testing.T) *Supervisor {
	s := &Supervisor{cfg: Config{DataDir: t.TempDir()}}
	s.cat = catalog.NewFromModels(map[string]catalog.Model{
		"seer":   {Key: "seer", Format: "gguf", Projector: "/models/mmproj.gguf"},
		"scribe": {Key: "scribe", Format: "gguf"},
	})
	return s
}

// Default image loads need one slot and enough context for an image prompt.
// Speculation is independent; do not restore the blanket mtmd workaround.
func TestVisionModelsLoadSafeByDefault(t *testing.T) {
	s := visionSupervisor(t)
	got, err := s.resolveSettings("seer", "", runtime.Settings{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Parallel == nil || *got.Parallel != 1 {
		t.Errorf("a vision model should default to one slot: %+v", got.Parallel)
	}
	if got.SpecMode != nil {
		t.Errorf("a vision model should be left to the runtime's own speculation "+
			"choice, which is its MTP head: %+v", *got.SpecMode)
	}
	// Without an explicit context, reducing slots selected the 8k runtime
	// default and rejected an 18,107-token image prompt.
	if got.ContextLength == nil || *got.ContextLength != 32768 {
		t.Errorf("a vision model needs room for an image prompt, got %+v", got.ContextLength)
	}
}

func TestTextModelsIgnoreVisionDefaults(t *testing.T) {
	s := visionSupervisor(t)
	if err := s.SetVisionDefaults(runtime.Settings{Parallel: ptr(1)}); err != nil {
		t.Fatal(err)
	}
	got, err := s.resolveSettings("scribe", "", runtime.Settings{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Parallel != nil {
		t.Errorf("a text model took the vision default: %+v", *got.Parallel)
	}
}

func TestVisionDefaultsAreEditable(t *testing.T) {
	s := visionSupervisor(t)
	if err := s.SetVisionDefaults(runtime.Settings{Parallel: ptr(4)}); err != nil {
		t.Fatal(err)
	}
	got, err := s.resolveSettings("seer", "", runtime.Settings{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Parallel == nil || *got.Parallel != 4 {
		t.Fatalf("the node's own slot count should win: %+v", got.Parallel)
	}
	if err := s.SetVisionDefaults(runtime.Settings{}); err != nil {
		t.Fatal(err)
	}
	got, err = s.resolveSettings("seer", "", runtime.Settings{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Parallel == nil || *got.Parallel != 1 {
		t.Fatalf("clearing should restore one slot, got %+v", got.Parallel)
	}
}

func TestVisionDefaultsLoseToEverythingElse(t *testing.T) {
	s := visionSupervisor(t)
	if err := s.SetDefaults("seer", ModelDefaults{Settings: runtime.Settings{Parallel: ptr(2)}}); err != nil {
		t.Fatal(err)
	}
	got, err := s.resolveSettings("seer", "", runtime.Settings{})
	if err != nil {
		t.Fatal(err)
	}
	if *got.Parallel != 2 {
		t.Errorf("the model's own defaults should win: %d", *got.Parallel)
	}
	got, err = s.resolveSettings("seer", "", runtime.Settings{Parallel: ptr(3)})
	if err != nil {
		t.Fatal(err)
	}
	if *got.Parallel != 3 {
		t.Errorf("the load request should win: %d", *got.Parallel)
	}
}

// The context is per node like the slots are, and the same rule applies: what
// the node saved wins over the default, and the model's own defaults win over
// the node's.
func TestVisionContextIsPerNodeAndOverridable(t *testing.T) {
	s := visionSupervisor(t)
	if err := s.SetVisionDefaults(runtime.Settings{ContextLength: ptr(65536), Parallel: ptr(2)}); err != nil {
		t.Fatal(err)
	}
	got, err := s.resolveSettings("seer", "", runtime.Settings{})
	if err != nil {
		t.Fatal(err)
	}
	if *got.ContextLength != 65536 || *got.Parallel != 2 {
		t.Fatalf("the node's own values should win: ctx=%v slots=%v", got.ContextLength, got.Parallel)
	}
	if err := s.SetDefaults("seer", ModelDefaults{Settings: runtime.Settings{ContextLength: ptr(16384)}}); err != nil {
		t.Fatal(err)
	}
	got, err = s.resolveSettings("seer", "", runtime.Settings{})
	if err != nil {
		t.Fatal(err)
	}
	if *got.ContextLength != 16384 {
		t.Errorf("the model's own context should win over the node's: %d", *got.ContextLength)
	}
	if *got.Parallel != 2 {
		t.Errorf("but the node's slot count still applies: %d", *got.Parallel)
	}
}

// A load that asked for the model without its projector is not image work, so
// the node's vision defaults must not apply - otherwise -vision off still
// inherits the one slot that exists only for images.
func TestVisionOffSkipsTheVisionDefaults(t *testing.T) {
	s := visionSupervisor(t)
	no := false
	got, err := s.resolveSettings("seer", "", runtime.Settings{Vision: &no})
	if err != nil {
		t.Fatal(err)
	}
	if got.Parallel != nil {
		t.Errorf("the one-slot vision default should not apply: %d", *got.Parallel)
	}
	if got.SpecMode != nil {
		t.Errorf("speculation should be left to its own rules: %q", *got.SpecMode)
	}
	if got.ContextLength != nil {
		t.Errorf("the 32k image context should not apply: %d", *got.ContextLength)
	}
	yes := true
	got, err = s.resolveSettings("seer", "", runtime.Settings{Vision: &yes})
	if err != nil {
		t.Fatal(err)
	}
	if got.Parallel == nil || *got.Parallel != 1 {
		t.Errorf("-vision on is image work: %+v", got.Parallel)
	}
}
