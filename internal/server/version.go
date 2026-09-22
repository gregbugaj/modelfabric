package server

import (
	"os"
	"runtime"
	"time"
)

// BuildInfo identifies the running node's binary, so `mfsh doctor` can tell
// when the file on disk was rebuilt after the node started — a stale node is
// the classic "I fixed it but nothing changed".
type BuildInfo struct {
	Version    string    `json:"version"`
	Go         string    `json:"go"`
	Executable string    `json:"executable"`
	ExeModTime time.Time `json:"exe_mod_time"`
	Started    time.Time `json:"started"`
	PID        int       `json:"pid"`
}

// SetVersion records the node's build identity at startup.
func (s *Server) SetVersion(version string) {
	b := BuildInfo{Version: version, Go: runtime.Version(), Started: time.Now(), PID: os.Getpid()}
	if exe, err := os.Executable(); err == nil {
		b.Executable = exe
		if fi, err := os.Stat(exe); err == nil {
			b.ExeModTime = fi.ModTime()
		}
	}
	s.build = b
}
