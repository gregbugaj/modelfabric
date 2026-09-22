package catalog

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestModelFiles(t *testing.T) {
	touch := func(t *testing.T, path string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range []struct {
		name  string
		setup []string // files under the model directory
		model func(dir string) Model
		want  []string
	}{
		{
			name:  "a single file is just itself",
			setup: []string{"m-Q4_K_M.gguf", "other-Q8_0.gguf"},
			model: func(d string) Model { return Model{Path: filepath.Join(d, "m-Q4_K_M.gguf")} },
			want:  []string{"m-Q4_K_M.gguf"},
		},
		{
			// A load names only the first shard. Copying just that moved one
			// piece of a model that then could not load.
			name: "a split GGUF carries every shard, and not another model's",
			setup: []string{"big-00001-of-00003.gguf", "big-00002-of-00003.gguf", "big-00003-of-00003.gguf",
				"bigger-00002-of-00002.gguf"},
			model: func(d string) Model { return Model{Path: filepath.Join(d, "big-00001-of-00003.gguf")} },
			want:  []string{"big-00001-of-00003.gguf", "big-00002-of-00003.gguf", "big-00003-of-00003.gguf"},
		},
		{
			name:  "the projector and model.yaml travel with the weights",
			setup: []string{"v-Q4_K_M.gguf", "mmproj-v-F16.gguf", "model.yaml"},
			model: func(d string) Model {
				return Model{Path: filepath.Join(d, "v-Q4_K_M.gguf"), Projector: filepath.Join(d, "mmproj-v-F16.gguf")}
			},
			want: []string{"v-Q4_K_M.gguf", "mmproj-v-F16.gguf", "model.yaml"},
		},
		{
			name:  "a directory model is every file under it",
			setup: []string{"config.json", "model.safetensors", "tok/tokenizer.json"},
			model: func(d string) Model { return Model{Path: d} },
			want:  []string{"config.json", "model.safetensors", "tok/tokenizer.json"},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			d := t.TempDir()
			for _, f := range c.setup {
				touch(t, filepath.Join(d, f))
			}
			dir, got, err := ModelFiles(c.model(d))
			if err != nil {
				t.Fatal(err)
			}
			if dir != d {
				t.Errorf("dir %q, want %q", dir, d)
			}
			if !slices.Equal(got, c.want) {
				t.Errorf("got %v, want %v", got, c.want)
			}
		})
	}
}
