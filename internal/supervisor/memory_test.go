package supervisor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// An engine's log is the only place it says its RAM cache dropped a
// conversation. The count has to survive being read a piece at a time: every
// peer asks for instance state every couple of seconds, so the log is read
// from where the last look stopped, and a message can straddle two looks.
func TestCacheDropsAreCountedFromTheLogAsItGrows(t *testing.T) {
	drop := "0.01.000 W srv         alloc:  - making room for prompt cache entry, removing oldest entry (size = 2369.559 MiB)\n"
	tooBig := "0.02.000 W srv         alloc:  - prompt state size 6079.642 MiB exceeds cache size limit 4096.000 MiB, skipping\n"
	other := "0.03.000 I slot      release: id  2 | task 110 | stop processing: n_tokens = 4456, truncated = 0\n"
	for _, c := range []struct {
		name   string
		writes []string // appended one at a time, counted after each
		want   int64
	}{
		{"nothing dropped", []string{other, other}, 0},
		{"each kind of drop counts once", []string{drop, other, tooBig}, 2},
		{"several in one read", []string{drop + other + drop + drop}, 3},
		{"a message split across two reads is counted once", []string{drop[:60], drop[60:] + other}, 1},
		{"split at every byte of the phrase, still once", strings.Split(drop, ""), 1},
		{"nothing new since the last look", []string{drop, ""}, 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "inst.log")
			var m memoryWatch
			var got int64
			for _, w := range c.writes {
				f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
				if err != nil {
					t.Fatal(err)
				}
				_, _ = f.WriteString(w)
				f.Close()
				got, _ = m.read("i1", path, 0)
			}
			if got != c.want {
				t.Errorf("counted %d drops, want %d", got, c.want)
			}
		})
	}
	// A log shorter than it was is a new one: an engine restarted under the
	// same id starts from nothing, and so does its count.
	t.Run("a replaced log starts the count again", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "inst.log")
		var m memoryWatch
		_ = os.WriteFile(path, []byte(drop+drop+other), 0o600)
		if got, _ := m.read("i1", path, 0); got != 2 {
			t.Fatalf("counted %d, want 2", got)
		}
		_ = os.WriteFile(path, []byte(drop), 0o600)
		if got, _ := m.read("i1", path, 0); got != 1 {
			t.Errorf("after the log was replaced: counted %d, want 1", got)
		}
	})
	// The engine's own process, for the size: this test is one.
	t.Run("a live process reports its memory, a missing log is no drops", func(t *testing.T) {
		var m memoryWatch
		dropped, rss := m.read("i1", filepath.Join(t.TempDir(), "absent.log"), os.Getpid())
		if dropped != 0 || rss <= 0 {
			t.Errorf("dropped %d rss %d MiB, want 0 drops and a size above zero", dropped, rss)
		}
	})
}
