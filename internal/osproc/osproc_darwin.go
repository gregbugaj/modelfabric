package osproc

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// macOS has no /proc, and the kinfo_proc sysctl is not in the standard
// library's syscall package for darwin, so these facts come from ps. Argv is
// therefore a command line split on spaces rather than the kernel's exact
// argument vector: a path containing a space arrives as two elements. Callers
// use it to recognise processes they started, under paths ModelFabric controls
// (~/.modelfabric, the state directory), so that is accurate there — but it is why
// nothing here is used to decide to kill a process by name alone.

// ps runs ps for one field, returning it trimmed. An unknown pid is an error.
func ps(pid int, format string) (string, error) {
	out, err := exec.Command("ps", "-ww", "-o", format, "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return "", fmt.Errorf("ps %s for pid %d: %w", format, pid, err)
	}
	s := strings.TrimSpace(string(out))
	if s == "" {
		return "", fmt.Errorf("no process %d", pid)
	}
	return s, nil
}

// BirthID is a process's start time as ps reports it ("Sat Sep 20 10:11:12
// 2026"). It has one-second resolution rather than Linux's clock ticks, so
// (pid, BirthID) distinguishes a replacement process unless the pid was reused
// within the same second — which needs the whole pid space to wrap first.
func BirthID(pid int) (string, error) {
	return ps(pid, "lstart=")
}

// Zombie reports whether pid has exited and is waiting to be reaped.
func Zombie(pid int) bool {
	st, err := ps(pid, "state=")
	return err == nil && strings.HasPrefix(st, "Z")
}

// Argv is a process's command line, split on spaces (see the file comment).
func Argv(pid int) ([]string, error) {
	cmd, err := ps(pid, "command=")
	if err != nil {
		return nil, err
	}
	return strings.Fields(cmd), nil
}

// Name is the process's executable name, or "?" when unknown.
func Name(pid int) string {
	comm, err := ps(pid, "comm=")
	if err != nil {
		return "?"
	}
	return filepath.Base(comm)
}

// All lists the processes this user can see.
func All() []Proc {
	out, err := exec.Command("ps", "-axww", "-o", "pid=,command=").Output()
	if err != nil {
		return nil
	}
	var procs []Proc
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		num, rest, ok := strings.Cut(line, " ")
		if !ok {
			continue
		}
		pid, err := strconv.Atoi(num)
		if err != nil {
			continue
		}
		argv := strings.Fields(rest)
		if len(argv) == 0 {
			continue
		}
		procs = append(procs, Proc{PID: pid, Argv: argv})
	}
	return procs
}

// DieWithParent starts a child in its own process group.
//
// macOS has no Pdeathsig, so a child does not die with its parent here: a node
// killed outright leaves llm-d running. That is what the pid
// files and orphan reaping on the next start are for, which every caller of
// this already does because the same case exists on Linux when a node is
// SIGKILLed before it can signal its group.
func DieWithParent() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true}
}

// MemlockLimit is RLIMIT_MEMLOCK's soft limit in bytes, and whether it is
// unlimited. macOS reports unlimited as the largest signed 64-bit value, not
// the all-ones Linux uses, and its resource numbers differ too — reading
// Linux's RLIMIT_MEMLOCK here would return the open-file limit instead.
func MemlockLimit() (uint64, bool) {
	const rlimitMemlock = 6 // RLIMIT_MEMLOCK on darwin
	var r syscall.Rlimit
	if syscall.Getrlimit(rlimitMemlock, &r) != nil {
		return 0, false
	}
	return r.Cur, r.Cur == 1<<63-1 // RLIM_INFINITY
}

// OSVersion is the operating system's own version string, for the mesh to show
// beside a node. macOS keeps the product version in a sysctl, so no process is
// spawned to read it.
func OSVersion() string {
	v, err := syscall.Sysctl("kern.osproductversion")
	if err != nil || v == "" {
		return "macOS"
	}
	return "macOS " + strings.TrimSpace(strings.TrimRight(v, "\x00"))
}
