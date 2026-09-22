package discovery

import (
	"strings"
	"testing"

	"github.com/gregbugaj/modelfabric/internal/engineshim"
)

// Each assertion below guards a failure found by running the real EPP against a
// real llama.cpp engine — every one of them failed silently.

func TestEPPConfigMapsLlamaCppMetrics(t *testing.T) {
	cfg := string(EPPConfig(EPPOptions{EndpointsPath: "/x/endpoints.yaml"}))
	for _, want := range []string{
		`defaultEngine: llamacpp`,
		`queuedRequestsSpec: "llamacpp:requests_deferred"`,
		`runningRequestsSpec: "llamacpp:requests_processing"`,
		`path: "/x/endpoints.yaml"`,
		`watchFile: true`,
	} {
		if !strings.Contains(cfg, want) {
			t.Errorf("config missing %q", want)
		}
	}
}

// llama.cpp exports no KV gauge, but it does hold the number: ModelFabric computes
// it from /slots and republishes it on the port llm-d scrapes (see
// internal/engineshim), so the spec names that metric rather than being empty.
func TestEPPConfigReadsTheSynthesizedKVMetric(t *testing.T) {
	cfg := string(EPPConfig(EPPOptions{EndpointsPath: "/x", PrefixCache: true}))
	if !strings.Contains(cfg, `kvUsageSpec: "`+engineshim.KVUsageMetric+`"`) {
		t.Fatalf("the KV spec should name the gauge ModelFabric publishes:\n%s", cfg)
	}
	// Reading the gauge is not the same as ranking on it: the scorer stays
	// off until a profile is measured to be better with it.
	if strings.Contains(cfg, "kv-cache-utilization-scorer") {
		t.Fatal("kv-cache-utilization-scorer must not be configured unasked")
	}
}

// Without injectDefaults: false the EPP adds its own vLLM-mapped source, which
// double-scrapes every endpoint and errors on each scrape.
func TestEPPConfigDisablesDefaultInjection(t *testing.T) {
	if !strings.Contains(string(EPPConfig(EPPOptions{EndpointsPath: "/x"})), "injectDefaults: false") {
		t.Fatal("default metrics source injection must be disabled")
	}
}

func TestEPPConfigPrefixCacheIsOptional(t *testing.T) {
	on := string(EPPConfig(EPPOptions{EndpointsPath: "/x", PrefixCache: true}))
	off := string(EPPConfig(EPPOptions{EndpointsPath: "/x", PrefixCache: false}))
	if !strings.Contains(on, "prefix-cache-scorer") {
		t.Fatal("prefix-cache scorer missing when enabled")
	}
	if strings.Contains(off, "prefix-cache-scorer") || strings.Contains(off, "pluginRef: prefix") {
		t.Fatal("prefix-cache scorer present when disabled")
	}
	// The precise scorer needs KV events llama.cpp does not emit.
	if strings.Contains(on, "precise-prefix-cache-scorer") {
		t.Fatal("precise-prefix-cache-scorer cannot work with llama.cpp")
	}
}

// The EPP serves ext_proc over TLS by default; plaintext from Envoy makes every
// request 500 with nothing in the EPP's log.
func TestEnvoyConfigSpeaksTLSToEPP(t *testing.T) {
	cfg := string(EnvoyConfig(EnvoyOptions{ListenPort: 8080, EPPPort: 9002, AdminPort: 19000}))
	for _, want := range []string{
		"envoy.transport_sockets.tls",
		"UpstreamTlsContext",
		`alpn_protocols: ["h2"]`,
		"port_value: 8080",
		"port_value: 9002",
		"http_header_name: x-gateway-destination-endpoint",
	} {
		if !strings.Contains(cfg, want) {
			t.Errorf("envoy config missing %q", want)
		}
	}
}

// The room filter must run before prefix affinity in every profile, so warmth
// only ever chooses among engines with a free slot; its in-flight cap is used
// only for a pool of equal engines.
func TestRoomFilterRunsFirstInEveryProfile(t *testing.T) {
	for _, profile := range []string{ProfileLoadAware, ProfileOptimizedBaseline, ProfileTuned} {
		cfg := string(EPPConfig(EPPOptions{EndpointsPath: "/e.yaml", Profile: profile, PrefixCache: true, RoomFilter: true, Slots: 2}))
		// In both profiles. The vision profile puts its capability filter first
		// — an engine that cannot read the image is not a candidate at all —
		// and the room filter runs next, before anything scores warmth.
		for _, p := range []struct{ name, before string }{
			{"default", ""},
			{VisionProfile, "      - pluginRef: vision-filter\n"},
		} {
			body := strings.TrimPrefix(profileBody(t, cfg, p.name), p.before)
			first := strings.Index(body, "pluginRef:")
			if first < 0 || !strings.HasPrefix(body[first:], "pluginRef: room-filter") {
				t.Errorf("%s, %s profile: room-filter is not the first plugin:\n%s",
					profile, p.name, body)
			}
		}
		for _, want := range []string{"type: utilization-filter", "fallbackOnEmpty: true", "metric: waiting-queue", "metric: active-requests\n          maxValue: 1", "type: inflight-load-producer"} {
			if !strings.Contains(cfg, want) {
				t.Errorf("%s: missing %q", profile, want)
			}
		}
	}
	mixed := string(EPPConfig(EPPOptions{EndpointsPath: "/e.yaml", Profile: ProfileOptimizedBaseline, RoomFilter: true}))
	if strings.Contains(mixed, "active-requests") || !strings.Contains(mixed, "waiting-queue") {
		t.Error("a pool of unequal engines gets only the per-engine queue condition")
	}
	off := string(EPPConfig(EPPOptions{EndpointsPath: "/e.yaml", Profile: ProfileOptimizedBaseline, Slots: 2}))
	if strings.Contains(off, "room-filter") {
		t.Error("without RoomFilter the profile must be llm-d's own")
	}
}

// Envoy's ORIGINAL_DST cluster forwards to whatever
// x-gateway-destination-endpoint says. The EPP sets that header; a client must
// not be able to, or it chooses the upstream. It is removed before ext_proc
// runs, so the EPP's own value is the only one that survives.
func TestEnvoyStripsAClientSuppliedDestination(t *testing.T) {
	cfg := string(EnvoyConfig(EnvoyOptions{ListenPort: 8080, EPPPort: 9002, AdminPort: 19000}))
	mut := strings.Index(cfg, "envoy.filters.http.header_mutation")
	ext := strings.Index(cfg, "envoy.filters.http.ext_proc")
	if mut < 0 {
		t.Fatal("no filter removes the client's destination header")
	}
	if mut > ext {
		t.Error("the header must be removed before ext_proc sets its own")
	}
	if !strings.Contains(cfg, "- remove: x-gateway-destination-endpoint") {
		t.Error("the removal does not name the destination header")
	}
}

// Under llm-d the front door learns which engine served a request only from
// what Envoy tells it. It used to look for a header nothing set, so every
// such request was recorded with no node. Envoy must report the address it
// dialled, and overwrite the header so an engine cannot name another.
func TestEnvoyReportsTheUpstreamItDialled(t *testing.T) {
	cfg := string(EnvoyConfig(EnvoyOptions{}))
	for _, want := range []string{"key: x-fabric-upstream", `value: "%UPSTREAM_REMOTE_ADDRESS%"`, "append_action: OVERWRITE_IF_EXISTS_OR_ADD"} {
		if !strings.Contains(cfg, want) {
			t.Errorf("envoy config has no %s", want)
		}
	}
	if strings.Contains(cfg, "%!") {
		t.Fatalf("a %% in the template was read as a format verb:\n%s", cfg)
	}
}
