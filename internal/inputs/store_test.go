package inputs

import (
	"errors"
	"os"
	"testing"
	"time"
)

// The bug this guards against: building a manifest from the current files and
// then verifying those files against it always succeeds, so tampering was
// accepted. Preparation must happen once and be pinned.
func TestPinnedInputsRejectLaterTampering(t *testing.T) {
	dir := t.TempDir()
	model := write(t, dir, "model.gguf", "original weights")
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	if _, _, err := store.Prepare("m", dir, []string{model}, ""); err != nil {
		t.Fatalf("first prepare should pin: %v", err)
	}
	// Unchanged files keep working.
	if _, _, err := store.Prepare("m", dir, []string{model}, ""); err != nil {
		t.Fatalf("second prepare on unchanged files: %v", err)
	}

	if err := os.WriteFile(model, []byte("tampered weights!"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, _, err = store.Prepare("m", dir, []string{model}, "")
	if !errors.Is(err, ErrChanged) {
		t.Fatalf("tampering after pinning was accepted: %v", err)
	}
}

func TestRepinAcceptsNewContents(t *testing.T) {
	dir := t.TempDir()
	model := write(t, dir, "model.gguf", "v1")
	store, _ := NewStore(t.TempDir())
	if _, _, err := store.Prepare("m", dir, []string{model}, ""); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if err := os.WriteFile(model, []byte("v2"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Prepare("m", dir, []string{model}, ""); !errors.Is(err, ErrChanged) {
		t.Fatal("expected the change to be rejected before repinning")
	}
	if err := store.Unpin("m"); err != nil {
		t.Fatalf("Unpin: %v", err)
	}
	if _, _, err := store.Prepare("m", dir, []string{model}, ""); err != nil {
		t.Fatalf("prepare after unpin should re-pin: %v", err)
	}
}

// A pin must survive a restart, or verification only holds within one process.
func TestPinSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	pinDir := t.TempDir()
	model := write(t, dir, "model.gguf", "weights")

	first, _ := NewStore(pinDir)
	if _, _, err := first.Prepare("m", dir, []string{model}, ""); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if err := os.WriteFile(model, []byte("changed!"), 0o644); err != nil {
		t.Fatal(err)
	}

	// A fresh Store over the same directory is what a restart looks like.
	second, _ := NewStore(pinDir)
	if _, _, err := second.Prepare("m", dir, []string{model}, ""); !errors.Is(err, ErrChanged) {
		t.Fatalf("pin did not survive restart: %v", err)
	}
}

func TestUnpinIsIdempotent(t *testing.T) {
	store, _ := NewStore(t.TempDir())
	if err := store.Unpin("never-pinned"); err != nil {
		t.Fatalf("Unpin of an absent key should be a no-op, got %v", err)
	}
}

// Re-hashing a 16GB model on every load costs seconds. An unchanged file is
// accepted on its size and mtime instead — but any change must still be caught.
func TestFastPathSkipsHashingUnchangedFiles(t *testing.T) {
	dir := t.TempDir()
	model := write(t, dir, "model.gguf", "weights")
	store, _ := NewStore(t.TempDir())

	if _, _, err := store.Prepare("m", dir, []string{model}, ""); err != nil {
		t.Fatalf("first prepare: %v", err)
	}
	// Corrupt the *content* while restoring size and mtime, then confirm the
	// fast path accepts it — this documents the trade rather than hiding it.
	info, _ := os.Stat(model)
	if err := os.WriteFile(model, []byte("XXXXXXX"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(model, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Prepare("m", dir, []string{model}, ""); err != nil {
		t.Fatalf("same size and mtime should take the fast path: %v", err)
	}

	// With VerifyFull the same situation must be rejected.
	store.VerifyFull = true
	if _, _, err := store.Prepare("m", dir, []string{model}, ""); !errors.Is(err, ErrChanged) {
		t.Fatalf("VerifyFull must re-hash and reject, got %v", err)
	}
}

func TestFastPathCatchesSizeAndMtimeChanges(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(t *testing.T, path string)
	}{
		{"size changes", func(t *testing.T, p string) {
			if err := os.WriteFile(p, []byte("a much longer set of weights"), 0o644); err != nil {
				t.Fatal(err)
			}
		}},
		{"mtime changes", func(t *testing.T, p string) {
			// Same bytes, rewritten later: the stamp no longer matches, so the
			// full hash runs and (here) passes.
			later := time.Now().Add(2 * time.Hour)
			if err := os.Chtimes(p, later, later); err != nil {
				t.Fatal(err)
			}
		}},
		{"file removed", func(t *testing.T, p string) {
			if err := os.Remove(p); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			model := write(t, dir, "model.gguf", "weights")
			store, _ := NewStore(t.TempDir())
			if _, _, err := store.Prepare("m", dir, []string{model}, ""); err != nil {
				t.Fatalf("first prepare: %v", err)
			}
			tc.mutate(t, model)

			_, _, err := store.Prepare("m", dir, []string{model}, "")
			switch tc.name {
			case "mtime changes":
				// Content is unchanged, so the re-hash must succeed.
				if err != nil {
					t.Fatalf("an mtime-only change should re-hash and pass: %v", err)
				}
			default:
				if !errors.Is(err, ErrChanged) {
					t.Fatalf("expected the change to be rejected, got %v", err)
				}
			}
		})
	}
}

// Two keys that sanitize to the same name shared a pin file, so one model
// could be verified against another's manifest; and a corrupt or unreadable
// pin was treated as "not pinned", which re-blessed whatever was on disk —
// the opposite of what pinning is for.
func TestPinsDoNotCollideAndCorruptionIsNotSilent(t *testing.T) {
	s, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if s.path("a/b") == s.path("a_b") {
		t.Fatal("two keys share one pin file")
	}
	// An unseen key is simply unpinned.
	if _, ok, err := s.get("never-seen"); ok || err != nil {
		t.Fatalf("unseen key: ok=%v err=%v; want false,nil", ok, err)
	}
	// A corrupt pin is an error, not "unpinned".
	if err := os.WriteFile(s.path("broken"), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := s.get("broken"); err == nil || ok {
		t.Fatalf("a corrupt pin was treated as missing (ok=%v err=%v)", ok, err)
	}
	// A pin that records a different key does not speak for this one.
	if err := os.WriteFile(s.path("mine"), []byte(`{"key":"someone-else"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := s.get("mine"); err == nil || ok {
		t.Fatalf("a pin for another key was accepted (ok=%v err=%v)", ok, err)
	}
}
