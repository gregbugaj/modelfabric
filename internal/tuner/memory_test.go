package tuner

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func withMemory(t *testing.T, total, available int64) {
	t.Helper()
	prev := hostMemory
	hostMemory = func() (int64, int64) { return total, available }
	t.Cleanup(func() { hostMemory = prev })
}

const gb = int64(1) << 30

// A row is refused before it allocates anything when the machine has nothing to
// spare. The sweep that prompted this took a Mac off the network entirely: no
// out-of-memory kill, no error from the engine, no row, and the ssh session gone
// with it.
func TestARowIsRefusedWhenMemoryIsAlreadyTight(t *testing.T) {
	withMemory(t, 36*gb, 2*gb) // 5.5%, under the start floor
	tight, why := memoryTooTight()
	if !tight {
		t.Fatal("a row was allowed on a machine with 2GB of 36GB free")
	}
	for _, want := range []string{"2.0GB", "36.0GB"} {
		if !strings.Contains(why, want) {
			t.Errorf("the reason does not name %s: %q", want, why)
		}
	}
}

func TestARowRunsWhenThereIsRoom(t *testing.T) {
	withMemory(t, 36*gb, 20*gb)
	if tight, why := memoryTooTight(); tight {
		t.Errorf("refused a row with 20GB of 36GB free: %s", why)
	}
}

// A platform that will not say must not block the sweep. Reading nothing is not
// the same as reading zero free, and refusing every row on a machine ModelFabric cannot
// measure would break tuning everywhere it cannot see memory.
func TestUnknownMemoryDoesNotRefuseTheRow(t *testing.T) {
	withMemory(t, 0, 0)
	if tight, _ := memoryTooTight(); tight {
		t.Error("refused a row on a platform that reports no memory figures")
	}
}

// The absolute floor matters on a small machine, where a percentage is not much
// memory: 12% of 8GB is under a gigabyte, which is not enough to keep a host
// answering while an engine loads.
func TestTheFloorIsAtLeastAGigabyte(t *testing.T) {
	withMemory(t, 8*gb, 900*1024*1024) // 11% of 8GB, but under 1GiB
	if tight, _ := memoryTooTight(); !tight {
		t.Error("allowed a row with under a gigabyte free")
	}
}

// The case the Mac hit: memory was fine when the row started and collapsed while
// it ran, because mlx-lm allocates as a request grows. The watch abandons the row
// instead of letting the host go under.
func TestTheWatchAbandonsARowWhenMemoryCollapses(t *testing.T) {
	// Atomic because the watch reads it from its own goroutine while the test
	// changes it — the race detector is right to object to the plain variable
	// this started as.
	var available atomic.Int64
	available.Store(20 * gb)
	prev := hostMemory
	hostMemory = func() (int64, int64) { return 36 * gb, available.Load() }
	t.Cleanup(func() { hostMemory = prev })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stop := watchMemory(ctx, cancel)

	// The row is running happily, then the engine eats the machine.
	time.Sleep(700 * time.Millisecond)
	if ctx.Err() != nil {
		t.Fatal("the row was abandoned while memory was fine")
	}
	available.Store(1 * gb) // under the abort floor for a 36GB host

	deadline := time.After(3 * time.Second)
	for ctx.Err() == nil {
		select {
		case <-deadline:
			t.Fatal("memory collapsed and the row was not abandoned")
		case <-time.After(50 * time.Millisecond):
		}
	}
	starved, why := stop()
	if !starved {
		t.Error("the row was cancelled but not reported as starved")
	}
	if !strings.Contains(why, "1.0GB") || !strings.Contains(why, "36.0GB") {
		t.Errorf("the reason should name what it saw: %q", why)
	}
}

// And it stays quiet when nothing goes wrong: a watch that reported starvation on
// a healthy row would make every sweep untrustworthy.
func TestTheWatchIsQuietOnAHealthyRow(t *testing.T) {
	withMemory(t, 36*gb, 20*gb)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stop := watchMemory(ctx, cancel)
	time.Sleep(700 * time.Millisecond)
	if starved, why := stop(); starved {
		t.Errorf("reported starvation on a healthy row: %s", why)
	}
	if ctx.Err() != nil {
		t.Error("cancelled a healthy row")
	}
}

// idleEngine is an engine that is always free and never fails, so a test can
// exercise the sweep's own decisions.
type idleEngine struct{ reloads int }

func (e *idleEngine) Reload(context.Context, string, int, int) error { e.reloads++; return nil }
func (e *idleEngine) Target(context.Context, string) (string, string, error) {
	return "http://127.0.0.1:1", "m", nil
}
func (e *idleEngine) Inflight(context.Context, string) (int, error)     { return 0, nil }
func (e *idleEngine) Current(context.Context, string) (int, int, error) { return 1, 4096, nil }

// A starved row ends the sweep, even for slot counts the operator listed: the
// next one allocates more, and the row after a wedge is not a measurement.
func TestStarvationStopsTheSweep(t *testing.T) {
	withMemory(t, 36*gb, 1*gb) // tight from the start
	e := &idleEngine{}
	rep, err := Run(context.Background(), e, Config{Model: "m", Slots: []int{1, 2, 4}}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(rep.Rows) != 1 {
		t.Fatalf("%d rows, want 1: the sweep should stop at the first starved row", len(rep.Rows))
	}
	if !rep.Rows[0].Starved {
		t.Errorf("row not marked starved: %+v", rep.Rows[0])
	}
	if e.reloads > 1 {
		t.Errorf("%d reloads: a refused row must not reload the engine first", e.reloads)
	}
	if !strings.Contains(rep.StoppedBecause, "memory") {
		t.Errorf("the report should say why it stopped, got %q", rep.StoppedBecause)
	}
}
