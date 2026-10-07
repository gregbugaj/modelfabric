package osproc

import (
	"strconv"
	"strings"
)

// Resident is how much physical memory a process holds, in bytes. ok is false
// when it cannot be read, which callers show as unknown and never as zero.
func Resident(pid int) (bytes uint64, ok bool) {
	s, err := ps(pid, "rss=")
	if err != nil {
		return 0, false
	}
	kb, err := strconv.ParseUint(strings.TrimSpace(s), 10, 64)
	if err != nil {
		return 0, false
	}
	return kb << 10, true
}
