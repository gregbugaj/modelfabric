package catalog

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// ModelFiles lists every file a model needs on disk, relative to dir, the
// directory holding them. It is what a copy of the model has to carry.
//
// This is wider than what a load pins. A load names the first shard of a split
// GGUF and llama.cpp finds the rest by name, so the other shards never appear
// in the model; a copy that took only Path would move one piece of three and
// produce a model that cannot load. model.yaml is included too: it carries the
// publisher's sampling defaults, and a copy without it would behave differently
// from the original.
//
// A directory model (MLX today; any Hugging Face layout, as vLLM and SGLang
// read, would be the same) is every file under its directory.
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
