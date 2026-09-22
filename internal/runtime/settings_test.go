package runtime

import (
	"strings"
	"testing"

	"github.com/gregbugaj/modelfabric/internal/catalog"
)

func ptr[T any](v T) *T { return &v }

// The KV cache must follow the context and slots actually requested. It was
// once sized from the runtime defaults before the request was applied.
func TestKVCacheFollowsTheRequest(t *testing.T) {
	d := testDef()
	a := llamaCPP{}.Apply(d, catalog.Model{Key: "m", Path: "/m.gguf"},
		Requested{Settings: Settings{ContextLength: ptr(4096), Parallel: ptr(8)}})
	if a.KVCacheTokens != 4096*8 {
		t.Fatalf("kv_cache_tokens = %d, want %d", a.KVCacheTokens, 4096*8)
	}
}

func TestMergeLaterWins(t *testing.T) {
	base := Settings{Temperature: ptr(0.7), TopK: ptr(40), TemplateKwargs: map[string]any{"a": 1, "b": 1}}
	over := Settings{Temperature: ptr(1.0), MinP: ptr(0.0), TemplateKwargs: map[string]any{"b": 2}}
	got := base.Merge(over)
	if *got.Temperature != 1.0 || *got.TopK != 40 || *got.MinP != 0 {
		t.Fatalf("merge = %+v", got)
	}
	if got.TemplateKwargs["a"] != 1 || got.TemplateKwargs["b"] != 2 {
		t.Fatalf("template kwargs = %v", got.TemplateKwargs)
	}
	if !(Settings{}).IsZero() || got.IsZero() {
		t.Fatal("IsZero wrong")
	}
}

func TestValidateRefusesManagedFlagsAndBadValues(t *testing.T) {
	for _, s := range []Settings{
		{ExtraArgs: []string{"--host", "0.0.0.0"}},
		{ExtraArgs: []string{"--port=9999"}},
		{ExtraArgs: []string{"-m", "/other.gguf"}},
		{OffloadRatio: ptr(1.5)},
		{GPULayers: ptr(10), OffloadRatio: ptr(0.5)},
		{SpecMode: ptr("fast")},
		{SpecMode: ptr("draft")}, // no draft model
		{DraftMin: ptr(5), DraftMax: ptr(3)},
		{CacheTypeK: ptr("q3_k")},
		{BatchSize: ptr(256), UBatchSize: ptr(512)},
		{TopP: ptr(2.0)},
	} {
		if err := s.Validate(); err == nil {
			t.Errorf("accepted %+v", s)
		}
	}
	ok := Settings{ExtraArgs: []string{"--mlock", "--no-warmup"}, CacheTypeK: ptr("q8_0"), SpecMode: ptr("mtp"),
		DraftMax: ptr(4), DraftMin: ptr(1), DraftPMin: ptr(0.6)}
	if err := ok.Validate(); err != nil {
		t.Fatalf("rejected valid settings: %v", err)
	}
}

// Every LM Studio setting reaches the engine's command line as llama.cpp's flag.
func TestSettingsReachTheCommandLine(t *testing.T) {
	d := testDef()
	moe := catalog.Model{Key: "moe", Path: "/moe.gguf", Layers: 48, Experts: 128, DraftLayers: 1, SizeBytes: 1}
	s := Settings{
		OffloadRatio: ptr(0.5), CPUMoERatio: ptr(0.25), KVOffload: ptr(false),
		CacheTypeK: ptr("q8_0"), CacheTypeV: ptr("q8_0"), BatchSize: ptr(1024), UBatchSize: ptr(256),
		RopeFreqBase: ptr(1e6), ThreadsBatch: ptr(12), Seed: ptr(42), CacheReuse: ptr(256),
		KeepInMemory: ptr(false), TryMmap: ptr(true),
		SpecMode: ptr("mtp"), DraftMax: ptr(4), DraftMin: ptr(2), DraftPMin: ptr(0.75),
		Temperature: ptr(0.6), FrequencyPenalty: ptr(0.1), EnableThinking: ptr(false),
		ExtraArgs: []string{"--no-warmup"},
	}
	got := argvString(llamaCPP{}.Argv(d, moe, llamaCPP{}.Apply(d, moe, Requested{Settings: s}), "127.0.0.1", 8000)) + " "
	for _, want := range []string{
		"-ngl 24 ", "--n-cpu-moe 12 ", "--no-kv-offload ", "--cache-type-k q8_0 ", "--cache-type-v q8_0 ",
		"--batch-size 1024 ", "--ubatch-size 256 ", "--rope-freq-base 1000000 ", "--threads-batch 12 ",
		"--seed 42 ", "--cache-reuse 256 ", "--load-mode mmap ",
		"--spec-type draft-mtp ", "--spec-draft-n-max 4 ", "--spec-draft-n-min 2 ", "--spec-draft-p-min 0.75 ",
		"--temp 0.6 ", "--frequency-penalty 0.1 ", `"enable_thinking":false`, "--no-warmup ",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("argv missing %q\n  got: %s", want, got)
		}
	}
	// ExtraArgs come last, after the runtime's own.
	if !strings.HasSuffix(strings.TrimSpace(got), "--no-warmup") {
		t.Errorf("extra args are not last: %s", got)
	}
}

func TestDraftModelAndSpeculationModes(t *testing.T) {
	d := testDef()
	plain := catalog.Model{Key: "big", Path: "/big.gguf"}
	a := llamaCPP{}.Apply(d, plain, Requested{Settings: Settings{DraftModel: ptr("small")}, DraftModelPath: "/small.gguf"})
	got := argvString(llamaCPP{}.Argv(d, plain, a, "127.0.0.1", 8000))
	if a.SpecType != "draft-simple" || !strings.Contains(got, "--spec-draft-model /small.gguf") {
		t.Fatalf("draft model not used: %s / %s", a.SpecType, got)
	}
	off := llamaCPP{}.Apply(d, catalog.Model{Key: "h", Path: "/h.gguf", DraftLayers: 1},
		Requested{Settings: Settings{SpecMode: ptr("off"), DraftMax: ptr(5)}})
	if off.Speculative || off.DraftMax != 0 {
		t.Fatalf("spec_mode off still speculates: %+v", off)
	}
	// mtp without a head does nothing rather than inventing one.
	none := llamaCPP{}.Apply(d, plain, Requested{Settings: Settings{SpecMode: ptr("mtp")}})
	if none.Speculative {
		t.Fatal("mtp requested for a model without a draft head")
	}
}
