package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// The project was called llm-z until September 2026, when its domain was
// registered by someone else and it became ModelFabric. Every directory it
// writes was renamed with it, as a clean break: `llmz` became `mfsh`, LLMZ_*
// became MFSH_*, and each llm-z directory below became a modelfabric one.
//
// Moving a directory is not enough on its own. Checksum pins record absolute
// paths (~/.llmz/models/...), configs name them, and a systemd unit or a
// hand-started node may still be using the old ones. So each old directory is
// renamed and a symlink is left at the old path pointing to the new one: every
// recorded path keeps resolving, and nothing is copied.
//
// It runs at the start of every command, not only when a node starts:
// `mfsh get` reads the config before any node is running, and would otherwise
// download into a default models root instead of the configured one.

type renamedDir struct {
	from, to string
	env      string // set, and the operator has chosen a location; leave it
}

func renamedDirs() []renamedDir {
	home, _ := os.UserHomeDir()
	var dirs []renamedDir
	if home != "" {
		dirs = append(dirs, renamedDir{filepath.Join(home, ".llmz"), filepath.Join(home, ".modelfabric"), "MFSH_HOME"})
	}
	if d, err := os.UserConfigDir(); err == nil {
		dirs = append(dirs, renamedDir{filepath.Join(d, "llm-z"), filepath.Join(d, "modelfabric"), "MFSH_CONFIG"})
	}
	if state := platformStateDir(); state != "" {
		dirs = append(dirs, renamedDir{filepath.Join(filepath.Dir(state), "llm-z"), state, "MFSH_STATE_DIR"})
	}
	switch runtime.GOOS {
	case "darwin":
		if home != "" {
			dirs = append(dirs, renamedDir{filepath.Join(home, "Library", "Logs", "llm-z"), filepath.Join(home, "Library", "Logs", "modelfabric"), "MFSH_LOG_DIR"})
		}
	case "linux", "freebsd", "openbsd":
		data := os.Getenv("XDG_DATA_HOME")
		if data == "" && home != "" {
			data = filepath.Join(home, ".local", "share")
		}
		if data != "" {
			dirs = append(dirs, renamedDir{filepath.Join(data, "llm-z"), filepath.Join(data, "modelfabric"), ""})
		}
	}
	return dirs
}

// migrateRename moves each llm-z directory to its ModelFabric name. It is
// safe to run every time: once moved, the old path is a symlink and is left
// alone.
func migrateRename() {
	for _, d := range renamedDirs() {
		if d.env != "" && os.Getenv(d.env) != "" {
			continue
		}
		if msg := moveRenamed(d.from, d.to); msg != "" {
			fmt.Fprintln(os.Stderr, "ModelFabric: "+msg)
		}
	}
	// The old variables are not read any more. Ignoring one silently would put
	// models, state or config somewhere other than where it was set to go.
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		if rest, ok := strings.CutPrefix(name, "LLMZ_"); ok {
			if os.Getenv("MFSH_"+rest) == "" {
				fmt.Fprintf(os.Stderr, "ModelFabric: %s is set but no longer read; rename it to MFSH_%s\n", name, rest)
			}
		}
	}
}

// moveRenamed moves from to to and leaves a symlink behind. It returns what it
// did or why it did not, for the operator, or "" when there was nothing to do.
func moveRenamed(from, to string) string {
	fi, err := os.Lstat(from)
	if err != nil || fi.Mode()&os.ModeSymlink != 0 || !fi.IsDir() {
		return "" // nothing there, or already moved
	}
	if _, err := os.Lstat(to); err == nil {
		// Both exist: something already writes to the new place. Merging two
		// trees of pins and ownership records could leave a node adopting the
		// wrong engines, so neither is touched.
		return fmt.Sprintf("both %s and %s exist; using %s. Move anything you need out of %s by hand", from, to, to, from)
	}
	if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
		return fmt.Sprintf("could not move %s: %v", from, err)
	}
	if err := os.Rename(from, to); err != nil {
		// Typically a different filesystem. Point the new name at the old
		// directory instead: nothing moves, and everything reads through the
		// new path. Copying models is not something to do unasked.
		if lerr := os.Symlink(from, to); lerr != nil {
			return fmt.Sprintf("could not move %s to %s (%v), or link it (%v); move it by hand", from, to, err, lerr)
		}
		return fmt.Sprintf("%s could not be moved (%v), so %s now points to it", from, err, to)
	}
	if runtime.GOOS == "windows" {
		return fmt.Sprintf("moved %s to %s", from, to) // symlinks need privileges there
	}
	if err := os.Symlink(to, from); err != nil {
		return fmt.Sprintf("moved %s to %s, but could not leave a link at the old path (%v); paths recorded under it will not resolve", from, to, err)
	}
	return fmt.Sprintf("moved %s to %s (the old path links to it)", from, to)
}
