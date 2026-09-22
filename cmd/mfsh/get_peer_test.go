package main

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"github.com/gregbugaj/modelfabric/internal/hub"
)

// The hub client replaces a file whose content does not match, which suits a
// half-finished download. A copy from a peer must not do that to a model this
// machine already had under the same name.
func TestCheckNoClobber(t *testing.T) {
	sum := func(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }
	want := hub.File{Name: "m.gguf", Size: 5, SHA256: sum("peer!")}
	for _, c := range []struct {
		name     string
		onDisk   *string
		wantErr  bool
		wantHave int64
	}{
		{"nothing there yet is fine", nil, false, 0},
		// Counted as already here, so the summary reports only what crossed
		// the tailnet: a re-run once said "copied at 3.2GB/s" having copied nothing.
		{"an identical file is fine; the copy skips it", ptr("peer!"), false, 5},
		{"same size, different bytes is refused", ptr("mine!"), true, 0},
		{"a different size is refused", ptr("mine"), true, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			if c.onDisk != nil {
				if err := os.WriteFile(filepath.Join(dir, "m.gguf"), []byte(*c.onDisk), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			have, err := checkNoClobber(dir, []hub.File{want})
			if (err != nil) != c.wantErr {
				t.Errorf("err = %v, want error %v", err, c.wantErr)
			}
			if have != c.wantHave {
				t.Errorf("already here: %d bytes, want %d", have, c.wantHave)
			}
		})
	}
}

func ptr(s string) *string { return &s }
