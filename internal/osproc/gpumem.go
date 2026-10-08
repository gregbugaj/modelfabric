package osproc

import (
	"context"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"
)

// GPUUse is the memory one process holds on one GPU.
type GPUUse struct {
	// GPU is the card's number as nvidia-smi lists it.
	GPU int   `json:"gpu"`
	MB  int64 `json:"mb"`
}

// GPUMemoryByPID reports, for each process using an NVIDIA GPU, how much
// memory it holds on each card. nil when nvidia-smi is not there or does not
// answer, which is the case on a Mac and on a machine with no NVIDIA GPU.
//
// It is how an engine that was left to use every GPU is shown for what it
// is. Asked for "every GPU", llama.cpp splits the model across them, and the
// only trace of that was two numbers in nvidia-smi: the dashboard called the
// engine "CUDA" and said nothing of two cards.
func GPUMemoryByPID(ctx context.Context) map[int][]GPUUse {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cards, err := exec.CommandContext(ctx, "nvidia-smi", "--query-gpu=index,uuid", "--format=csv,noheader").Output()
	if err != nil {
		return nil
	}
	apps, err := exec.CommandContext(ctx, "nvidia-smi", "--query-compute-apps=gpu_uuid,pid,used_memory", "--format=csv,noheader,nounits").Output()
	if err != nil {
		return nil
	}
	return parseGPUMemory(string(cards), string(apps))
}

// parseGPUMemory joins nvidia-smi's list of cards (index, uuid) with its list
// of compute processes (gpu_uuid, pid, used MiB).
func parseGPUMemory(cards, apps string) map[int][]GPUUse {
	index := map[string]int{}
	for _, line := range strings.Split(cards, "\n") {
		f := strings.Split(line, ",")
		if len(f) != 2 {
			continue
		}
		if n, err := strconv.Atoi(strings.TrimSpace(f[0])); err == nil {
			index[strings.TrimSpace(f[1])] = n
		}
	}
	out := map[int][]GPUUse{}
	for _, line := range strings.Split(apps, "\n") {
		f := strings.Split(line, ",")
		if len(f) != 3 {
			continue
		}
		gpu, known := index[strings.TrimSpace(f[0])]
		pid, err1 := strconv.Atoi(strings.TrimSpace(f[1]))
		mb, err2 := strconv.ParseInt(strings.TrimSpace(f[2]), 10, 64)
		if !known || err1 != nil || err2 != nil {
			continue
		}
		out[pid] = append(out[pid], GPUUse{GPU: gpu, MB: mb})
	}
	for pid := range out {
		sort.Slice(out[pid], func(a, b int) bool { return out[pid][a].GPU < out[pid][b].GPU })
	}
	return out
}
