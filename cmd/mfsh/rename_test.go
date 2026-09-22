package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The rename moves each directory and leaves a link, because checksum pins
// and configs record absolute paths under the old name. Without the link every
// pinned model would fail verification on its next load.
func TestMoveRenamed(t *testing.T) {
	for _, c := range []struct {
		name     string
		setup    func(t *testing.T, from, to string)
		wantMsg  string // substring; "" means nothing to report
		wantFile string // where models/m.gguf must be readable afterwards
	}{
		{
			name: "an old directory moves and the old path still resolves",
			setup: func(t *testing.T, from, _ string) {
				write(t, filepath.Join(from, "models", "m.gguf"), "x")
			},
			wantMsg:  "moved",
			wantFile: "both",
		},
		{
			name:    "nothing old is nothing to do",
			setup:   func(*testing.T, string, string) {},
			wantMsg: "",
		},
		{
			name: "a second run leaves the link alone",
			setup: func(t *testing.T, from, to string) {
				write(t, filepath.Join(to, "models", "m.gguf"), "x")
				if err := os.Symlink(to, from); err != nil {
					t.Fatal(err)
				}
			},
			wantMsg:  "",
			wantFile: "both",
		},
		{
			// Two trees of pins and ownership records cannot be merged safely.
			name: "both existing touches neither",
			setup: func(t *testing.T, from, to string) {
				write(t, filepath.Join(from, "models", "m.gguf"), "x")
				write(t, filepath.Join(to, "other"), "x")
			},
			wantMsg:  "both",
			wantFile: "from",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			from, to := filepath.Join(dir, ".llmz"), filepath.Join(dir, ".modelfabric")
			c.setup(t, from, to)
			msg := moveRenamed(from, to)
			if c.wantMsg == "" && msg != "" || c.wantMsg != "" && !strings.Contains(msg, c.wantMsg) {
				t.Errorf("message %q, want %q", msg, c.wantMsg)
			}
			switch c.wantFile {
			case "both":
				for _, root := range []string{from, to} {
					if _, err := os.Stat(filepath.Join(root, "models", "m.gguf")); err != nil {
						t.Errorf("not readable under %s: %v", root, err)
					}
				}
				if fi, err := os.Lstat(from); err != nil || fi.Mode()&os.ModeSymlink == 0 {
					t.Errorf("the old path should be a link: %v", err)
				}
			case "from":
				if _, err := os.Stat(filepath.Join(from, "models", "m.gguf")); err != nil {
					t.Errorf("the old tree was disturbed: %v", err)
				}
			}
		})
	}
}

// A location chosen by environment variable is the operator's; the rename
// does not move what they pointed somewhere on purpose.
func TestMigrateRenameRespectsTheOverride(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, ".local", "state"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, ".local", "share"))
	t.Setenv("MFSH_HOME", filepath.Join(home, "elsewhere"))
	write(t, filepath.Join(home, ".llmz", "models", "m.gguf"), "x")
	write(t, filepath.Join(home, ".config", "llm-z", "config.json"), "x")

	migrateRename()

	if fi, err := os.Lstat(filepath.Join(home, ".llmz")); err != nil || fi.Mode()&os.ModeSymlink != 0 {
		t.Errorf("~/.llmz moved despite MFSH_HOME: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, ".config", "modelfabric", "config.json")); err != nil {
		t.Errorf("the config was not moved: %v", err)
	}
}

// Each directory the rename moves has to be the one the code then reads. The
// automated rename once turned the config path into ~/.config/ModelFabric
// while the migration moved it to ~/.config/modelfabric, so every node would
// have started on defaults and ignored its config.
func TestRenamedDirsAreWhereTheCodeLooks(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, ".local", "state"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, ".local", "share"))
	for _, k := range []string{"MFSH_HOME", "MFSH_CONFIG", "MFSH_STATE_DIR", "MFSH_LOG_DIR", "MFSH_MODELS", "MFSH_RUNTIMES"} {
		t.Setenv(k, "")
	}
	targets := map[string]string{}
	for _, d := range renamedDirs() {
		targets[d.env] = d.to
	}
	for _, c := range []struct{ name, path, env string }{
		{"config", defaultConfigPath(), "MFSH_CONFIG"},
		{"data home", fabricHome(), "MFSH_HOME"},
		{"models", defaultModelsRoot(), "MFSH_HOME"},
		{"state", defaultStateDir(), "MFSH_STATE_DIR"},
	} {
		to := targets[c.env]
		if to == "" || (c.path != to && !strings.HasPrefix(c.path, to+string(filepath.Separator))) {
			t.Errorf("%s is read from %s, but the rename moves it to %q", c.name, c.path, to)
		}
	}
}
