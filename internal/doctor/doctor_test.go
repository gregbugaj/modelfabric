package doctor

import (
	"strings"
	"testing"

	"github.com/gregbugaj/modelfabric/internal/config"
)

// engine_bind "tailnet" is the node's Tailscale address, looked up at start.
// Doctor treated anything it did not recognise as a non-tailnet address and
// warned that unauthenticated engines were on the LAN.
func TestEngineBindVerdicts(t *testing.T) {
	tests := []struct {
		bind string
		want Status
		fix  string // a substring of the fix, when there is one
	}{
		{bind: "", want: StatusOK},
		{bind: "tailnet", want: StatusOK},
		{bind: "100.107.225.6", want: StatusOK, fix: `"tailnet"`},
		{bind: "0.0.0.0", want: StatusWarn, fix: `"tailnet"`},
		{bind: "192.168.1.10", want: StatusWarn, fix: `"tailnet"`},
	}
	for _, tc := range tests {
		t.Run("engine_bind="+tc.bind, func(t *testing.T) {
			d := &doctor{cfg: config.Config{EngineBind: tc.bind}}
			d.checkSecurity()
			for _, c := range d.checks {
				if c.Name != "engines" {
					continue
				}
				if c.Status != tc.want || !strings.Contains(c.Fix, tc.fix) {
					t.Fatalf("got %v %q (fix %q), want %v with fix containing %q", c.Status, c.Detail, c.Fix, tc.want, tc.fix)
				}
				return
			}
			t.Fatal("no engines check")
		})
	}
}

// The shim was invisible: a proxy in the path of everything llm-d schedules,
// and of everything the disk cache places, that no view mentioned.
func TestShimJobs(t *testing.T) {
	tests := []struct {
		name              string
		kv, tokens, cache bool
		ceiling           int
		want              string
	}{
		{name: "everything on", kv: true, tokens: true, ceiling: 16384, cache: true,
			want: "KV-cache gauge for llm-d · live tokens · output ceiling 16384 · disk prompt cache, for every routing method"},
		{name: "cache off and no ceiling say so", kv: true,
			want: "KV-cache gauge for llm-d · disk prompt cache off"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := shimJobs(tc.kv, tc.tokens, tc.ceiling, tc.cache); got != tc.want {
				t.Fatalf("got  %q\nwant %q", got, tc.want)
			}
		})
	}
}
