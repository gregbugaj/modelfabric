package catalog

import (
	"strings"
	"testing"
)

func emptyCatalog() *Catalog {
	return &Catalog{
		models:   map[string]Model{},
		aliases:  map[string]string{},
		variants: map[string]Model{},
	}
}

// A catalog holding one model in two shapes: the same weights as GGUF and as
// MLX, which is exactly what an LM Studio tree looks like on a Mac.
func twoFormats() *Catalog {
	c := emptyCatalog()
	c.add(Model{Key: "qwen/qwen3.8-27b", PathKey: "pub/Qwen3.8-27B-GGUF", Format: "gguf"})
	c.add(Model{Key: "qwen/qwen3.8-27b", PathKey: "pub/Qwen3.8-27B-MLX-4bit", Format: "mlx"})
	return c
}

// The bug this fixes: asking for the MLX weights must not hand back the GGUF
// just because they share a name.
func TestResolveFormatPicksTheVariant(t *testing.T) {
	c := twoFormats()

	mlx, err := c.ResolveFormat("qwen/qwen3.8-27b", "mlx")
	if err != nil {
		t.Fatalf("mlx: %v", err)
	}
	if mlx.Format != "mlx" || mlx.PathKey != "pub/Qwen3.8-27B-MLX-4bit" {
		t.Errorf("got %s/%s, want the MLX variant", mlx.Format, mlx.PathKey)
	}

	gguf, err := c.ResolveFormat("qwen/qwen3.8-27b", "gguf")
	if err != nil {
		t.Fatalf("gguf: %v", err)
	}
	if gguf.Format != "gguf" {
		t.Errorf("got %s, want gguf", gguf.Format)
	}

	// No format asked for keeps the old behaviour exactly: the primary.
	first, err := c.ResolveFormat("qwen/qwen3.8-27b", "")
	if err != nil || first.Format != "gguf" {
		t.Errorf("got %s (%v), want the primary unchanged", first.Format, err)
	}
}

// Asking for a format that is not on this machine has to say so, and say what
// is here instead — the alternative is loading the wrong weights silently.
func TestResolveFormatMissingSaysWhatIsHere(t *testing.T) {
	c := emptyCatalog()
	c.add(Model{Key: "qwen/qwen3-0.6b", PathKey: "pub/Qwen3-0.6B-GGUF", Format: "gguf"})

	_, err := c.ResolveFormat("qwen/qwen3-0.6b", "mlx")
	if err == nil {
		t.Fatal("want an error for a format that is not here")
	}
	for _, want := range []string{"mlx", "gguf"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q should mention %q", err, want)
		}
	}
}

// Case is not something to fail over: "MLX" and "mlx" name one thing.
func TestResolveFormatIgnoresCase(t *testing.T) {
	m, err := twoFormats().ResolveFormat("qwen/qwen3.8-27b", "MLX")
	if err != nil || m.Format != "mlx" {
		t.Errorf("got %s (%v), want the mlx variant", m.Format, err)
	}
}

// Every shape of one model, so a caller can show the choice rather than
// leaving it to be guessed at.
func TestVariantsOf(t *testing.T) {
	vs := twoFormats().VariantsOf("qwen/qwen3.8-27b")
	if len(vs) != 2 {
		t.Fatalf("got %d variants, want 2", len(vs))
	}
	// A single-file model returns itself, so callers need no special case.
	c := emptyCatalog()
	c.add(Model{Key: "solo", PathKey: "solo", Format: "gguf"})
	if got := c.VariantsOf("solo"); len(got) != 1 {
		t.Errorf("got %d, want the model itself", len(got))
	}
}
