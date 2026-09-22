package main

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// An entrypoint was started with -config ~/.config/ModelFabric/config.json, a
// capitalisation its file never had, and ran on defaults (a GPU node, no
// public listener) without a word. A path asked for must exist.
func TestRequireExplicitConfig(t *testing.T) {
	dir := t.TempDir()
	there := filepath.Join(dir, "config.json")
	os.WriteFile(there, []byte("{}"), 0o644)
	missing := filepath.Join(dir, "Config.json")
	tests := []struct {
		name    string
		args    []string
		env     string
		path    string
		wantErr bool
	}{
		{name: "the default path may be absent: a fresh install", path: missing},
		{name: "an explicit -config that exists is fine", args: []string{"-config", there}, path: there},
		{name: "an explicit -config that is missing is refused", args: []string{"-config", missing}, path: missing, wantErr: true},
		{name: "MFSH_CONFIG naming a missing file is refused", env: missing, path: missing, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("MFSH_CONFIG", tc.env)
			fs := flag.NewFlagSet("serve", flag.ContinueOnError)
			fs.String("config", tc.path, "")
			if err := fs.Parse(tc.args); err != nil {
				t.Fatal(err)
			}
			err := requireExplicitConfig(fs, tc.path)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, want error %v", err, tc.wantErr)
			}
			if err != nil && !strings.Contains(err.Error(), tc.path) {
				t.Fatalf("the error does not name the path: %v", err)
			}
		})
	}
}

// A unit outlives the build that wrote it; the default path frozen into it
// is the one that build had. Only a path chosen on purpose is written.
func TestServiceUnitConfigArg(t *testing.T) {
	t.Setenv("MFSH_CONFIG", "")
	if u := serviceUnit("/bin/mfsh", defaultConfigPath()); !strings.Contains(u, "ExecStart=\"/bin/mfsh\" serve\n") {
		t.Fatalf("default path written into the unit:\n%s", u)
	}
	if u := serviceUnit("/bin/mfsh", "/etc/mfsh/other.json"); !strings.Contains(u, "serve -config \"/etc/mfsh/other.json\"\n") {
		t.Fatalf("a chosen path was dropped:\n%s", u)
	}
}
