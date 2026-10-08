package supervisor

import (
	"context"
	"testing"
	"time"

	"github.com/gregbugaj/modelfabric/internal/osproc"
)

// One failed nvidia-smi (a driver reset, a timeout) replaced the last answer
// with nothing, so every engine's GPU memory went blank until the next ask.
// The last answer now stands through a failure, but not for ever: a figure
// nobody has been able to measure for half a minute is not known.
func TestGPUMemoryOutlivesOneFailedAskButNotMany(t *testing.T) {
	held := map[int][]osproc.GPUUse{42: {{GPU: 0, MB: 21000}}}
	answer := held
	m := &memoryWatch{askGPU: func(context.Context) map[int][]osproc.GPUUse { return answer }}
	// again makes the next call ask, as five seconds passing would.
	again := func() { m.mu.Lock(); m.gpuAt = time.Time{}; m.mu.Unlock() }

	if got := m.gpu(42); len(got) != 1 || got[0].MB != 21000 {
		t.Fatalf("first ask = %v, want the engine's 21000 MiB", got)
	}

	answer = nil // nvidia-smi fails
	again()
	if got := m.gpu(42); len(got) != 1 {
		t.Fatalf("after one failed ask = %v, want the last answer kept", got)
	}

	m.mu.Lock()
	m.gpuGoodAt = time.Now().Add(-gpuTrust - time.Second)
	m.mu.Unlock()
	again()
	if got := m.gpu(42); got != nil {
		t.Fatalf("after failing for longer than gpuTrust = %v, want unknown", got)
	}

	// An answer that names no process is an answer: the engine holds nothing.
	answer = held
	again()
	m.gpu(42)
	answer = map[int][]osproc.GPUUse{}
	again()
	if got := m.gpu(42); got != nil {
		t.Fatalf("after nvidia-smi listed no processes = %v, want none", got)
	}
}
