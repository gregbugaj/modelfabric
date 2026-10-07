package tuner

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/gregbugaj/modelfabric/internal/runtime"
)

// Monitor available host memory during sweeps. Engines such as mlx-lm grow KV
// state during requests, so successful loading does not establish memory safety.
// Refuse low-memory rows and abort on pressure; either condition stops the sweep.

// Fractional and absolute memory floors reserve capacity for the OS and remote access.
const (
	startFloorFraction = 0.12
	abortFloorFraction = 0.06
	absoluteFloor      = 1 << 30 // 1 GiB
)

// Probes, replaced in tests.
var hostMemory = runtime.HostMemory

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
	// Wait for the polling goroutine to exit before releasing its reader.
	exited := make(chan string, 1)
	go func() {
		defer close(exited)
		// Poll every half second to cancel before memory pressure disables the host.
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
