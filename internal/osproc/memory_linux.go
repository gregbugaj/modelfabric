package osproc

import (
	"os"
	"strconv"
	"strings"
)

// Resident is how much physical memory a process holds, in bytes. ok is false
// when it cannot be read, which callers show as unknown and never as zero.
func Resident(pid int) (bytes uint64, ok bool) {
	raw, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/statm")
	if err != nil {
		return 0, false
	}
	f := strings.Fields(string(raw))
	if len(f) < 2 {
		return 0, false
	}
	pages, err := strconv.ParseUint(f[1], 10, 64)
	if err != nil {
		return 0, false
	}
	return pages * uint64(os.Getpagesize()), true
}
