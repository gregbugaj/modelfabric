package discovery

import "testing"
import "strings"

func TestKVOptionsRender(t *testing.T) {
	cfg := string(EPPConfig(EPPOptions{EndpointsPath: "/e", Profile: ProfileOptimizedBaseline, RoomFilter: true, Slots: 2, KVCeiling: 0.97, KVScorer: 2}))
	for _, want := range []string{"metric: kv-cache-utilization", "maxValue: 0.970", "type: kv-cache-utilization-scorer", "pluginRef: kv-scorer"} {
		if !strings.Contains(cfg, want) {
			t.Errorf("missing %q:\n%s", want, cfg)
		}
	}
	off := string(EPPConfig(EPPOptions{EndpointsPath: "/e", RoomFilter: true, Slots: 2}))
	if strings.Contains(off, "kv-cache-utilization") {
		t.Error("KV options must be off unless asked")
	}
}
