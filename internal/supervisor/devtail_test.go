package supervisor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gregbugaj/modelfabric/internal/devlog"
)

// Lines as llama-server b11153 writes them, taken from this fleet's logs.
func TestEngineLinesAreSaidInWords(t *testing.T) {
	for _, c := range []struct {
		name  string
		line  string
		level string // of the entry said in words; "" when the line is only passed on
		want  string
	}{
		{"progress through a long prompt",
			"1.05.357.563 I slot print_timing: id  2 | task 122 | prompt processing, n_tokens =   3385, progress = 0.44, t =   4.28 s / 791.43 tokens per second",
			devlog.Info, "slot 2: prompt processing 44% (3385 tokens read)"},
		{"how fast the prompt was read",
			"0.58.859.674 I slot print_timing: id  3 | task 91 | prompt eval time =    3256.17 ms /  2692 tokens (    1.21 ms per token,   826.74 tokens per second)",
			devlog.Info, "slot 3: read 2692 prompt tokens in 3.3 s (827 tok/s)"},
		{"how fast it generated, which is not the prompt line",
			"0.58.859.678 I slot print_timing: id  3 | task 91 |        eval time =     945.64 ms /    48 tokens (   20.12 ms per token,    49.70 tokens per second)",
			devlog.Info, "slot 3: generated 48 tokens in 946 ms (49.7 tok/s)"},
		{"a slot that already held the prompt",
			"0.51.324.951 I slot get_availabl: id  0 | task -1 | selected slot by LCP similarity, f_sim_best = 0.905 (> 0.100 thold), f_keep = 1.000",
			devlog.Info, "slot 0: chosen because it holds 91% of this prompt"},
		{"a slot taken from another conversation",
			"0.52.887.973 I slot get_availabl: id  1 | task -1 | selected slot by LRU, t_last = 1999633961946",
			devlog.Info, "slot 1: chosen as the least recently used; no slot holds this prompt"},
		{"the RAM cache overflowing, which is why a request reads everything again",
			"3.10.001.002 W srv         alloc:  - making room for prompt cache entry, removing oldest entry (size = 2334.512 MiB)",
			devlog.Warn, "RAM cache full: dropped the oldest saved conversation (2335 MiB). Its next request reads its whole prompt again"},
		{"speculative decoding's score",
			"0.50.989.256 I slot print_timing: id  1 | task 57 | draft acceptance = 0.71111 (   32 accepted /    45 generated), mean len =  3.13",
			devlog.Info, "slot 1: speculative decoding: 32 of 45 drafted tokens accepted (71%)"},
		{"a line with nothing to add is passed on as it is",
			"0.50.989.434 I slot      release: id  1 | task 57 | stop processing: n_tokens = 3028, truncated = 0", "", ""},
		{"another engine's format is passed on as it is",
			"INFO: 127.0.0.1:51234 - \"POST /v1/chat/completions HTTP/1.1\" 200 OK", "", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := engineEntries(c.line)
			if got[0].Level != devlog.Debug || got[0].Msg != c.line || got[0].Source != devlog.Engine {
				t.Errorf("the line itself must come first, at debug, unchanged: %+v", got[0])
			}
			if c.level == "" {
				if len(got) != 1 {
					t.Errorf("nothing more to say, got %q", got[1].Msg)
				}
				return
			}
			if len(got) != 2 {
				t.Fatalf("got %d entries, want the line and what it says", len(got))
			}
			if got[1].Level != c.level || got[1].Msg != c.want {
				t.Errorf("got %s %q\nwant %s %q", got[1].Level, got[1].Msg, c.level, c.want)
			}
		})
	}
	t.Run("the engine's own error is an error, not detail", func(t *testing.T) {
		got := engineEntries("9.01.002.003 E init_batch: failed to prepare attention ubatches")
		if got[0].Level != devlog.Error {
			t.Errorf("level %s, want error", got[0].Level)
		}
	})
}

func TestTailReadsWholeLinesAsTheyAreWritten(t *testing.T) {
	path := filepath.Join(t.TempDir(), "inst.log")
	write := func(s string) {
		f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		if _, err := f.WriteString(s); err != nil {
			t.Fatal(err)
		}
	}
	var tl devTail
	read := func(p string) []string { lines, _ := tl.read(p); return lines }
	write("loading model\nhalf a li")
	if got := read(path); strings.Join(got, "|") != "loading model" {
		t.Errorf("got %q, want the finished line only", got)
	}
	// The rest of the line arrives: it is one line, not two halves.
	write("ne\n\nnext\n")
	if got := read(path); strings.Join(got, "|") != "half a line|next" {
		t.Errorf("got %q, want the completed line and the next, blank skipped", got)
	}
	if got := read(path); got != nil {
		t.Errorf("nothing new, got %q", got)
	}
	// A reloaded engine starts a new file under the same name.
	if err := os.WriteFile(path, []byte("fresh\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := read(path); strings.Join(got, "|") != "fresh" {
		t.Errorf("got %q, want the replaced log from its start", got)
	}

	t.Run("a large log first seen is followed from its end", func(t *testing.T) {
		big := filepath.Join(t.TempDir(), "big.log")
		if err := os.WriteFile(big, []byte(strings.Repeat("old line\n", tailFromStartBelow/9+10)), 0o600); err != nil {
			t.Fatal(err)
		}
		var tl devTail
		if got, _ := tl.read(big); got != nil {
			t.Errorf("replayed %d old lines from an engine that was already running", len(got))
		}
	})
}

// Read half a second late and in batches, an engine's lines landed after the
// router's "finished" for the request they describe (seen live, 2026-10-07:
// "read 1982 prompt tokens" 16ms after "finished"). Each line's own stamp
// puts it where it happened.
func TestEngineLinesKeepTheTimeTheyWereWritten(t *testing.T) {
	now := time.Date(2026, 10, 7, 15, 28, 0, 0, time.UTC)
	var tl devTail
	if got := tl.when("0.10.000.000 I anything"); !got.IsZero() {
		t.Errorf("before any batch there is no start to count from, got %v", got)
	}
	// A batch read 400ms after its last line was written: the start is put
	// 400ms late, and every line with it.
	tl.calibrate([]string{"0.09.000.000 I early", "0.10.000.000 I late"}, now.Add(400*time.Millisecond))
	if got, want := tl.when("0.10.000.000 I late"), now.Add(400*time.Millisecond); !got.Equal(want) {
		t.Errorf("first estimate %v, want %v", got, want)
	}
	// A later batch read promptly corrects it, for lines old and new.
	tl.calibrate([]string{"1.00.000.000 I a minute in"}, now.Add(50*time.Second+5*time.Millisecond))
	if got, want := tl.when("0.09.000.000 I early"), now.Add(-995*time.Millisecond); !got.Equal(want) {
		t.Errorf("after a prompt read, %v, want %v", got, want)
	}
	// A slower read afterwards must not move it back.
	tl.calibrate([]string{"2.00.000.000 I later still"}, now.Add(111*time.Second))
	if got, want := tl.when("0.10.000.000 I late"), now.Add(5*time.Millisecond); !got.Equal(want) {
		t.Errorf("a late read moved the start: %v, want %v", got, want)
	}
	if got := tl.when("INFO: a line from an engine that does not stamp"); !got.IsZero() {
		t.Errorf("an unstamped line has no time of its own, got %v", got)
	}
	if d, ok := stampOf("531.58.684.468 I slot print_timing"); !ok || d != 531*time.Minute+58*time.Second+684*time.Millisecond+468*time.Microsecond {
		t.Errorf("stamp read as %v %v", d, ok)
	}
}
