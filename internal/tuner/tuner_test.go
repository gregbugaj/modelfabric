package tuner

import (
	"context"
	"strings"
	"testing"
)

// A single in-flight check is a race: under load the instantaneous count dips
// to zero between requests, and a sweep starting in that dip reloads the
// engine out from under live traffic.
type flickeringEngine struct {
	counts  []int // returned in order, one per call
	calls   int
	reloads int
}

func (f *flickeringEngine) Inflight(context.Context, string) (int, error) {
	n := 0
	if f.calls < len(f.counts) {
		n = f.counts[f.calls]
	}
	f.calls++
	return n, nil
}
func (f *flickeringEngine) Reload(context.Context, string, int, int) error { f.reloads++; return nil }
func (f *flickeringEngine) Target(context.Context, string) (string, string, error) {
	return "http://127.0.0.1:1", "m", nil
}
func (f *flickeringEngine) Current(context.Context, string) (int, int, error) { return 2, 4096, nil }

func TestRunRefusesAnEngineThatIsOnlyMomentarilyIdle(t *testing.T) {
	// Idle on the first look, busy on the second — the case a single check
	// misses.
	e := &flickeringEngine{counts: []int{0, 2, 2, 2, 2}}
	_, err := Run(context.Background(), e, Config{Model: "m", Slots: []int{1}}, nil)
	if err == nil {
		t.Fatal("started a sweep on an engine that was serving traffic")
	}
	if !strings.Contains(err.Error(), "serving") {
		t.Errorf("error did not explain why: %v", err)
	}
	if e.reloads != 0 {
		t.Errorf("reloaded the engine %d time(s) before refusing", e.reloads)
	}
}
