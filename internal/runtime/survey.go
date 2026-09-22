package runtime

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

// Hardware survey, like `lms runtime survey`: what this machine has, and
// whether each runtime can use it.
//
// Engine packages declare their requirements — CPU instruction set extensions,
// the GPU framework, the CUDA compute capabilities they were built for, and a
// minimum driver — so compatibility is checked against facts rather than
// guessed from a package name.

// GPU is one accelerator.
type GPU struct {
	Name       string `json:"name"`
	MemoryMB   int    `json:"memory_mb"`
	ComputeCap string `json:"compute_cap,omitempty"` // CUDA compute capability, e.g. "12.0"
	Driver     string `json:"driver,omitempty"`
	// Unified marks memory shared with the host (Apple silicon): MemoryMB is
	// a budget out of Hardware.MemoryMB, not separate VRAM, so the two must
	// never be added together.
	Unified bool `json:"unified,omitempty"`
}

// Hardware is what the survey found.
type Hardware struct {
	CPU         string          `json:"cpu"`
	CPUFlags    map[string]bool `json:"-"`
	MemoryMB    int             `json:"memory_mb"`
	GPUs        []GPU           `json:"gpus"`
	CUDAVersion string          `json:"cuda_version,omitempty"` // highest the driver supports
	Vulkan      bool            `json:"vulkan"`
	ROCm        bool            `json:"rocm"` // an AMD compute device (/dev/kfd)
}

// clone deep-copies the reference fields, so a survey handed across a lock
// boundary cannot be changed from the other side.
func (h Hardware) clone() Hardware {
	c := h
	if h.CPUFlags != nil {
		c.CPUFlags = make(map[string]bool, len(h.CPUFlags))
		for k, v := range h.CPUFlags {
			c.CPUFlags[k] = v
		}
	}
	c.GPUs = append([]GPU(nil), h.GPUs...)
	return c
}

// Survey inspects the local machine. Missing tools degrade to "unknown"; the
// survey never fails outright.
func Survey(ctx context.Context) Hardware {
	hw := Hardware{CPUFlags: map[string]bool{}}
	surveyHost(&hw)
	surveyAccelerators(ctx, &hw)
	return hw
}

// Fit is a runtime's compatibility with the surveyed hardware.
type Fit string

const (
	FitYes     Fit = "yes"
	FitNo      Fit = "no"
	FitUnknown Fit = "unknown" // the package does not declare enough to tell
)

// Requirements are what a package declares it needs.
type Requirements struct {
	CPUExtensions []string
	GPUFramework  string   // "CUDA", "Vulkan", ...
	GPUTargets    []string // CUDA compute capabilities
	MinDriver     int      // CUDA version as major*1000+minor*10, e.g. 12040
	// ProbedDevices are accelerators the engine itself enumerated on this
	// machine when ModelFabric installed it — evidence of fit for upstream builds,
	// which do not declare their GPU architectures.
	ProbedDevices []string
}

// Check reports whether a runtime can run here, with reasons for anything
// other than a clean yes.
func (d *Definition) Check(hw Hardware) (Fit, []string) {
	var reasons []string
	fit := FitYes
	fail := func(r string) { fit = FitNo; reasons = append(reasons, r) }
	unknown := func(r string) {
		if fit == FitYes {
			fit = FitUnknown
		}
		reasons = append(reasons, r)
	}

	for _, ext := range d.req.CPUExtensions {
		switch {
		case len(hw.CPUFlags) == 0:
			// Discovery failed, so nothing is known — which is not the same as
			// "the requirement is met". Treating it as met offered a runtime
			// that needs AVX2 on a machine that may not have it, and the
			// engine dies with an illegal instruction rather than a reason.
			unknown("cannot tell whether this CPU has " + ext)
		case !hw.CPUFlags[strings.ToLower(ext)]:
			fail("CPU lacks " + ext)
		}
	}

	switch strings.ToLower(d.req.GPUFramework) {
	case "":
	case "cuda":
		if len(hw.GPUs) == 0 {
			fail("no NVIDIA GPU")
			break
		}
		if len(d.req.GPUTargets) == 0 {
			// Old packages omit targets. A CUDA 11 build cannot run on newer
			// architectures, but the manifest will not say which it is. An
			// install-time probe that found every GPU here is evidence
			// instead; one that found none, or other GPUs, is not.
			if !probedAll(d.req.ProbedDevices, hw.GPUs) {
				unknown("package does not declare which GPU architectures it supports")
			}
		} else {
			for _, g := range hw.GPUs {
				if !contains(d.req.GPUTargets, g.ComputeCap) {
					fail(fmt.Sprintf("built for compute %s, %s is %s",
						strings.Join(d.req.GPUTargets, "/"), g.Name, g.ComputeCap))
				}
			}
		}
		if d.req.MinDriver > 0 {
			if have := cudaVersionCode(hw.CUDAVersion); have == 0 {
				unknown("could not read the CUDA version the driver supports")
			} else if have < d.req.MinDriver {
				fail(fmt.Sprintf("needs CUDA %s, driver supports %s", cudaVersionString(d.req.MinDriver), hw.CUDAVersion))
			}
		}
	case "vulkan":
		if !hw.Vulkan {
			fail("no Vulkan driver found")
		}
	}
	return fit, reasons
}

// probedAll reports whether an install-time probe enumerated every GPU the
// survey found, matched by name.
// probedAll reports whether the engine's own device probe covers every GPU
// here. Each GPU needs its own probed entry: one entry naming "RTX 4090" used
// to satisfy two cards of that name, and an unnamed GPU matched anything at
// all, because strings.Contains(p, "") is always true.
func probedAll(probed []string, gpus []GPU) bool {
	if len(probed) == 0 || len(gpus) == 0 {
		return false
	}
	used := make([]bool, len(probed))
	for _, g := range gpus {
		if g.Name == "" {
			return false // no name is no evidence
		}
		found := false
		for i, p := range probed {
			if !used[i] && strings.Contains(p, g.Name) {
				used[i], found = true, true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// cudaVersionCode turns "13.0" into 13000, the encoding manifests use.
func cudaVersionCode(v string) int {
	major, minor, ok := strings.Cut(v, ".")
	if !ok {
		return 0
	}
	a, err1 := strconv.Atoi(major)
	b, err2 := strconv.Atoi(minor)
	if err1 != nil || err2 != nil {
		return 0
	}
	return a*1000 + b*10
}

func cudaVersionString(code int) string {
	return fmt.Sprintf("%d.%d", code/1000, (code%1000)/10)
}
