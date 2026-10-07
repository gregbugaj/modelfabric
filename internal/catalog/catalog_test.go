package catalog

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// A split GGUF is indexed as its first shard, but its size is the whole set:
// reporting 12 GB for a 60 GB model misleads the disk summary and every
// placement decision that asks whether a node has room.
func TestShardedModelReportsEveryShardsBytes(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "lmstudio-community", "Big-GGUF")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for i, size := range []int{4096, 2048, 1024} {
		name := fmt.Sprintf("big-%05d-of-00003.gguf", i+1)
		if err := os.WriteFile(filepath.Join(dir, name), make([]byte, size), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	c, err := Scan(root)
	if err != nil {
		t.Fatal(err)
	}
	models := c.Models()
	if len(models) != 1 {
		t.Fatalf("a split model should be one entry, got %d", len(models))
	}
	if want := int64(4096 + 2048 + 1024); models[0].SizeBytes != want {
		t.Errorf("size %d, want %d (every shard)", models[0].SizeBytes, want)
	}
}

// Only one projector per directory was kept, so with two multimodal models in
// one directory whichever was walked last was attached to both; and a model
// answers as if it can see with a projector that is not its own.
func TestProjectorIsMatchedToItsModel(t *testing.T) {
	one, ok := projectorFor([]string{"/m/mmproj-f16.gguf"}, "/m/qwen-vl-Q4_K_M.gguf")
	if !ok || one != "/m/mmproj-f16.gguf" {
		t.Errorf("a lone projector should be used: %q %v", one, ok)
	}
	pair := []string{"/m/qwen-vl.mmproj-f16.gguf", "/m/gemma-vision.mmproj-f16.gguf"}
	got, ok := projectorFor(pair, "/m/gemma-vision-Q4_K_M.gguf")
	if !ok || got != "/m/gemma-vision.mmproj-f16.gguf" {
		t.Errorf("picked %q for gemma-vision, want its own projector", got)
	}
	if got, ok := projectorFor(pair, "/m/llama-3-Q4_K_M.gguf"); ok {
		t.Errorf("attached an unrelated projector %q", got)
	}
}
