package runtime

import (
	"strings"
	"testing"
)

func blackwell() Hardware {
	return Hardware{
		CPUFlags:    map[string]bool{"avx2": true},
		GPUs:        []GPU{{Name: "RTX 5090", ComputeCap: "12.0"}},
		CUDAVersion: "13.0",
	}
}

func cudaDef(name string, targets []string, minDriver int) *Definition {
	return &Definition{Name: name, Backend: "cuda", Version: "2.41.0",
		req: Requirements{CPUExtensions: []string{"AVX2"}, GPUFramework: "CUDA", GPUTargets: targets, MinDriver: minDriver}}
}

func TestCheckFitsMatchingCUDABuild(t *testing.T) {
	fit, why := cudaDef("cuda12", []string{"8.9", "12.0"}, 12040).Check(blackwell())
	if fit != FitYes {
		t.Fatalf("fit = %s (%v), want yes", fit, why)
	}
}

func TestCheckRejectsWrongArchitecture(t *testing.T) {
	fit, why := cudaDef("old", []string{"7.5", "8.0"}, 0).Check(blackwell())
	if fit != FitNo || !strings.Contains(strings.Join(why, " "), "12.0") {
		t.Fatalf("fit = %s (%v); a build without sm_120 cannot run on Blackwell", fit, why)
	}
}

func TestCheckRejectsOldDriver(t *testing.T) {
	hw := blackwell()
	hw.CUDAVersion = "12.2"
	fit, why := cudaDef("cuda12", []string{"12.0"}, 12040).Check(hw)
	if fit != FitNo || !strings.Contains(strings.Join(why, " "), "12.4") {
		t.Fatalf("fit = %s (%v); needs CUDA 12.4", fit, why)
	}
}

// A package that does not declare its architectures is unknown, not fine: an
// old CUDA 11 build fails on newer GPUs and its manifest will not say so.
func TestCheckUndeclaredTargetsIsUnknown(t *testing.T) {
	if fit, _ := cudaDef("legacy", nil, 0).Check(blackwell()); fit != FitUnknown {
		t.Fatalf("fit = %s, want unknown", fit)
	}
}

func TestCheckCPUAndMissingGPU(t *testing.T) {
	d := &Definition{Name: "cpu", req: Requirements{CPUExtensions: []string{"AVX512F"}}}
	if fit, _ := d.Check(blackwell()); fit != FitNo {
		t.Fatal("a missing CPU extension must be a no")
	}
	if fit, _ := cudaDef("c", []string{"12.0"}, 0).Check(Hardware{CPUFlags: map[string]bool{"avx2": true}}); fit != FitNo {
		t.Fatal("a CUDA build without an NVIDIA GPU must be a no")
	}
}

// Automatic selection must never pick a runtime known not to run here, and
// prefers a known fit over an unknown one — even when the unknown is newer.
func TestDefaultSkipsIncompatibleAndPrefersKnownFit(t *testing.T) {
	good := cudaDef("good", []string{"12.0"}, 12040)
	good.Version = "2.33.0"
	bad := cudaDef("bad", []string{"8.0"}, 0)
	bad.Version = "9.9.9" // newest, but cannot run
	legacy := cudaDef("legacy", nil, 0)
	legacy.Version = "9.0.0" // newer than good, but unverified
	r := &Registry{defs: []*Definition{good, bad, legacy}}
	r.SetHardware(blackwell())
	d, err := r.Default()
	if err != nil || d.Name != "good" {
		t.Fatalf("default = %v, %v; want the known-compatible build", d, err)
	}
}

func TestCUDAVersionCode(t *testing.T) {
	if cudaVersionCode("13.0") != 13000 || cudaVersionCode("12.4") != 12040 || cudaVersionCode("x") != 0 {
		t.Fatal("CUDA version encoding does not match the manifest format")
	}
}

// The engine's own device probe is evidence of fit for upstream builds, which
// do not declare GPU architectures. One probed entry cannot stand for two
// cards, and a GPU with no name is not evidence of anything.
func TestProbedDevicesMustCoverEachGPU(t *testing.T) {
	two := []GPU{{Name: "NVIDIA GeForce RTX 4090"}, {Name: "NVIDIA GeForce RTX 4090"}}
	if probedAll([]string{"NVIDIA GeForce RTX 4090"}, two) {
		t.Error("one probed entry satisfied two cards")
	}
	if !probedAll([]string{"NVIDIA GeForce RTX 4090", "NVIDIA GeForce RTX 4090"}, two) {
		t.Error("two matching entries should cover two cards")
	}
	if probedAll([]string{"anything"}, []GPU{{Name: ""}}) {
		t.Error("an unnamed GPU was treated as probed")
	}
	if !probedAll([]string{"device 0: NVIDIA GeForce RTX 5090, compute 12.0"}, []GPU{{Name: "NVIDIA GeForce RTX 5090"}}) {
		t.Error("a probe line containing the name should match")
	}
}
