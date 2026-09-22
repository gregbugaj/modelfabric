package catalog

import (
	"os"
	"path/filepath"
	"testing"
)

// writeMLXModel lays out a model directory the way Hugging Face ships one.
func writeMLXModel(t *testing.T, dir, config string, shards ...string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if config != "" {
		if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(config), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, s := range shards {
		if err := os.WriteFile(filepath.Join(dir, s), make([]byte, 1024), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

const qwenMLXConfig = `{
  "model_type": "qwen3_5",
  "architectures": ["Qwen3MoeForCausalLM"],
  "max_position_embeddings": 262144,
  "num_hidden_layers": 48,
  "num_local_experts": 128,
  "num_nextn_predict_layers": 1,
  "quantization": {"bits": 4, "group_size": 64}
}`

func TestScanIndexesAnMLXDirectoryAsOneModel(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "lmstudio-community", "Qwen3.8-27B-MLX-4bit")
	writeMLXModel(t, dir, qwenMLXConfig,
		"model-00001-of-00002.safetensors", "model-00002-of-00002.safetensors",
		"tokenizer_config.json", "chat_template.jinja")

	c, err := ScanRoots(root)
	if err != nil {
		t.Fatalf("ScanRoots: %v", err)
	}
	if got := len(c.Models()); got != 1 {
		t.Fatalf("indexed %d models, want 1: the directory is one model, not one per shard", got)
	}
	m := c.Models()[0]
	if m.Format != "mlx" {
		t.Errorf("Format = %q, want mlx", m.Format)
	}
	if m.Path != dir {
		t.Errorf("Path = %q, want the directory %q", m.Path, dir)
	}
	if m.Quantization != "4bit" {
		t.Errorf("Quantization = %q, want 4bit", m.Quantization)
	}
	if m.MaxContextLength != 262144 || m.Layers != 48 || m.Experts != 128 {
		t.Errorf("config.json not read: ctx=%d layers=%d experts=%d", m.MaxContextLength, m.Layers, m.Experts)
	}
	if m.DraftLayers != 1 {
		t.Errorf("DraftLayers = %d, want 1", m.DraftLayers)
	}
	if m.Architecture != "qwen3_5" {
		t.Errorf("Architecture = %q, want qwen3_5", m.Architecture)
	}
	// Every file in the directory is loaded, so the size is all of them: the
	// two 1 KiB shards plus the small config and tokenizer files.
	if m.SizeBytes <= 2048 {
		t.Errorf("SizeBytes = %d, want more than the shards alone", m.SizeBytes)
	}
	if m.Publisher != "lmstudio-community" {
		t.Errorf("Publisher = %q", m.Publisher)
	}
}

// A directory without weights, or without a config, is not a model: those are
// the tokenizer and processor directories that sit beside real ones.
func TestScanIgnoresIncompleteDirectories(t *testing.T) {
	root := t.TempDir()
	writeMLXModel(t, filepath.Join(root, "org", "config-only"), `{"model_type":"x"}`)
	writeMLXModel(t, filepath.Join(root, "org", "weights-only"), "", "model.safetensors")

	c, err := ScanRoots(root)
	if err != nil {
		t.Fatalf("ScanRoots: %v", err)
	}
	if got := len(c.Models()); got != 0 {
		t.Fatalf("indexed %d models, want 0: %+v", got, c.Models())
	}
}

// A Mac holds the same model in both formats, as LM Studio does. Both must stay
// loadable, and which one a bare hub id means must not depend on walk order.
func TestBothFormatsOfOneModelStayAddressable(t *testing.T) {
	root := t.TempDir()
	mlxDir := filepath.Join(root, "lmstudio-community", "Qwen3.8-27B-MLX-4bit")
	writeMLXModel(t, mlxDir, qwenMLXConfig, "model-00001-of-00001.safetensors")
	ggufDir := filepath.Join(root, "lmstudio-community", "Qwen3.8-27B-GGUF")
	if err := os.MkdirAll(ggufDir, 0o755); err != nil {
		t.Fatal(err)
	}
	gguf := filepath.Join(ggufDir, "Qwen3.8-27B-Q4_K_M.gguf")
	if err := os.WriteFile(gguf, make([]byte, 4096), 0o644); err != nil {
		t.Fatal(err)
	}

	c, err := ScanRoots(root)
	if err != nil {
		t.Fatalf("ScanRoots: %v", err)
	}
	// Without hub metadata each keeps its own path-derived key, so both are
	// models in their own right.
	if got := len(c.Models()); got != 2 {
		t.Fatalf("indexed %d models, want 2", got)
	}
	for _, ref := range []string{
		"lmstudio-community/Qwen3.8-27B-MLX-4bit",
		"lmstudio-community/Qwen3.8-27B-GGUF/Qwen3.8-27B-Q4_K_M",
	} {
		m, err := c.Resolve(ref)
		if err != nil {
			t.Fatalf("Resolve(%q): %v", ref, err)
		}
		if m.PathKey != ref {
			t.Errorf("Resolve(%q) returned %q", ref, m.PathKey)
		}
	}
}
