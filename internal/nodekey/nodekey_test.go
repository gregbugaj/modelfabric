package nodekey

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Legacy gateway keys remain valid to preserve existing client credentials.
func TestTheGatewaysKeyIsStillHonoured(t *testing.T) {
	home := t.TempDir()
	legacy := filepath.Join(home, "gateway")
	if err := os.MkdirAll(legacy, 0o755); err != nil {
		t.Fatal(err)
	}
	const deployed = "sk-mfsh-deadbeef"
	if err := os.WriteFile(filepath.Join(legacy, "master.key"), []byte(deployed+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := Key(home)
	if err != nil {
		t.Fatal(err)
	}
	if got != deployed {
		t.Errorf("Key = %q, want the deployed key %q: generating a new one would "+
			"revoke every credential already issued", got, deployed)
	}
	if _, err := os.Stat(filepath.Join(home, "api.key")); err == nil {
		t.Error("a second key file was created while a usable one existed")
	}
	if Path(home) != filepath.Join(legacy, "master.key") {
		t.Errorf("Path should report the key actually in use, got %s", Path(home))
	}
}

func TestAKeyIsCreatedOnFirstUse(t *testing.T) {
	home := t.TempDir()
	got, err := Key(home)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(got, "sk-") {
		t.Errorf("key %q should start with sk-: OpenAI-compatible clients validate that shape", got)
	}
	// Stable across calls, or every restart would revoke the last one.
	again, err := Key(home)
	if err != nil || again != got {
		t.Errorf("second call returned %q (err %v), want the same key", again, err)
	}
	fi, err := os.Stat(filepath.Join(home, "api.key"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("key file mode %o, want 600: it authorizes every request to this node", fi.Mode().Perm())
	}
}

// An unreadable key file must not be read as an absent one. Falling through
// there would mint a new key over the top of a working deployment.
func TestAnUnreadableKeyIsAnErrorNotAFreshKey(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can read anything")
	}
	home := t.TempDir()
	p := filepath.Join(home, "api.key")
	if err := os.WriteFile(p, []byte("sk-mfsh-existing\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, 0o000); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(p, 0o600)

	if _, err := Key(home); err == nil {
		t.Error("an unreadable key file should be reported, not replaced")
	}
}

// Rotating must retire the old key wherever it lives. A key from before the
// rename can sit in the gateway's file, which Key reads first: replacing only
// api.key would leave the old key working while reporting it rotated.
func TestRotateRetiresTheOldKey(t *testing.T) {
	tests := []struct {
		name   string
		legacy bool
	}{
		{name: "a key in api.key is replaced"},
		{name: "a key in the gateway's file stops working too", legacy: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			if tc.legacy {
				os.MkdirAll(filepath.Join(home, "gateway"), 0o755)
				os.WriteFile(filepath.Join(home, legacyFile), []byte("sk-llmz-old\n"), 0o600)
			}
			old, err := Key(home)
			if err != nil {
				t.Fatal(err)
			}
			fresh, err := Rotate(home)
			if err != nil {
				t.Fatal(err)
			}
			if fresh == old || !strings.HasPrefix(fresh, "sk-mfsh-") {
				t.Fatalf("rotated %q -> %q", old, fresh)
			}
			if got, _ := Key(home); got != fresh {
				t.Fatalf("Key after rotating = %q, want the new key %q", got, fresh)
			}
			fi, err := os.Stat(filepath.Join(home, file))
			if err != nil || fi.Mode().Perm() != 0o600 {
				t.Fatalf("key file %v, %v; want owner-only", fi, err)
			}
			if left, _ := filepath.Glob(filepath.Join(home, ".api.key-*")); len(left) > 0 {
				t.Fatalf("temporary files left: %v", left)
			}
		})
	}
}

func TestPrefixOfShowsTheKindOfKeyAndNothingOfTheKey(t *testing.T) {
	for _, c := range []struct{ name, key, want string }{
		{"a key this build makes", "sk-mfsh-0123456789abcdef0123456789abcdef", "sk-mfsh-"},
		// A node keeps the key it had before the project was renamed, and its
		// mask has to say so (2026-10-08).
		{"a key from before the rename", "sk-llmz-0123456789abcdef0123456789abcdef", "sk-llmz-"},
		{"a key someone wrote into the file by hand", "hunter2hunter2hunter2", ""},
		{"dashes inside the secret are not a prefix", "sk-mfsh-0123-4567-89ab-cdef-0123456789ab", ""},
		{"too little after the dash to call the rest a secret", "sk-mfsh-abc", ""},
		{"empty", "", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := PrefixOf(c.key); got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}
