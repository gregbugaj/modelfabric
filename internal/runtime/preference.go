package runtime

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Default runtime selection prefers a supported accelerator, then the newest
// version. Name ordering can select an older CPU build on a GPU host.

// accelerators lists detected backends in preference order. Undetected
// backends remain eligible but are not preferred.
func accelerators() []string {
	var out []string
	if hasNVIDIA() {
		out = append(out, "cuda")
	}
	if hasROCm() {
		out = append(out, "rocm", "hip")
	}
	if hasMetal() {
		out = append(out, "metal")
	}
	if hasNVIDIA() || hasROCm() || hasVulkanICD() {
		out = append(out, "vulkan")
	}
	return append(out, "cpu")
}

func hasNVIDIA() bool {
	if _, err := os.Stat("/proc/driver/nvidia/version"); err == nil {
		return true
	}
	if _, err := os.Stat("/dev/nvidiactl"); err == nil {
		return true
	}
	return false
}

func hasROCm() bool {
	_, err := os.Stat("/dev/kfd")
	return err == nil
}

func hasMetal() bool {
	return runtimeGOOS() == "darwin"
}

func hasVulkanICD() bool {
	for _, dir := range []string{"/usr/share/vulkan/icd.d", "/etc/vulkan/icd.d"} {
		if entries, err := os.ReadDir(dir); err == nil && len(entries) > 0 {
			return true
		}
	}
	matches, _ := filepath.Glob("/usr/lib/*/libvulkan.so*")
	return len(matches) > 0
}

func score(d *Definition, prefs []string) (int, []int) {
	backendRank := 0
	for i, a := range prefs {
		if strings.EqualFold(d.Backend, a) {
			backendRank = len(prefs) - i
			break
		}
	}
	return backendRank, versionParts(d.Version)
}

// versionParts splits versions into integers; nonnumeric segments sort as 0.
func versionParts(v string) []int {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	if v == "" {
		return nil
	}
	fields := strings.FieldsFunc(v, func(r rune) bool { return r == '.' || r == '-' || r == '+' })
	out := make([]int, 0, len(fields))
	for _, f := range fields {
		n, err := strconv.Atoi(f)
		if err != nil {
			// An upstream build tag is "b11040": Atoi fails on the leading
			// "b", and returning 0 made every such version compare equal, so
			// b11040 and b10662 were indistinguishable here.
			if digits := strings.TrimLeft(f, "bB"); digits != f {
				if n, err = strconv.Atoi(digits); err != nil {
					n = 0
				}
			} else {
				n = 0
			}
		}
		out = append(out, n)
	}
	return out
}

func compareVersions(a, b []int) int {
	for i := 0; i < len(a) || i < len(b); i++ {
		var x, y int
		if i < len(a) {
			x = a[i]
		}
		if i < len(b) {
			y = b[i]
		}
		if x != y {
			if x > y {
				return 1
			}
			return -1
		}
	}
	return 0
}

// newer compares runtimes of equal backend rank using upstream llama.cpp
// builds when known; version numbers from different packagers are incomparable.
func newer(a *Definition, aVer []int, b *Definition, bVer []int) bool {
	if a.LlamaBuild > 0 && b.LlamaBuild > 0 {
		return a.LlamaBuild > b.LlamaBuild
	}
	// Prefer a known upstream build over an incomparable packager version.
	if a.LlamaBuild > 0 != (b.LlamaBuild > 0) {
		return a.LlamaBuild > 0
	}
	return compareVersions(aVer, bVer) > 0
}

func best(defs []*Definition) *Definition {
	if len(defs) == 0 {
		return nil
	}
	prefs := accelerators()
	winner := defs[0]
	wRank, wVer := score(winner, prefs)
	for _, d := range defs[1:] {
		rank, ver := score(d, prefs)
		switch {
		case rank > wRank:
			winner, wRank, wVer = d, rank, ver
		case rank == wRank && newer(d, ver, winner, wVer):
			winner, wRank, wVer = d, rank, ver
		}
	}
	return winner
}
