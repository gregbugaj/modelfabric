package catalog

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// ModelFiles lists the files needed to copy a model, relative to dir.
// GGUF copies include all shards and model.yaml to preserve loadability and
// sampling defaults. Directory models include every file below their root.
func ModelFiles(m Model) (dir string, files []string, err error) {
	info, err := os.Stat(m.Path)
	if err != nil {
		return "", nil, err
	}
	if info.IsDir() {
		files, _, err := mlxFiles(m.Path)
		if err != nil {
			return "", nil, err
		}
		if len(files) == 0 {
			return "", nil, fmt.Errorf("%s holds no files", m.Path)
		}
		return m.Path, files, nil
	}

	dir = filepath.Dir(m.Path)
	files = []string{filepath.Base(m.Path)}
	if prefix := shardGroup(m.Path); prefix != "" {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return "", nil, err
		}
		base := filepath.Base(prefix)
		for _, e := range entries {
			name := e.Name()
			if name == files[0] || e.IsDir() || !isNonFirstShard(name) {
				continue
			}
			if shardGroup(filepath.Join(dir, name)) == prefix && strings.HasPrefix(name, base) {
				files = append(files, name)
			}
		}
	}
	if m.Projector != "" {
		// The catalog pairs projectors from the same directory, so one found
		// anywhere else is not something this layout can carry.
		if filepath.Dir(m.Projector) != dir {
			return "", nil, fmt.Errorf("projector %s is not beside %s", m.Projector, m.Path)
		}
		files = append(files, filepath.Base(m.Projector))
	}
	if fi, err := os.Stat(filepath.Join(dir, "model.yaml")); err == nil && fi.Mode().IsRegular() {
		files = append(files, "model.yaml")
	}
	sort.Strings(files[1:])
	return dir, files, nil
}
