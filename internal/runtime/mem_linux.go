package runtime

import (
	"os"
	"strconv"
	"strings"
)

// Host memory from /proc/meminfo, in bytes; 0 when it cannot be read.

func readMemTotal() int64     { return meminfo("MemTotal:") }
func readMemAvailable() int64 { return meminfo("MemAvailable:") }

func meminfo(key string) int64 {
	b, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(b), "\n") {
		if f := strings.Fields(line); len(f) >= 2 && f[0] == key {
			// A partial or overflowing value used to be shifted anyway, which
			// turns an unreadable /proc/meminfo into a confident wrong number.
			kb, err := strconv.ParseInt(f[1], 10, 64)
			if err != nil || kb < 0 {
				return 0
			}
			return kb << 10
		}
	}
	return 0
}
