package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// Migrate legacy llm-z directories to ModelFabric names, leaving symlinks so
// absolute paths in pins, configs and running services continue to resolve.
// Run before every command: commands such as get read config without starting a node.

type renamedDir struct {
	from, to string
	env      string // preserve explicitly configured locations
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
		// If rename fails across filesystems, link the new path to the old directory without copying model data.
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
