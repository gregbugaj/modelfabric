package osproc

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
)

// BirthID is a process's start time in clock ticks since boot, /proc/<pid>/stat
// field 22. Reusing a pid requires wrapping the whole pid space, and the
// replacement has a different start time, so (pid, BirthID) is unique in
// practice.
func BirthID(pid int) (string, error) {
	fields, err := statFields(pid)
	if err != nil {
		return "", err
	}
	// After comm, field 3 is state; starttime is field 22 overall, so index 19
	// counting state as index 0.
	const startTimeOffset = 19
	if len(fields) <= startTimeOffset {
		return "", fmt.Errorf("unexpected /proc/%d/stat layout", pid)
	}
	if _, err := strconv.ParseUint(fields[startTimeOffset], 10, 64); err != nil {
		return "", fmt.Errorf("parse starttime: %w", err)
	}
	return fields[startTimeOffset], nil
}

// Zombie reports whether pid has exited and is waiting to be reaped. A child
// that failed at startup stays a zombie until its parent waits for it, and
// /proc still lists it, so liveness checks must exclude that state.
func Zombie(pid int) bool {
	fields, err := statFields(pid)
	return err == nil && len(fields) > 0 && fields[0] == "Z"
}

// statFields is /proc/<pid>/stat after comm, which is parenthesised and may
// itself contain spaces; so it is parsed after the final ')'.
func statFields(pid int) ([]string, error) {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return nil, err
	}
	line := string(b)
	idx := strings.LastIndexByte(line, ')')
	if idx < 0 || idx+2 >= len(line) {
		return nil, fmt.Errorf("malformed /proc/%d/stat", pid)
	}
	return strings.Fields(line[idx+2:]), nil
}

func Argv(pid int) ([]string, error) {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil {
		return nil, err
	}
	if len(b) == 0 {
		return nil, fmt.Errorf("process %d has no command line", pid)
	}
	return strings.Split(strings.TrimRight(string(b), "\x00"), "\x00"), nil
}

// Name is the process's own name for itself, or "?" when unknown.
func Name(pid int) string {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/comm", pid))
	if err != nil {
		return "?"
	}
	return strings.TrimSpace(string(b))
}

// All lists the processes this user can see, skipping any that vanish while
// being read.
func All() []Proc {
	entries, _ := os.ReadDir("/proc")
	out := make([]Proc, 0, len(entries))
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		argv, err := Argv(pid)
		if err != nil {
			continue
		}
		out = append(out, Proc{PID: pid, Argv: argv})
	}
	return out
}

// DieWithParent starts a child in its own process group, terminated if this
// process dies. Pdeathsig is Linux-only; see the darwin file for what is
// possible there.
func DieWithParent() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGTERM}
}

// MemlockLimit is RLIMIT_MEMLOCK's soft limit in bytes, and whether it is
// unlimited; a sentinel each platform spells differently, so callers must not
// compare against one themselves. The limit is 0 when it cannot be read.
func MemlockLimit() (uint64, bool) {
	const rlimitMemlock = 0x8 // RLIMIT_MEMLOCK on Linux
	var r syscall.Rlimit
	if syscall.Getrlimit(rlimitMemlock, &r) != nil {
		return 0, false
	}
	return r.Cur, r.Cur == ^uint64(0) // RLIM_INFINITY
}

// OSVersion is the distribution's own name for itself, which is what an
// operator recognises ("Ubuntu 24.04.1 LTS"), falling back to the kernel
// release when there is no os-release file.
func OSVersion() string {
	if b, err := os.ReadFile("/etc/os-release"); err == nil {
		for _, line := range strings.Split(string(b), "\n") {
			if v, ok := strings.CutPrefix(line, "PRETTY_NAME="); ok {
				if name := strings.Trim(strings.TrimSpace(v), `"`); name != "" {
					return name
				}
			}
		}
	}
	var u syscall.Utsname
	if syscall.Uname(&u) == nil {
		return "Linux " + cstr(u.Release[:])
	}
	return "Linux"
}

func cstr(b []int8) string {
	out := make([]byte, 0, len(b))
	for _, c := range b {
		if c == 0 {
			break
		}
		out = append(out, byte(c))
	}
	return string(out)
}
