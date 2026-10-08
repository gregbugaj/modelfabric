package runtime

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Choosing which GPU an engine runs on.
//
// llama.cpp, told nothing, spreads a model's layers over every GPU it can
// see. That is the right default for a model too large for one card and the
// wrong one for a machine with two cards and a model that fits on either: the
// model runs at the pace of the slower card, pays a transfer between them on
// every token, and the second card adds no slots. One engine on each card is
// what such a machine is for, and the router already places conversations
// across several engines on a node.
//
// The choice is made with CUDA's own environment variables and not
// llama.cpp's --device, for two reasons. CUDA numbers devices fastest first
// unless told otherwise, so "--device CUDA0" is not the card nvidia-smi calls
// 0, and on a machine with a 6000 Ada and a 4090 the two orders differ.
// CUDA_DEVICE_ORDER=PCI_BUS_ID makes CUDA's numbering nvidia-smi's, which is
// the numbering the hardware survey, `mfsh doctor` and the dashboard show.
// And an engine that cannot see a card cannot put anything on it, whatever
// a later flag or a draft model's own offload does.

// ParseGPUs reads a gpu setting into indices, lowest first. "" and "all" are
// no choice, and return nil.
func ParseGPUs(v string) ([]int, error) {
	v = strings.TrimSpace(v)
	if v == "" || v == "all" {
		return nil, nil
	}
	seen := map[int]bool{}
	var out []int
	for _, part := range strings.Split(v, ",") {
		n, err := strconv.Atoi(strings.TrimSpace(part))
		if err != nil || n < 0 {
			return nil, fmt.Errorf("gpu must be \"all\" or GPU numbers as nvidia-smi lists them, such as 0 or 0,1 (got %q)", v)
		}
		if !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	sort.Ints(out)
	return out, nil
}

// normalGPU is a gpu setting in the one form it is stored in: "" for no
// choice, otherwise indices lowest first, so "1,0" and "0, 1" are one
// configuration and not two.
func normalGPU(v string) string {
	ids, err := ParseGPUs(v)
	if err != nil || len(ids) == 0 {
		return ""
	}
	parts := make([]string, len(ids))
	for i, n := range ids {
		parts[i] = strconv.Itoa(n)
	}
	return strings.Join(parts, ",")
}

// CheckGPUs reports whether a gpu setting can be honoured by this runtime on
// this hardware, with what to do about it when it cannot.
func CheckGPUs(d *Definition, gpu string, hw Hardware) error {
	ids, err := ParseGPUs(gpu)
	if err != nil || len(ids) == 0 {
		return err
	}
	if d.Backend != "cuda" {
		return fmt.Errorf("choosing a GPU works with CUDA runtimes; %s uses %s. Leave gpu unset, or select a CUDA runtime with `mfsh runtime select`", d.Name, orUnknown(d.Backend))
	}
	if len(hw.GPUs) == 0 {
		return fmt.Errorf("gpu %s was asked for but nvidia-smi reports no GPU on this machine; see `mfsh doctor`", gpu)
	}
	for _, n := range ids {
		if n >= len(hw.GPUs) {
			return fmt.Errorf("there is no GPU %d here: this machine has %s", n, gpuList(hw))
		}
	}
	return nil
}

func orUnknown(s string) string {
	if s == "" {
		return "an unknown backend"
	}
	return s
}

// gpuList is the machine's GPUs as a person would be told them: "GPU 0 (RTX
// 6000 Ada, 48 GB) and GPU 1 (RTX 4090, 24 GB)".
func gpuList(hw Hardware) string {
	parts := make([]string, len(hw.GPUs))
	for i, g := range hw.GPUs {
		parts[i] = fmt.Sprintf("GPU %d (%s, %d GB)", i, strings.TrimPrefix(g.Name, "NVIDIA "), (g.MemoryMB+512)/1024)
	}
	if len(parts) < 2 {
		return strings.Join(parts, "")
	}
	return strings.Join(parts[:len(parts)-1], ", ") + " and " + parts[len(parts)-1]
}

// GPUEnv is the environment that confines an engine to the GPUs in a.GPU,
// nil when no choice was made.
func GPUEnv(a Applied) []string {
	if a.GPU == "" {
		return nil
	}
	return []string{"CUDA_DEVICE_ORDER=PCI_BUS_ID", "CUDA_VISIBLE_DEVICES=" + a.GPU}
}
