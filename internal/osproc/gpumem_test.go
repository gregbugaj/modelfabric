package osproc

import (
	"reflect"
	"testing"
)

// nvidia-smi's own output from a machine with a 6000 Ada and a 4090, with one
// engine left to use both cards (2026-10-08) and one confined to the second.
func TestParseGPUMemory(t *testing.T) {
	const cards = "0, GPU-b49be9bb-ca30-4c02-1915-058f47d65275\n1, GPU-77e87bdf-cfc3-96c2-b8d8-2345a3d76f15\n"
	for _, c := range []struct {
		name, apps string
		want       map[int][]GPUUse
	}{
		{"one engine split across both cards",
			"GPU-b49be9bb-ca30-4c02-1915-058f47d65275, 24773, 25610\nGPU-77e87bdf-cfc3-96c2-b8d8-2345a3d76f15, 24773, 15220\n",
			map[int][]GPUUse{24773: {{GPU: 0, MB: 25610}, {GPU: 1, MB: 15220}}}},
		{"listed second card first: still in card order",
			"GPU-77e87bdf-cfc3-96c2-b8d8-2345a3d76f15, 7, 100\nGPU-b49be9bb-ca30-4c02-1915-058f47d65275, 7, 200\n",
			map[int][]GPUUse{7: {{GPU: 0, MB: 200}, {GPU: 1, MB: 100}}}},
		{"an engine on each card",
			"GPU-b49be9bb-ca30-4c02-1915-058f47d65275, 10508, 43622\nGPU-77e87bdf-cfc3-96c2-b8d8-2345a3d76f15, 10561, 21094\n",
			map[int][]GPUUse{10508: {{GPU: 0, MB: 43622}}, 10561: {{GPU: 1, MB: 21094}}}},
		{"nothing running", "", map[int][]GPUUse{}},
		{"a card nvidia-smi did not list, and a line it garbled, are left out",
			"GPU-unknown, 5, 100\nnot a line\nGPU-b49be9bb-ca30-4c02-1915-058f47d65275, 6, [N/A]\n", map[int][]GPUUse{}},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := parseGPUMemory(cards, c.apps); !reflect.DeepEqual(got, c.want) {
				t.Errorf("got %v, want %v", got, c.want)
			}
		})
	}
}
