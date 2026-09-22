package runtime

import (
	"strings"
	"testing"

	"github.com/gregbugaj/modelfabric/internal/catalog"
)

func mlxDef() *Definition {
	return &Definition{
		Name: "mlx-lm-test", Engine: "mlx", Entrypoint: "/pkg/.venv/bin/mlx_lm.server",
		ContextLength: 8192, Parallel: 2, CtxCheckpoints: 4, BatchSize: 2048,
		// llama.cpp-only defaults, which an MLX launch must not try to pass on.
		GPULayers: 99, FlashAttention: true, CacheTypeK: "f16", LoadMode: "mmap+mlock",
	}
}

func mlxModel() catalog.Model {
	return catalog.Model{
		Key: "qwen/qwen3.8-27b", Format: "mlx", Quantization: "4bit",
		Path: "/models/lmstudio-community/Qwen3.8-27B-MLX-4bit", MaxContextLength: 262144,
	}
}

func TestMLXArgvUsesTheModelDirectoryAndConcurrency(t *testing.T) {
	d, m := mlxDef(), mlxModel()
	a := mlxLM{}.Apply(d, m, Requested{})
	argv := strings.Join(mlxLM{}.Argv(d, m, a, "127.0.0.1", 18000), " ")

	for _, want := range []string{
		"--model /models/lmstudio-community/Qwen3.8-27B-MLX-4bit",
		"--host 127.0.0.1", "--port 18000",
		"--decode-concurrency 2", "--prompt-concurrency 2",
		"--prompt-cache-size 4", "--prefill-step-size 2048",
	} {
		if !strings.Contains(argv, want) {
			t.Errorf("argv is missing %q\ngot: %s", want, argv)
		}
	}
	// llama.cpp's flags do not exist here, and passing one would abort the
	// launch with an argparse error rather than degrade.
	for _, never := range []string{"-ngl", "--flash-attn", "--cache-type-k", "--load-mode", "-c ", "--alias", "-m "} {
		if strings.Contains(argv, never) {
			t.Errorf("argv contains llama.cpp flag %q\ngot: %s", never, argv)
		}
	}
}

// The context an MLX engine serves is the model's own: there is no flag to cap
// it, so reporting anything smaller would claim a limit nothing enforces.
func TestMLXContextIsTheModelsOwn(t *testing.T) {
	d, m := mlxDef(), mlxModel()
	ctx := 4096
	a := mlxLM{}.Apply(d, m, Requested{Settings: Settings{ContextLength: &ctx}})
	if a.ContextLength != m.MaxContextLength {
		t.Errorf("ContextLength = %d, want the model's %d", a.ContextLength, m.MaxContextLength)
	}
	if a.KVCacheTokens != 0 {
		t.Errorf("KVCacheTokens = %d, want 0: MLX allocates KV per request", a.KVCacheTokens)
	}
}

// Requests must carry the one id mlx-lm maps to the model it was started with;
// anything else makes it reach for Hugging Face.
func TestMLXServedModelIsDefaultModel(t *testing.T) {
	mlx, llama := mlxLM{}, llamaCPP{}
	if got := mlx.ServedModel(mlxModel()); got != MLXServedModel {
		t.Errorf("ServedModel = %q, want %q", got, MLXServedModel)
	}
	if got := llama.ServedModel(mlxModel()); got != "qwen/qwen3.8-27b" {
		t.Errorf("llama.cpp ServedModel = %q, want the catalog key", got)
	}
}

func TestMLXTraitsSayWhatItCannotBeAsked(t *testing.T) {
	if tr := EngineTraits("mlx"); !tr.FixedModels || !tr.NoMetrics {
		t.Errorf("mlx traits = %+v, want both set", tr)
	}
	if tr := EngineTraits("llama.cpp"); tr.FixedModels || tr.NoMetrics {
		t.Errorf("llama.cpp traits = %+v, want the zero value", tr)
	}
}

// A load must not be handed to an engine that cannot read the weights.
func TestRuntimeSelectionFollowsTheWeightsFormat(t *testing.T) {
	gguf := &Definition{Name: "llama", Engine: "llama.cpp"}
	mlx := &Definition{Name: "mlx", Engine: "mlx"}
	for _, c := range []struct {
		d      *Definition
		format string
		want   bool
	}{
		{gguf, "gguf", true}, {gguf, "mlx", false}, {gguf, "safetensors", false},
		{mlx, "mlx", true}, {mlx, "safetensors", true}, {mlx, "gguf", false},
		// An unknown format is nobody's to refuse, and no format means any.
		{gguf, "", true}, {mlx, "", true}, {gguf, "awq", true},
	} {
		if got := c.d.LoadsFormat(c.format); got != c.want {
			t.Errorf("%s.LoadsFormat(%q) = %v, want %v", c.d.Engine, c.format, got, c.want)
		}
	}
}
