package runtime

import (
	"context"
	"strings"
	"syscall"
)

// On Apple silicon the CPU, the GPU and the host share one pool of memory, so
// the survey reports the chip once as the CPU and once as a GPU whose MemoryMB
// is what Metal will actually hand out — not a second, separate pool.

// surveyHost fills in the CPU and host memory from sysctl.
func surveyHost(hw *Hardware) {
	if brand, err := syscall.Sysctl("machdep.cpu.brand_string"); err == nil {
		hw.CPU = strings.TrimSpace(brand)
	}
	// Apple silicon always has these; ModelFabric uses them the way it uses x86
	// flags, to match an engine package's declared requirements.
	if strings.HasPrefix(hw.CPU, "Apple") {
		hw.CPUFlags["advsimd"] = true
		hw.CPUFlags["neon"] = true
	}
	hw.MemoryMB = int(readMemTotal() >> 20)
}

// surveyAccelerators reports the Apple GPU. Metal is part of the OS, so there
// is nothing to detect: what matters is how much of the shared memory it may
// use. macOS keeps the rest for everything else, and a machine can be told to
// allow more with iogpu.wired_limit_mb.
func surveyAccelerators(_ context.Context, hw *Hardware) {
	if !strings.HasPrefix(hw.CPU, "Apple") || hw.MemoryMB == 0 {
		return
	}
	budget := hw.MemoryMB / 4 * 3
	// Decoded as a number, not as text: syscall.Sysctl returns this OID's raw
	// little-endian bytes, so parsing it as a decimal string always failed and
	// an operator who raised the wired limit was silently ignored.
	if mb, ok := sysctlUint("iogpu.wired_limit_mb"); ok && mb > 0 && int(mb) <= hw.MemoryMB {
		// Never more than the machine has: unified memory comes out of host
		// RAM, so a misconfigured or misread limit claiming more than that is
		// not a budget, it is a wrong number that placement would believe.
		budget = int(mb)
	}
	hw.GPUs = append(hw.GPUs, GPU{Name: hw.CPU, MemoryMB: budget, Unified: true})
}
