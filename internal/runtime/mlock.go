package runtime

import (
	"strings"

	"github.com/gregbugaj/modelfabric/internal/osproc"
)

// mlock keeps a model's weights resident, which is what LM Studio asks for.
// llama.cpp locks the whole mapped file, so on a machine whose RAM is smaller
// than the model it either fails (a small RLIMIT_MEMLOCK, and a warning on
// every load) or succeeds and pins more memory than the system can spare.
// Both happen on a GPU box with modest RAM: minion has 15GB for a 16GB 27B.
// The weights live in VRAM there anyway, so locking the host copy buys
// nothing.

// Probes, replaced in tests.
var (
	memAvailable = readMemAvailable
	memTotal     = readMemTotal
	memlockLimit = osproc.MemlockLimit
)

// fitLoadMode drops mlock from a load mode when the model cannot be locked:
// the memlock limit is below the model size, or the model would take more
// than three quarters of available memory. Unknown values leave it as is.
func fitLoadMode(mode string, size int64) string {
	if size <= 0 || !strings.Contains(mode, "mlock") {
		return mode
	}
	limit, unlimited := memlockLimit()
	avail := memAvailable()
	if (!unlimited && limit < uint64(size)) || (avail > 0 && size > avail/4*3) {
		var keep []string
		for _, p := range strings.Split(mode, "+") {
			if p != "mlock" {
				keep = append(keep, p)
			}
		}
		if len(keep) == 0 {
			// "mlock" alone is try_mmap=false with keep_in_memory=true, so
			// mmap here does go against what was asked. It is still the right
			// answer: the reason the lock was dropped is that memory is tight,
			// and "none" would have llama.cpp read the whole model into
			// anonymous memory — the worst outcome on exactly that machine.
			// mmap lets the kernel page it instead.
			return "mmap"
		}
		return strings.Join(keep, "+")
	}
	return mode
}

// llama-server keeps evicted slot states in a host-RAM prompt cache, 8 GiB by
// default. On a GPU box with modest RAM that is on top of the context
// checkpoints (see Definition.CtxCheckpoints) and the process itself; minion
// (15 GiB) was OOM-killed by the two together. The cache only saves prefill,
// so it is capped at an eighth of RAM there rather than risk the engine.
const defaultCacheRAMMiB = 8192

// fitCacheRAM returns the prompt-cache cap in MiB for this host.
func fitCacheRAM() int {
	total := memTotal()
	if total <= 0 {
		return defaultCacheRAMMiB
	}
	return min(defaultCacheRAMMiB, int(total>>20)/8)
}

// HostMemory is the machine's total and currently available memory in bytes,
// zero for either when the platform will not say.
//
// Exported for the slot tuner, which reloads an engine once per slot count and
// can wedge a machine doing it. On a unified-memory Mac the engine's KV comes
// out of the same pool as everything else, and a sweep that pushed two
// concurrent requests through a 27B MLX model took the host off the network
// entirely — no OOM kill, no error, just a machine that stopped answering. A
// sweep has to be able to see that coming.
func HostMemory() (total, available int64) { return memTotal(), memAvailable() }
