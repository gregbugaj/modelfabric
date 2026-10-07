package runtime

import (
	"strings"

	"github.com/gregbugaj/modelfabric/internal/osproc"
)

// llama.cpp locks the whole mapped weights file. If it exceeds RLIMIT_MEMLOCK,
// locking fails; if it exceeds available RAM, locking can starve the host.

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
			// Fall back to mmap when memory is tight. "none" would read the whole
			// model into anonymous memory; mmap allows the kernel to page it out.
			return "mmap"
		}
		return strings.Join(keep, "+")
	}
	return mode
}

// llama-server's default 8 GiB host prompt cache adds to context checkpoints
// and caused OOMs on a 15 GiB host. Cap it at one eighth of RAM.
const defaultCacheRAMMiB = 8192

func fitCacheRAM() int {
	total := memTotal()
	if total <= 0 {
		return defaultCacheRAMMiB
	}
	return min(defaultCacheRAMMiB, int(total>>20)/8)
}

// HostMemory returns total and available host memory in bytes, zero when
// unknown. The slot tuner uses it to avoid exhausting shared CPU/GPU memory.
func HostMemory() (total, available int64) { return memTotal(), memAvailable() }
