package main

import (
	"strings"
	"testing"
	"time"
)

// The server answers a load that is still running with the operation and no
// instance. `load -gpu each` printed that as a loaded engine with a blank id.
func TestGPULoadLineDoesNotTickAnEngineThatIsStillLoading(t *testing.T) {
	cases := []struct {
		name, instance, operation string
		want, not                 string
	}{
		{"a loaded engine is named", "inst-abc", "", "GPU 1 as inst-abc in 4.2s", "still loading"},
		{"a load still running names its operation and claims nothing", "", "op-7", "GPU 1 is still loading (operation op-7)", "✓"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := gpuLoadLine(1, c.instance, c.operation, 4200*time.Millisecond)
			if !strings.Contains(got, c.want) || strings.Contains(got, c.not) {
				t.Fatalf("line = %q; want it to contain %q and not %q", got, c.want, c.not)
			}
		})
	}
}
