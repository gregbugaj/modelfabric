// Package osproc answers what the operating system knows about a process:
// whether it is still the one we started, what it is running, and how to make
// a child die with its parent.
//
// ModelFabric supervises processes it did not necessarily start itself (engines are
// adopted across restarts), so a pid on its own is never an identity — the
// pair (pid, birth time) is. Linux reads /proc; macOS has no /proc, so it asks
// ps, which is how the rest of the tree already talks to nvidia-smi and
// tailscale. Facts that cannot be established are reported as missing rather
// than guessed: a caller that cannot identify a process must not kill it.
package osproc

import "path/filepath"

// Proc is one running process.
type Proc struct {
	PID  int
	Argv []string
}

// Argv0Base is the executable name of a process, without its directory.
func (p Proc) Argv0Base() string {
	if len(p.Argv) == 0 {
		return ""
	}
	return filepath.Base(p.Argv[0])
}

// Arg reports whether argv contains flag, and returns the value after it.
func (p Proc) Arg(flag string) (string, bool) {
	for i, a := range p.Argv {
		if a == flag && i+1 < len(p.Argv) {
			return p.Argv[i+1], true
		}
	}
	return "", false
}
