package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Like get, import must stop on a broken config before it writes to a fallback
// disk. With -mode move, swallowing the error also removes the source file.
func TestImportRejectsBrokenConfigBeforeMovingFiles(t *testing.T) {
	for _, tc := range []struct {
		name   string
		config string
	}{
		{"malformed config", `{"models_root":`},
		{"invalid config", `{"cors_origins":["not-an-origin"]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			cfg := filepath.Join(dir, "config.json")
			src := filepath.Join(dir, "model.gguf")
			root := filepath.Join(dir, "models")
			for path, body := range map[string]string{cfg: tc.config, src: "GGUFtest"} {
				if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv("MFSH_CONFIG", cfg)
			t.Setenv("MFSH_MODELS", root)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
			defer upstream.Close()
			err := importCmd([]string{src, "-mode", "move", "-addr", upstream.URL})
			if err == nil || !strings.Contains(err.Error(), "read config") {
				t.Errorf("got %v, want config error", err)
			}
			if _, err := os.Stat(src); err != nil {
				t.Errorf("source was removed: %v", err)
			}
			if _, err := os.Stat(root); !os.IsNotExist(err) {
				t.Errorf("fallback root was touched: %v", err)
			}
		})
	}
}

func TestImportWithMissingConfigUsesDefaultRoot(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "model.gguf")
	root := filepath.Join(dir, "models")
	t.Setenv("MFSH_CONFIG", filepath.Join(dir, "absent.json"))
	t.Setenv("MFSH_MODELS", root)
	if err := os.WriteFile(src, []byte("GGUFtest"), 0o600); err != nil {
		t.Fatal(err)
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	defer upstream.Close()
	if err := importCmd([]string{src, "-mode", "copy", "-addr", upstream.URL}); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(root, "local", "model", "model.gguf"))
	if err != nil || string(b) != "GGUFtest" {
		t.Fatalf("imported file = %q, %v", b, err)
	}
}
