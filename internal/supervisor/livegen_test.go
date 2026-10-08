package supervisor

import (
	"testing"
	"time"
)

// The lines are as llama.cpp b11153 writes them on this mesh. An engine with
// four busy slots at about 22 tok/s each was shown as 22: the per-request
// average, which made the machine doing the most work look the slowest.
func TestLiveGenSumsWhatEachSlotIsWriting(t *testing.T) {
	t0 := time.Unix(1700000000, 0)
	progress := func(slot, rate string) string {
		return "75.26.495.176 I slot print_timing: id  " + slot + " | task 12620 | n_gen =    561, tg =  36.95 t/s, tg_3s =  " + rate + " t/s"
	}
	cases := []struct {
		name   string
		lines  []string
		after  time.Duration
		want   float64
		wantOK bool
	}{
		{"an engine whose log has not been read is unknown, not idle", nil, 0, 0, false},
		{"one slot writing", []string{progress("0", "43.01")}, 0, 43.01, true},
		{"four slots writing add up", []string{progress("0", "20"), progress("1", "22"), progress("2", "21"), progress("3", "23")}, 0, 86, true},
		{"a slot's newer rate replaces its older one", []string{progress("0", "40"), progress("0", "10")}, 0, 10, true},
		{"a released slot stops counting at once", []string{progress("0", "40"), progress("1", "30"),
			"20.58.224.977 I slot      release: id  1 | task 15107 | stop processing: n_tokens = 36348, truncated = 0"}, 0, 40, true},
		{"a slot that went quiet without a release line stops counting", []string{progress("0", "40")}, 8 * time.Second, 0, true},
		{"reading a prompt is not writing", []string{progress("0", "40"),
			"78.29.747.457 I slot print_timing: id  1 | task 24900 | prompt processing, n_tokens =  19224, progress = 0.85, t =  3.1"}, 0, 40, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var g liveGen
			for _, l := range c.lines {
				g.line("e", l, t0)
			}
			got, ok := g.rate("e", t0.Add(c.after))
			if ok != c.wantOK || got != c.want {
				t.Fatalf("rate = %v, %v; want %v, %v", got, ok, c.want, c.wantOK)
			}
		})
	}
}

func TestLiveGenIdleEngineReadsZeroOnceItsLogIsRead(t *testing.T) {
	var g liveGen
	g.saw("e")
	if got, ok := g.rate("e", time.Now()); !ok || got != 0 {
		t.Fatalf("rate = %v, %v; want 0, true", got, ok)
	}
	g.forget("e")
	if _, ok := g.rate("e", time.Now()); ok {
		t.Fatal("a forgotten engine still has a rate")
	}
}
