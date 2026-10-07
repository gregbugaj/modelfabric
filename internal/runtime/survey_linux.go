package runtime

import (
	"bufio"
	"context"
	"encoding/csv"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

func surveyHost(hw *Hardware) {
	if f, err := os.Open("/proc/cpuinfo"); err == nil {
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			k, v, ok := strings.Cut(sc.Text(), ":")
			if !ok {
				continue
			}
			switch strings.TrimSpace(k) {
			case "model name":
				if hw.CPU == "" {
					hw.CPU = strings.TrimSpace(v)
				}
			case "flags":
				if len(hw.CPUFlags) == 0 {
					for _, fl := range strings.Fields(v) {
						hw.CPUFlags[fl] = true
					}
				}
			}
		}
		f.Close()
	}
	hw.MemoryMB = int(readMemTotal() >> 20)
}

func surveyAccelerators(ctx context.Context, hw *Hardware) {
	if hasNVIDIA() {
		// A deadline each: sharing one meant a slow GPU query could leave the
		// banner call no time, and the CUDA version silently went unread.
		qctx, qcancel := context.WithTimeout(ctx, 5*time.Second)
		defer qcancel()
		if out, err := exec.CommandContext(qctx, "nvidia-smi",
			"--query-gpu=name,memory.total,compute_cap,driver_version",
			"--format=csv,noheader,nounits").Output(); err == nil {
			// Parsed as CSV, not split on every comma: --format=csv quotes a
			// field that contains one, and a GPU name like
			// "NVIDIA RTX A6000, 48GB" would otherwise shift every column.
			rows, _ := csv.NewReader(strings.NewReader(string(out))).ReadAll()
			for _, f := range rows {
				if len(f) < 4 {
					continue
				}
				mem, _ := strconv.Atoi(strings.TrimSpace(f[1]))
				hw.GPUs = append(hw.GPUs, GPU{
					Name: strings.TrimSpace(f[0]), MemoryMB: mem,
					ComputeCap: strings.TrimSpace(f[2]), Driver: strings.TrimSpace(f[3]),
				})
			}
		}
		// The CUDA version a driver supports appears only in the summary
		// banner, not in the query interface.
		bctx, bcancel := context.WithTimeout(ctx, 5*time.Second)
		defer bcancel()
		if out, err := exec.CommandContext(bctx, "nvidia-smi").Output(); err == nil {
			if m := regexp.MustCompile(`CUDA Version:\s*([0-9.]+)`).FindSubmatch(out); m != nil {
				hw.CUDAVersion = string(m[1])
			}
		}
	}
	hw.Vulkan = hasVulkanICD()
	hw.ROCm = hasROCm()
}
