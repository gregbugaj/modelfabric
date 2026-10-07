package main

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func write(t *testing.T, p, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func xdg(t *testing.T) (cache, state, data string) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("exercises the XDG layout")
	}
	root := t.TempDir()
	cache, state, data = filepath.Join(root, "cache"), filepath.Join(root, "state"), filepath.Join(root, "data")
	t.Setenv("XDG_CACHE_HOME", cache)
	t.Setenv("XDG_STATE_HOME", state)
	t.Setenv("XDG_DATA_HOME", data)
	t.Setenv("MFSH_STATE_DIR", "")
	t.Setenv("MFSH_LOG_DIR", "")
	return
}

func TestMigrateLegacyState(t *testing.T) {
	cache, state, data := xdg(t)
	legacy := filepath.Join(cache, "llm-z")
	write(t, filepath.Join(legacy, "instances", "q.json"), "owner")
	write(t, filepath.Join(legacy, "logs", "inst-1.log"), "engine")
	write(t, filepath.Join(legacy, "node.log"), "node")
	write(t, filepath.Join(legacy, "bench", "swe", "runs", "r.json"), "run")

	if got := defaultStateDir(); got != legacy {
		t.Fatalf("before migration: state dir %s, want %s", got, legacy)
	}
	migrateLegacyState(quietLog())

	dir := filepath.Join(state, "modelfabric")
	if defaultStateDir() != dir || logDir() != filepath.Join(dir, "logs") {
		t.Fatalf("after migration: state %s, logs %s", defaultStateDir(), logDir())
	}
	for p, want := range map[string]string{
		filepath.Join(dir, "instances", "q.json"):                            "owner",
		filepath.Join(dir, "logs", "inst-1.log"):                             "engine",
		filepath.Join(dir, "logs", "node.log"):                               "node",
		filepath.Join(data, "modelfabric", "bench", "swe", "runs", "r.json"): "run",
	} {
		if b, err := os.ReadFile(p); err != nil || string(b) != want {
			t.Errorf("%s: %q, %v", p, b, err)
		}
	}
	if exists(legacy) {
		t.Error("the cache directory should be gone after the move")
	}
	if nodeLogPath() != filepath.Join(dir, "logs", "node.log") {
		t.Errorf("node log at %s", nodeLogPath())
	}
	migrateLegacyState(quietLog())
}

func TestFreshInstallUsesTheStateDirectory(t *testing.T) {
	_, state, _ := xdg(t)
	if got := defaultStateDir(); got != filepath.Join(state, "modelfabric") {
		t.Fatalf("state dir %s", got)
	}
	migrateLegacyState(quietLog())
}
