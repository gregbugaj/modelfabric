package rtpkg

import (
	"strings"

	"github.com/gregbugaj/modelfabric/internal/runtime"
)

// Update is one installed variant and the newest upstream build of it.
type Update struct {
	Family    string `json:"family"`    // package name without version
	Installed int    `json:"installed"` // newest installed llama.cpp build
	Latest    *Plan  `json:"-"`
	Available bool   `json:"available"` // a newer build exists
	Err       string `json:"error,omitempty"`
}

// Updates checks every ModelFabric-installed variant under root against builds.
// Old builds of a variant do not count: only its newest installed one.
func Updates(root string, builds []Build, hw runtime.Hardware) ([]Update, error) {
	defs, err := runtime.DiscoverPackages(root, runtime.OriginUpstream)
	if err != nil {
		return nil, err
	}
	newest := map[string]int{}
	var order []string
	for _, d := range defs {
		if d.Provenance() == nil {
			continue
		}
		family, _, _ := strings.Cut(d.Name, "@")
		if _, seen := newest[family]; !seen {
			order = append(order, family)
		}
		newest[family] = max(newest[family], d.LlamaBuild)
	}
	out := make([]Update, 0, len(order))
	for _, family := range order {
		u := Update{Family: family, Installed: newest[family]}
		if p, err := Choose(builds, WantFromName(family), hw); err != nil {
			u.Err = err.Error()
		} else {
			u.Latest = p
			u.Available = p.BuildNumber > u.Installed
		}
		out = append(out, u)
	}
	return out, nil
}

// WantFromName recovers the variant from a package name such as
// llama.cpp-upstream-linux-x86_64-cuda-12.8.
func WantFromName(name string) Want {
	parts := strings.Split(name, "-")
	var w Want
	for i, p := range parts {
		switch p {
		case "cpu", "vulkan", "rocm":
			w.Backend = p
		case "cuda":
			w.Backend = p
			if i+1 < len(parts) {
				w.CUDA = parts[i+1]
			}
		}
	}
	return w
}
