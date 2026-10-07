package main

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
)

// Where ModelFabric keeps what it writes, by kind, following each platform's
// conventions:
//
//	            state (ownership records, journal, pins)   logs
//	Linux       $XDG_STATE_HOME/modelfabric (~/.local/state)    <state>/logs
//	macOS       ~/Library/Application Support/modelfabric       ~/Library/Logs/modelfabric
//	Windows     %LocalAppData%\modelfabric                      <state>\logs
//
// Legacy state in ~/.cache/llm-z must migrate: cache cleanup would delete
// ownership records and orphan engines.
//
// MFSH_STATE_DIR and MFSH_LOG_DIR override.

func defaultStateDir() string {
	if d := os.Getenv("MFSH_STATE_DIR"); d != "" {
		return d
	}
	dir := platformStateDir()
	if dir == "" {
		return ".modelfabric-state"
	}
	// Keep using the legacy directory if migration is blocked or fails, avoiding split state.
	if legacy := legacyStateDir(); legacy != "" && !exists(dir) && exists(legacy) {
		return legacy
	}
	return dir
}

func logDir() string {
	if d := os.Getenv("MFSH_LOG_DIR"); d != "" {
		return d
	}
	state := defaultStateDir()
	if runtime.GOOS == "darwin" && state == platformStateDir() {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, "Library", "Logs", "modelfabric")
		}
	}
	return filepath.Join(state, "logs")
}

// nodeLogPath is where `mfsh up` sends the node's own output. Under systemd
// that output goes to the journal instead (journalctl --user -u mfsh).
func nodeLogPath() string {
	if defaultStateDir() == legacyStateDir() {
		return filepath.Join(legacyStateDir(), "node.log")
	}
	return filepath.Join(logDir(), "node.log")
}

func benchDir() string {
	if runtime.GOOS == "linux" || runtime.GOOS == "freebsd" || runtime.GOOS == "openbsd" {
		if d := os.Getenv("XDG_DATA_HOME"); d != "" {
			return filepath.Join(d, "modelfabric", "bench")
		}
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, ".local", "share", "modelfabric", "bench")
		}
	}
	return filepath.Join(platformStateDir(), "bench")
}

func platformStateDir() string {
	switch runtime.GOOS {
	case "darwin":
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, "Library", "Application Support", "modelfabric")
		}
	case "windows":
		if d, err := os.UserCacheDir(); err == nil { // %LocalAppData%
			return filepath.Join(d, "modelfabric")
		}
	default:
		if d := os.Getenv("XDG_STATE_HOME"); d != "" {
			return filepath.Join(d, "modelfabric")
		}
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, ".local", "state", "modelfabric")
		}
	}
	return ""
}

func legacyStateDir() string {
	if runtime.GOOS == "windows" {
		return ""
	}
	if d, err := os.UserCacheDir(); err == nil {
		return filepath.Join(d, "llm-z")
	}
	return ""
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// migrateLegacyState atomically renames ~/.cache/llm-z at node startup.
// Surviving engines retain open log files across the rename. Logs and benchmark
// runs then move to their platform-specific destinations.
func migrateLegacyState(log *slog.Logger) {
	if os.Getenv("MFSH_STATE_DIR") != "" {
		return
	}
	legacy, dir := legacyStateDir(), platformStateDir()
	if legacy == "" || dir == "" || legacy == dir || !exists(legacy) || exists(dir) {
		return
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		log.Warn("state not moved out of the cache directory; still using it", "from", legacy, "err", err)
		return
	}
	if err := os.Rename(legacy, dir); err != nil {
		// On rename failure, retain the legacy directory; copying live state could leave inconsistent files.
		log.Warn("state not moved out of the cache directory; still using it", "from", legacy, "to", dir, "err", err)
		return
	}
	type move struct{ from, to string }
	var moves []move
	// Move the log directory before node.log; creating its destination first makes the directory rename fail.
	if runtime.GOOS == "darwin" {
		moves = append(moves, move{filepath.Join(dir, "logs"), logDir()})
	}
	moves = append(moves,
		move{filepath.Join(dir, "node.log"), filepath.Join(logDir(), "node.log")},
		move{filepath.Join(dir, "bench"), benchDir()},
	)
	for _, m := range moves {
		if !exists(m.from) || m.from == m.to {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(m.to), 0o755); err != nil {
			// Skipping silently here still reported the migration as done.
			log.Warn("could not prepare the destination; leaving it where it is",
				"from", m.from, "to", m.to, "err", err)
			continue
		}
		// Rename replaces an existing file on Unix, so an already-migrated
		// destination would be overwritten by the stale copy.
		if exists(m.to) {
			log.Warn("destination already exists; leaving the old copy in place",
				"from", m.from, "to", m.to)
			continue
		}
		if err := os.Rename(m.from, m.to); err != nil && !errors.Is(err, os.ErrExist) {
			log.Warn("could not move", "from", m.from, "to", m.to, "err", err)
		}
	}
	log.Info("moved state and logs out of the cache directory", "from", legacy, "state", dir, "logs", logDir())
	fmt.Fprintf(os.Stderr, "ModelFabric: moved %s to %s (logs: %s)\n", legacy, dir, logDir())
}
