// Package tscli finds the tailscale CLI to talk to.
//
// On Linux there is one: `tailscale` on PATH, talking to the one tailscaled.
// A Mac can have two installations that disagree — Homebrew's tailscaled runs
// in userspace-networking mode, which serves Tailscale SSH and ping but carries
// no ordinary TCP, while the Tailscale app provides the real tunnel and keeps
// its CLI inside the bundle, off PATH. A CLI whose daemon is not running fails,
// so the one to use is the one that answers, not the first one found.
package tscli

import (
	"context"
	"os"
	"os/exec"
	"runtime"
	"sync"
	"time"
)

// candidates are tried in order: PATH first, so an explicit install wins.
func candidates() []string {
	out := []string{"tailscale"}
	if runtime.GOOS != "darwin" {
		return out
	}
	home, _ := os.UserHomeDir()
	for _, app := range []string{
		"/Applications/Tailscale.app/Contents/MacOS/Tailscale",
		"/Applications/Tailscale.app/Contents/MacOS/tailscale",
	} {
		out = append(out, app)
		if home != "" {
			out = append(out, home+app)
		}
	}
	return out
}

var (
	mu      sync.Mutex
	found   string
	checked time.Time
)

// Path is the tailscale CLI to run. It is the first candidate whose daemon
// answers; when none does, it is plain "tailscale", so the caller reports the
// error a missing Tailscale should produce rather than a path nobody expects.
func Path() string {
	mu.Lock()
	defer mu.Unlock()
	// Re-resolve periodically: which installation answers changes when one is
	// started or stopped, and a node outlives that.
	if found != "" && time.Since(checked) < time.Minute {
		return found
	}
	for _, c := range candidates() {
		bin := c
		if !hasSlash(c) {
			p, err := exec.LookPath(c)
			if err != nil {
				continue
			}
			bin = p
		} else if fi, err := os.Stat(c); err != nil || fi.IsDir() {
			continue
		}
		if answers(bin) {
			found, checked = bin, time.Now()
			return bin
		}
	}
	found, checked = "tailscale", time.Now()
	return found
}

// Run executes the working tailscale CLI with these arguments.
func Run(ctx context.Context, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, Path(), args...).Output()
}

// answers reports whether this CLI can reach a running tailscaled.
func answers(bin string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	// --self=false keeps the output small; any success proves the daemon is
	// reachable through this binary.
	return exec.CommandContext(ctx, bin, "status", "--json", "--self=false").Run() == nil
}

func hasSlash(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] == '/' {
			return true
		}
	}
	return false
}
