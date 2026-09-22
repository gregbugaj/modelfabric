package discovery

import (
	"strings"
	"testing"
)

// profileBody returns one scheduling profile's plugin list from a generated
// config.
func profileBody(t *testing.T, cfg, name string) string {
	t.Helper()
	at := strings.Index(cfg, "  - name: "+name+"\n    plugins:\n")
	if at < 0 {
		t.Fatalf("no %q profile in:\n%s", name, cfg[strings.Index(cfg, "schedulingProfiles:"):])
	}
	body := cfg[at+len("  - name: "+name+"\n    plugins:\n"):]
	// Up to the next profile or the next top-level key.
	if end := strings.Index(body, "\n  - name: "); end >= 0 {
		return body[:end+1]
	}
	if end := strings.Index(body, "\n\n"); end >= 0 {
		return body[:end+1]
	}
	return body
}

// llm-d is given modelfabric.sh/vision on every endpoint and, until this, nothing
// that read it: under any profile an image could be scheduled onto an engine
// loaded without its projector, which fails inside llama.cpp with "failed to
// process mtmd chunk".
func TestEveryProfileCanFilterOnVision(t *testing.T) {
	for _, profile := range []string{ProfileLoadAware, ProfileOptimizedBaseline, ProfileTuned} {
		cfg := string(EPPConfig(EPPOptions{EndpointsPath: "/e.yaml", Profile: profile,
			PrefixCache: true, RoomFilter: true, Slots: 2}))
		for _, want := range []string{
			"type: label-selector-filter",
			"modelfabric.sh/vision: \"true\"",
			"type: header-profile-handler",
			"headerName: " + ProfileHeader,
			// Without this a request that names no profile fails outright:
			// measured against EPP v0.10.0, every headerless request came back
			// "ResourceExhausted - failed to find target endpoint".
			"defaultProfile: default",
		} {
			if !strings.Contains(cfg, want) {
				t.Errorf("%s: missing %q", profile, want)
			}
		}
	}
}

// The vision profile is the default profile with the filter in front. Scheduling
// an image differently from text would be an accident waiting to happen: the two
// are built from one list so they cannot drift.
func TestTheVisionProfileSchedulesLikeTheDefaultOne(t *testing.T) {
	for _, profile := range []string{ProfileLoadAware, ProfileOptimizedBaseline, ProfileTuned} {
		cfg := string(EPPConfig(EPPOptions{EndpointsPath: "/e.yaml", Profile: profile,
			PrefixCache: true, RoomFilter: true, Slots: 2, KVScorer: 1}))
		def := profileBody(t, cfg, "default")
		vis := profileBody(t, cfg, VisionProfile)
		const filter = "      - pluginRef: vision-filter\n"
		if !strings.HasPrefix(vis, filter) {
			t.Errorf("%s: the vision profile does not filter first:\n%s", profile, vis)
		}
		if rest := strings.TrimPrefix(vis, filter); rest != def {
			t.Errorf("%s: the two profiles schedule differently\ndefault:\n%s\nvision, past the filter:\n%s",
				profile, def, rest)
		}
		if strings.Contains(def, "vision-filter") {
			t.Errorf("%s: the default profile filters on vision, so text requests "+
				"would be confined to engines holding a projector:\n%s", profile, def)
		}
	}
}
