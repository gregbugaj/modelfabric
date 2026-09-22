package tuner

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/gregbugaj/modelfabric/internal/runtime"
)

// Watching host memory while a row runs, so a sweep cannot wedge the machine it
// is measuring.
//
// This is not a prediction. The whole reason this package exists is that KV cost
// per token cannot be calculated — attention-head geometry a GGUF does not
// always carry, hybrid attention wrong by four times, quantized caches, unified
// memory — so a guard that estimated what a slot count needs would be inventing
// the number the tool was built to measure.
//
// What it can do is watch. Measured the hard way on a Mac: a sweep loaded a 27B
// MLX model and started its two-slot row, and the host went off the tailnet
// entirely — no out-of-memory kill, no error from the engine, no row, and ssh
// gone with it. mlx-lm allocates as a request grows and the model's declared
// context was 262144 tokens, so nothing was refused; the machine simply ran out
// of room to be a machine. llama.cpp is easier: its KV is allocated at load, so
// too many slots fails the load and the sweep already reports that.
//
// So: refuse a row when memory is tight before it starts, and abandon one when
// memory collapses while it runs. Both stop the sweep, because the next slot
// count is larger and there is no reason to think it will go better.

// Floors, as fractions of the machine's total memory.
//
// A sweep is a deliberate act on an idle machine, so these are about survival
// rather than comfort: enough left that the kernel, the network stack and an ssh
// session keep working. The absolute floor exists because a percentage of a small
// machine is not much memory at all.
const (
	startFloorFraction = 0.12
	abortFloorFraction = 0.06
	absoluteFloor      = 1 << 30 // 1 GiB
)

// Probes, replaced in tests.
var hostMemory = runtime.HostMemory

// memoryTooTight reports whether a row should not be started, and why.
func memoryTooTight() (bool, string) {
	total, available := hostMemory()
	if total <= 0 || available <= 0 {
		return false, "" // the platform will not say; carry on as before
	}
	floor := int64(float64(total) * startFloorFraction)
	if floor < absoluteFloor {
		floor = absoluteFloor
	}
	if available >= floor {
		return false, ""
	}
	return true, fmt.Sprintf("only %s of %s memory is free, and this row allocates more",
		gib(available), gib(total))
}

// watchMemory cancels the row when the machine runs short, and reports what it
// saw. The returned func stops the watch and says whether it fired.
func watchMemory(ctx context.Context, cancel context.CancelFunc) func() (bool, string) {
	total, _ := hostMemory()
	if total <= 0 {
		return func() (bool, string) { return false, "" }
	}
	floor := int64(float64(total) * abortFloorFraction)
	if floor < absoluteFloor/2 {
		floor = absoluteFloor / 2
	}

	done := make(chan struct{})
	// Buffered and closed by the goroutine, so stopping can wait for it to be
	// finished rather than merely asked. A watch that is still polling after it
	// was stopped holds a reference to whatever it reads — which the race
	// detector caught here first, and which would be a leak in a long-lived
	// process.
	exited := make(chan string, 1)
	go func() {
		defer close(exited)
		// Half a second: the window between "memory is going" and "the host has
		// stopped answering" is short, and a sweep that noticed a minute later
		// would be describing a machine that is already gone.
		t := time.NewTicker(500 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-t.C:
				if _, available := hostMemory(); available > 0 && available < floor {
					exited <- fmt.Sprintf("host memory fell to %s of %s while the row was running",
						gib(available), gib(total))
					cancel()
					return
				}
			}
		}
	}()
	var once sync.Once
	return func() (bool, string) {
		once.Do(func() { close(done) })
		// Blocks until the goroutine has returned, so nothing it reads can be
		// swapped out from under it afterwards.
		why, fired := <-exited
		return fired, why
	}
}

func gib(bytes int64) string {
	return fmt.Sprintf("%.1fGB", float64(bytes)/(1<<30))
}
