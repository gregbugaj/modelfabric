package runtime

import (
	"os/exec"
	"strconv"
	"strings"
	"syscall"
)

// Host memory on macOS. The total is a sysctl; there is no single "available"
// figure, so it is summed from vm_stat the way Activity Monitor's free memory
// is: pages that can be handed out without evicting anything in use.

func readMemTotal() int64 {
	v, ok := sysctlUint("hw.memsize")
	if !ok {
		return 0
	}
	return int64(v)
}

// sysctlUint reads a numeric sysctl.
//
// syscall.Sysctl hands the raw little-endian value back as a string and trims
// what it takes to be a C string's trailing NULs — which are the value's
// high-order bytes, so the result is usually shorter than the integer's width.
// A few OIDs do return text, so a decimal string is accepted too.
func sysctlUint(name string) (uint64, bool) {
	raw, err := syscall.Sysctl(name)
	if err != nil {
		return 0, false
	}
	if t := strings.TrimSpace(strings.TrimRight(raw, "\x00")); t != "" {
		if n, err := strconv.ParseUint(t, 10, 64); err == nil {
			return n, true
		}
	}
	if raw == "" {
		return 0, true // an all-NUL value is a real zero
	}
	var v uint64
	for i := 0; i < len(raw) && i < 8; i++ {
		v |= uint64(raw[i]) << (8 * i)
	}
	return v, true
}

func readMemAvailable() int64 {
	out, err := exec.Command("vm_stat").Output()
	if err != nil {
		return 0
	}
	pageSize := int64(16384) // Apple silicon; corrected from the header below
	var pages int64
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(line, "Mach Virtual Memory Statistics:") {
			if _, size, ok := strings.Cut(line, "page size of "); ok {
				// Indexing Fields()[0] assumed a token was there; a changed or
				// truncated header would panic instead of keeping the default.
				if fields := strings.Fields(size); len(fields) > 0 {
					if n, err := strconv.ParseInt(fields[0], 10, 64); err == nil && n > 0 {
						pageSize = n
					}
				}
			}
			continue
		}
		key, val, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		switch strings.TrimSpace(key) {
		// Not "Pages purgeable": it is an attribute counted across VM pages,
		// mostly ones already in the active and inactive queues, so adding it
		// counted the same memory twice. free + inactive + speculative is the
		// set Activity Monitor calls available.
		case "Pages free", "Pages inactive", "Pages speculative":
			n, err := strconv.ParseInt(strings.Trim(strings.TrimSpace(val), "."), 10, 64)
			if err == nil {
				pages += n
			}
		}
	}
	return pages * pageSize
}
