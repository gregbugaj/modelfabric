package bench

import (
	"bytes"
	"context"
	"os/exec"
	goruntime "runtime"
	"strconv"
	"strings"
	"time"
)

// NvidiaMemory is the GPU memory a process holds, from nvidia-smi: the
// engine's own, not the whole GPU's. A whole-GPU reading counts whatever else
// shares the card — a training run holding 26 GB of a 32 GB 5090 would have
// been reported as the model's peak.
func NvidiaMemory(pid int) (int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "nvidia-smi", "--query-compute-apps=pid,used_memory", "--format=csv,noheader,nounits").Output()
	if err != nil {
		return 0, err
	}
	total := 0
	for _, line := range strings.Split(string(bytes.TrimSpace(out)), "\n") {
		f := strings.Split(line, ",")
		if len(f) != 2 {
			continue
		}
		p, err1 := strconv.Atoi(strings.TrimSpace(f[0]))
		mb, err2 := strconv.Atoi(strings.TrimSpace(f[1]))
		if err1 == nil && err2 == nil && p == pid {
			total += mb
		}
	}
	return total, nil
}

// GPUInfo is the first GPU's name, driver and memory, for the report. On a
// Mac it is the chip and its unified memory: there is no nvidia-smi there, and
// a report from helion said nothing at all about the hardware it measured.
func GPUInfo() (name, driver string, mb int) {
	if goruntime.GOOS == "darwin" {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		out, err := exec.CommandContext(ctx, "sysctl", "-n", "machdep.cpu.brand_string", "hw.memsize").Output()
		if err != nil {
			return "", "", 0
		}
		return macGPU(string(out))
	}
	return nvidiaGPU()
}

// macGPU reads sysctl's two lines: the chip, then memory in bytes. The memory
// is unified — the GPU's and the system's are the same — and is labelled so.
func macGPU(out string) (name, driver string, mb int) {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) < 2 {
		return "", "", 0
	}
	b, err := strconv.ParseInt(strings.TrimSpace(lines[1]), 10, 64)
	if err != nil {
		return strings.TrimSpace(lines[0]), "", 0
	}
	return strings.TrimSpace(lines[0]) + " (unified memory)", "", int(b >> 20)
}

func nvidiaGPU() (name, driver string, mb int) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "nvidia-smi", "--query-gpu=name,driver_version,memory.total", "--format=csv,noheader,nounits").Output()
	if err != nil {
		return "", "", 0
	}
	line, _, _ := strings.Cut(string(bytes.TrimSpace(out)), "\n")
	f := strings.Split(line, ",")
	if len(f) < 3 {
		return "", "", 0
	}
	mb, _ = strconv.Atoi(strings.TrimSpace(f[2]))
	return strings.TrimSpace(f[0]), strings.TrimSpace(f[1]), mb
}
