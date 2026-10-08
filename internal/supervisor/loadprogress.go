package supervisor

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/gregbugaj/modelfabric/internal/osproc"
)

// A load used to say "starting engine" from the first second to the last. On
// a GPU with the weights already in the page cache that is four seconds; cold
// from a slow disk, or a large model, it is minutes of a spinner with nothing
// to tell a load that is working from one that is stuck.
//
// llama.cpp does not report how far a load has got: its log has a line when
// it starts reading the model and one when it is done. What can be measured
// from outside is the memory the engine holds, which rises as the weights go
// in and stops near their size. So progress is that memory over the size of
// the files being loaded: on the GPU where nvidia-smi sees the process, in
// RAM otherwise.

// loadReportEvery is how often a load's progress is measured. Each report is
// written to the journal, and nvidia-smi is run for each.
const loadReportEvery = time.Second

// loadFraction is how far a load has got, given what the engine holds and the
// size of what it is loading. It stops short of 1: the weights going in is
// most of a load and not all of it, and "100%" beside a spinner is a lie.
func loadFraction(heldBytes, weightBytes int64) float64 {
	if heldBytes <= 0 || weightBytes <= 0 {
		return 0
	}
	return min(float64(heldBytes)/float64(weightBytes), 0.99)
}

// loadMessage says where a load is, for a person.
func loadMessage(heldBytes, weightBytes int64, onGPU bool) string {
	if heldBytes <= 0 {
		return "starting engine"
	}
	where := "into memory"
	if onGPU {
		where = "onto the GPU"
	}
	if weightBytes <= 0 {
		// The catalog did not size this model, so there is nothing to be a
		// fraction of.
		return fmt.Sprintf("loading model: %.1f GB so far", float64(heldBytes)/(1<<30))
	}
	if heldBytes >= weightBytes {
		return "weights loaded; preparing slots and cache"
	}
	return fmt.Sprintf("loading weights %s: %.1f of %.1f GB", where, float64(heldBytes)/(1<<30), float64(weightBytes)/(1<<30))
}

// held is the memory an engine process holds: across its GPUs when nvidia-smi
// lists it, otherwise its resident memory.
func held(ctx context.Context, pid int) (bytes int64, onGPU bool) {
	for _, g := range osproc.GPUMemoryByPID(ctx)[pid] {
		bytes += g.MB << 20
	}
	if bytes > 0 {
		return bytes, true
	}
	if b, ok := osproc.Resident(pid); ok {
		return int64(b), false
	}
	return 0, false
}

// reportLoad writes a load's progress to its operation until stop is closed.
// deployment is what the launcher recorded the engine's process under.
func (s *Supervisor) reportLoad(opID, deployment string, weightBytes int64, stop <-chan struct{}) {
	t := time.NewTicker(loadReportEvery)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-s.stop:
			return
		case <-t.C:
		}
		pid := s.lch.PIDOf(deployment)
		if pid <= 0 {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		bytes, onGPU := held(ctx, pid)
		cancel()
		// Checked again after measuring: a load that finished meanwhile has
		// its own last word, and this must not land after it.
		select {
		case <-stop:
			return
		default:
		}
		s.journal.Report(opID, loadMessage(bytes, weightBytes, onGPU), loadFraction(bytes, weightBytes))
	}
}

// fileSize is a file's size in bytes, zero when it cannot be read.
func fileSize(path string) int64 {
	if path == "" {
		return 0
	}
	st, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return st.Size()
}
