package discovery

import (
	"fmt"
	"github.com/gregbugaj/modelfabric/internal/engineshim"
	"strings"
)

// Configuration for running llm-d's EPP against a ModelFabric mesh.
//
// The EPP chooses how to read an endpoint's Prometheus metrics from the
// endpoint's engine-type label, and ships mappings for vLLM, SGLang and the
// TensorRT-LLM family — not llama.cpp. Supporting llama.cpp is configuration,
// not code: an extra engine entry for the core-metrics-extractor.
//
// The mapping below is taken from what this llama.cpp build actually serves on
// /metrics, not from documentation:
//
//	llamacpp:requests_deferred    -> queued requests
//	llamacpp:requests_processing  -> running requests
//
// There is no KV-cache utilisation metric in this build (older write-ups name
// llamacpp:kv_cache_usage_ratio; it is not present). The engine shim supplies
// the KV utilization gauge instead. The generated config maps that gauge;
// a KV utilization filter or scorer is included when its option is enabled.

const (
	// EngineTypeLabel is the endpoint label the EPP reads to pick a mapping.
	EngineTypeLabel = "llm-d.ai/engine-type"
	// EngineTypeLlamaCPP names the mapping defined in EPPConfig.
	EngineTypeLlamaCPP = "llamacpp"
	// SlotsLabel is the engine's concurrent request capacity (--parallel).
	SlotsLabel = "modelfabric.sh/slots"
	// PrefillLabel is the engine's measured prompt rate, tokens per second.
	PrefillLabel = "modelfabric.sh/prefill-tok-s"
	// KVUsageLabel is the engine's KV-cache utilization at the time the file
	// was written, for an operator reading it; the EPP reads the live gauge
	// from the endpoint, not this.
	KVUsageLabel = "modelfabric.sh/kv-usage"
	// ServedModelLabel is the id this engine answers to, when it is not the
	// model's own name (mlx-lm). Anything that dials the engine directly must
	// send that id instead — so a scheduler that cannot rewrite the request
	// must not dial it at all. See Endpoint.NeedsRewrite.
	ServedModelLabel = "modelfabric.sh/served-model"
	// VisionLabel is "true" when the engine was loaded with its image
	// projector. The visionProfile filters on it; see there for why a filter
	// and not a scorer.
	VisionLabel = "modelfabric.sh/vision"
)

// ProfileHeader names the scheduling profile to use for one request. ModelFabric sets
// it on the way to Envoy; a caller that dials Envoy directly sets nothing and
// gets the default profile.
const ProfileHeader = "x-fabric-profile"

// VisionProfile is the profile for a request carrying an image.
const VisionProfile = "vision"

// EPPOptions shapes the generated EPP config.
type EPPOptions struct {
	// EndpointsPath is where ModelFabric writes the endpoint list.
	EndpointsPath string
	// Profile names the scheduling profile (see Profiles). Empty means
	// ProfileLoadAware.
	Profile string
	// PrefixCache adds the approximate prefix-cache scorer to the load-aware
	// profile. It hashes prompts inside the EPP and needs no engine events,
	// so unlike precise-prefix-cache-scorer it works with llama.cpp.
	PrefixCache bool
	// PeakPrefillThroughput (tokens/s) is how fast an engine prefills, used by
	// prefix-cache-affinity-filter to turn queued tokens into an estimated
	// TTFT. llm-d's default (15928) is calibrated for Qwen 32B on 2x H100;
	// llama.cpp on one workstation GPU is several times slower, and a too-high
	// value makes every queue look short. Zero means DefaultPeakPrefill.
	PeakPrefillThroughput float64
	// RoomFilter puts roomFilter ahead of every profile's own decisions.
	RoomFilter bool
	// Slots is the per-engine request capacity when every engine in the pool
	// has the same one; zero when they differ or are unknown. It sets the
	// room filter's in-flight cap (see roomFilter).
	Slots int
	// KVCeiling refuses engines whose KV cache is fuller than this share
	// (0..1), reading the gauge ModelFabric synthesizes. Zero leaves it out.
	//
	// It is a safety valve against eviction thrash, not load balancing: on
	// llama.cpp the tokens counted are prefixes kept for reuse, so a warm
	// engine reads high, and a low ceiling would exclude the very engine
	// prefix affinity wants. Useful near 1.
	KVCeiling float64
	// KVScorer adds llm-d's kv-cache-utilization-scorer at this weight, which
	// prefers the emptier cache. Zero leaves it out. On llama.cpp that pulls
	// against prefix affinity for the same reason as KVCeiling — measure it
	// before trusting it.
	KVScorer int
}

// Scheduling profiles ModelFabric offers, named after llm-d's well-lit paths where
// one applies. Each is plain EPP configuration, runnable with llama.cpp.
const (
	ProfileLoadAware         = "load-aware"
	ProfileOptimizedBaseline = "optimized-baseline"
	ProfileTuned             = "tuned"
)

// DefaultPeakPrefill is a conservative prefill rate for a ~30B model under
// llama.cpp on one GPU, used until an engine's own counters say otherwise.
const DefaultPeakPrefill = 2000

// ProfileInfo describes a profile for the CLI and dashboard.
type ProfileInfo struct {
	Name      string `json:"name"`
	Title     string `json:"title"`
	Summary   string `json:"summary"`
	WellLit   string `json:"well_lit_path,omitempty"` // llm-d guide it follows
	Available bool   `json:"available"`
	Reason    string `json:"reason,omitempty"` // why not, when unavailable
	// Experimental marks profiles that load llm-d plugins at Alpha stability;
	// the EPP refuses them without --allow-experimental-plugins.
	Experimental bool `json:"experimental,omitempty"`
}

const wellLit = "https://github.com/llm-d/llm-d/blob/main/docs/well-lit-paths/"

// Profiles lists what can be selected, and — so the choice is informed — the
// well-lit paths that llama.cpp cannot run, with the reason.
var Profiles = []ProfileInfo{
	{Name: ProfileLoadAware, Title: "Load + prefix (classic)", Available: true,
		Summary: "Queue depth and running requests from each engine's metrics, plus approximate prefix-cache scoring. What ModelFabric shipped first."},
	{Name: ProfileOptimizedBaseline, Title: "Optimized Baseline", Available: true,
		WellLit: wellLit + "foundations/optimized-baseline.md",
		Summary: "llm-d's Optimized Baseline: route to the engine holding the prompt's prefix unless that would cost more TTFT than it saves (prefix-cache-affinity-filter, calibrated to these GPUs), then balance by in-flight tokens."},
	{Name: ProfileTuned, Title: "Optimized Baseline + fan-out", Available: true, Experimental: true,
		WellLit: wellLit + "workloads/agentic-serving.md",
		Summary: "Optimized Baseline, plus: requests sharing a prompt that arrive together are co-located so the prompt is prefilled once (burst-prefix-cache-producer), and prompts with no cache anywhere spread to the least recently used engine (no-hit-lru-scorer). For parallel fan-out: RAG over shared documents, agents, best-of-N."},
	{Name: "precise-prefix-cache-routing", Title: "Precise Prefix-Cache Aware Routing",
		WellLit: wellLit + "foundations/precise-prefix-cache-routing.md",
		Reason:  "needs KV-cache events from the engine (vLLM, SGLang, TensorRT-LLM); llama.cpp publishes none"},
	{Name: "pd-disaggregation", Title: "Prefill/Decode Disaggregation",
		WellLit: wellLit + "foundations/pd-disaggregation.md",
		Reason:  "needs KV transfer between engines (vLLM NIXL/Mooncake, SGLang); llama.cpp has none"},
	{Name: "tiered-prefix-cache", Title: "Tiered Prefix Cache",
		WellLit: wellLit + "foundations/tiered-prefix-cache.md",
		Reason:  "needs vLLM's KV offloading tiers"},
	{Name: "wide-expert-parallelism", Title: "Wide Expert Parallelism",
		WellLit: wellLit + "foundations/wide-expert-parallelism.md",
		Reason:  "needs vLLM expert parallelism across GPUs"},
	{Name: "predicted-latency", Title: "Route by Predicted Latency",
		WellLit: wellLit + "foundations/predicted-latency.md",
		// The EPP binary ModelFabric ships already carries this path's plugins
		// (predicted-latency-producer, latency-scorer, latency-slo-admitter and
		// the predictor client). What is missing is ours to run: llm-d's
		// latency-predictor, a Python service (FastAPI + XGBoost, no pip
		// package) that trains on live traffic — a training server and a
		// prediction server ModelFabric would have to supervise, as it does Envoy.
		//
		// Its KV-cache feature is no longer an obstacle: two of the model's six
		// inputs are KV-cache usage, which ModelFabric now publishes (see kvUsage).
		// What remains is that the predictor is documented as requiring one GPU
		// type per pool, and this mesh is deliberately mixed.
		Reason: "ModelFabric does not run llm-d's latency-predictor service yet; the predictor also assumes one GPU type per pool"},
	{Name: "flow-control", Title: "Flow Control and Fairness",
		WellLit: wellLit + "foundations/flow-control.md",
		Reason:  "runs on llama.cpp; not yet offered by ModelFabric"},
	{Name: "multi-model-routing", Title: "Multi-Model Routing",
		WellLit: wellLit + "foundations/multi-model-routing.md",
		Reason:  "ModelFabric runs llm-d for one model at a time for now"},
}

// kvUsage names a metric llama.cpp does not publish: measured on b11026 and
// b11040, its whole set is the token and request counters plus
// n_busy_slots_per_decode. ModelFabric computes utilization from /slots and
// republishes it on the port llm-d scrapes (internal/engineshim), which is why
// the spec can name it at all.
//
// It is read, not ranked on. llm-d has a kv-cache-utilization-scorer and the
// utilization filter takes a kv-cache-utilization condition — both load
// cleanly against this gauge — but on llama.cpp a full cache means warm, not
// busy: the tokens counted are the prefixes kept for reuse. Preferring the
// emptier cache would steer away from exactly the engine prefix affinity wants,
// so neither is enabled until a benchmark says otherwise.

// kvScorerRef adds the KV scorer to a profile's plugin list when asked.
func kvScorerRef(o EPPOptions) string {
	if o.KVScorer <= 0 {
		return ""
	}
	return fmt.Sprintf("      - pluginRef: kv-scorer\n        weight: %d\n", o.KVScorer)
}

// roomFilter keeps only engines with a free slot, ahead of every other
// scheduling decision, so prefix affinity can never pick an engine where the
// request would queue while another has room. On llama.cpp a queued request
// waits for a slot and then evicts another conversation from the shared KV
// pool; with long agent conversations that turned into minutes-long
// re-prefill on one GPU while the other idled (the 2026-09-19 SWE-bench runs).
//
// Two conditions, both must hold:
//   - waiting-queue <= 0: the engine's own count of deferred requests
//     (llamacpp:requests_deferred). Correct for any slot count, so it holds on
//     a mixed fleet, but it only trips once a request has queued.
//   - active-requests <= slots-1: the EPP's own count of what it has
//     dispatched, so it trips before anything queues. It is one cap for the
//     pool, so it is used only when every engine has the same slot count.
//
// fallbackOnEmpty keeps routing when every engine is full; then the usual
// scoring picks among them.
func roomFilter(slots int, o EPPOptions) string {
	var b strings.Builder
	b.WriteString(`  - name: room-filter
    type: utilization-filter
    parameters:
      fallbackOnEmpty: true
      conditions:
`)
	if o.RoomFilter {
		b.WriteString(`        - metric: waiting-queue
          maxValue: 0
`)
	}
	if o.RoomFilter && slots > 0 {
		fmt.Fprintf(&b, `        - metric: active-requests
          maxValue: %d
`, slots-1)
	}
	if o.KVCeiling > 0 {
		fmt.Fprintf(&b, `        - metric: kv-cache-utilization
          maxValue: %.3f
`, o.KVCeiling)
	}
	if o.RoomFilter && slots > 0 {
		b.WriteString("      inFlightLoadProducerName: inflight-load-producer\n")
	}
	return b.String()
}

// visionProfile is the plugins that make a request carrying an image reach only
// an engine that can read one.
//
// An engine loaded without its projector answers to a multimodal model's name
// and fails the whole request inside llama.cpp with "failed to process mtmd
// chunk". Which machine is idle must not decide whether an image can be read,
// so this is a filter and not a scorer: a scorer would only prefer, and under
// load it would still hand the image to an engine that cannot serve it.
//
// It cannot be unconditional, or a text request would be confined to the
// engines that happen to hold a projector while the others idled. So llm-d
// picks the profile from a header ModelFabric sets, and the vision profile is the
// default profile's plugins with the filter in front — identical scheduling,
// minus the engines that would fail.
//
// Measured against EPP v0.10.0 with two stub engines, one labelled
// modelfabric.sh/vision=false and one =true (2026-09-25):
//
//   - x-fabric-profile: vision  → the vision-capable engine, 8 requests of 8.
//   - no header                → either engine, unfiltered, as before.
//   - a profile name that does not exist → every request fails with
//     "ResourceExhausted - failed to find target endpoint". So ModelFabric sends
//     only names it generated here, and defaultProfile covers the absent case:
//     without it, a request with no header failed the same way.
func visionProfile() string {
	return fmt.Sprintf(`  - name: vision-filter
    type: label-selector-filter
    parameters:
      matchLabels:
        %s: "true"
  - name: profile-by-header
    type: header-profile-handler
    parameters:
      headerName: %s
      defaultProfile: default
`, VisionLabel, ProfileHeader)
}

// profiles renders the default profile and the vision profile from one plugin
// list, so the two can never drift: a request carrying an image is scheduled
// exactly like any other, minus the engines that cannot serve it.
func profiles(refs string) string {
	return fmt.Sprintf(`
schedulingProfiles:
  - name: default
    plugins:
%s  - name: %s
    plugins:
      - pluginRef: vision-filter
%s`, refs, VisionProfile, refs)
}

// ProfileByName finds an available profile.
func ProfileByName(name string) (ProfileInfo, bool) {
	for _, p := range Profiles {
		if p.Name == name && p.Available {
			return p, true
		}
	}
	return ProfileInfo{}, false
}

// EPPConfig renders an EndpointPickerConfig for llama.cpp endpoints discovered
// from a ModelFabric endpoints file, for the chosen profile.
func EPPConfig(o EPPOptions) []byte {
	var unknownProfile strings.Builder
	// An unknown name falls back to the default, and says so where it can be
	// seen: the API validates the profile before it gets here, so reaching
	// this means a caller skipped that — and silently generating a different
	// scheduler than the one asked for is the worst way to find out.
	profile := o.Profile
	if _, ok := ProfileByName(profile); !ok {
		if profile != "" {
			fmt.Fprintf(&unknownProfile, "# ModelFabric: profile %q is not one ModelFabric offers; using %s\n", profile, ProfileLoadAware)
		}
		profile = ProfileLoadAware
	}
	peak := o.PeakPrefillThroughput
	if peak <= 0 {
		peak = DefaultPeakPrefill
	}
	var b strings.Builder
	b.WriteString(unknownProfile.String())
	fmt.Fprintf(&b, `# Generated by ModelFabric. EndpointPickerConfig for llama.cpp endpoints discovered
# from a ModelFabric mesh, without Kubernetes. Profile: %s.
apiVersion: llm-d.ai/v1alpha1
kind: EndpointPickerConfig
plugins:
  - name: mfsh-discovery
    type: file-discovery
    parameters:
`, profile)
	fmt.Fprintf(&b, "      path: %q\n", o.EndpointsPath)
	b.WriteString(`      watchFile: true

  - name: mfsh-metrics
    type: metrics-data-source
    parameters:
      scheme: http
      path: /metrics
  - name: mfsh-extractor
    type: core-metrics-extractor
    parameters:
      defaultEngine: llamacpp
      engineConfigs:
        - name: llamacpp
          queuedRequestsSpec: "llamacpp:requests_deferred"
          runningRequestsSpec: "llamacpp:requests_processing"
          kvUsageSpec: "` + engineshim.KVUsageMetric + `"
          loraSpec: ""
          cacheInfoSpec: ""

`)
	room := ""
	if o.RoomFilter || o.KVCeiling > 0 {
		b.WriteString(roomFilter(o.Slots, o))
		room = "      - pluginRef: room-filter\n"
	}
	if o.KVScorer > 0 {
		b.WriteString(`  - name: kv-scorer
    type: kv-cache-utilization-scorer
`)
	}
	switch profile {
	case ProfileOptimizedBaseline, ProfileTuned:
		// llm-d's Optimized Baseline (guides/optimized-baseline): prefix
		// and load are both tracked by the EPP itself, so it needs nothing
		// from the engine beyond reachability. Plugin names equal their
		// types, which is what the producer-name defaults expect.
		b.WriteString(`  - name: approx-prefix-cache-producer
    type: approx-prefix-cache-producer
  - name: inflight-load-producer
    type: inflight-load-producer
  - name: prefix-cache-affinity-filter
    type: prefix-cache-affinity-filter
    parameters:
`)
		fmt.Fprintf(&b, "      peakPrefillThroughput: %.0f\n", peak)
		b.WriteString(`  - name: token-load-scorer
    type: token-load-scorer
`)
		if profile == ProfileTuned {
			// Requests sharing a prompt that arrive within the window are
			// placed together. minColocateBlocks lets requests that share a
			// long prefix but differ at the end (a document plus different
			// questions) count, not only identical prompts: 8 blocks of 64
			// tokens = 512 shared tokens before co-locating.
			b.WriteString(`  - name: burst-prefix-cache-producer
    type: burst-prefix-cache-producer
    parameters:
      windowDurationMs: 50
      minColocateBlocks: 8
  - name: burst-prefix-scorer
    type: prefix-cache-scorer
    parameters:
      prefixMatchInfoProducerName: burst-prefix-cache-producer
  - name: no-hit-lru-scorer
    type: no-hit-lru-scorer
    parameters:
      prefixMatchInfoProducerName: approx-prefix-cache-producer
`)
		}
		b.WriteString(`  - name: picker
    type: max-score-picker
` + visionProfile())
		refs := room + `      - pluginRef: prefix-cache-affinity-filter
      - pluginRef: token-load-scorer
        weight: 2
`
		if profile == ProfileTuned {
			refs += `      - pluginRef: burst-prefix-scorer
        weight: 3
      - pluginRef: no-hit-lru-scorer
        weight: 1
`
		}
		refs += kvScorerRef(o) + `      - pluginRef: picker
`
		b.WriteString(profiles(refs))
	default:
		if o.RoomFilter && o.Slots > 0 {
			// The room filter's in-flight cap reads this producer's counts.
			b.WriteString(`  - name: inflight-load-producer
    type: inflight-load-producer
`)
		}
		b.WriteString(`  - name: queue
    type: queue-scorer
  - name: running
    type: running-requests-size-scorer
`)
		if o.PrefixCache {
			b.WriteString(`  - name: prefix
    type: prefix-cache-scorer
`)
		}
		b.WriteString(`  - name: picker
    type: max-score-picker
` + visionProfile())
		refs := room + `      - pluginRef: queue
        weight: 2
      - pluginRef: running
        weight: 1
`
		if o.PrefixCache {
			refs += `      - pluginRef: prefix
        weight: 3
`
		}
		refs += kvScorerRef(o) + `      - pluginRef: picker
`
		b.WriteString(profiles(refs))
	}
	b.WriteString(`
dataLayer:
  # Without this the EPP also injects its default vLLM-mapped metrics source,
  # scraping every endpoint a second time and logging an extraction error per
  # scrape. Its "skip if a metrics-data-source is listed" check matches the
  # plugin name, so a renamed source does not count.
  injectDefaults: false
  discovery:
    pluginRef: mfsh-discovery
  sources:
    - pluginRef: mfsh-metrics
      extractors:
        - pluginRef: mfsh-extractor
`)
	return []byte(b.String())
}

// EnvoyOptions shapes the generated Envoy config.
type EnvoyOptions struct {
	// ListenAddress is where Envoy accepts requests; loopback when empty.
	// Engines behind it take no key, so wider exposure is a deliberate act.
	ListenAddress string
	ListenPort    int // user-facing port
	EPPPort       int // EPP gRPC ext_proc port
	AdminPort     int
}

// EnvoyConfig renders the Envoy bootstrap that puts the EPP in the request path.
// It assumes the EPP runs with its default --secure-serving=true:
// ext_proc asks the EPP to pick, and an ORIGINAL_DST cluster forwards to the
// endpoint it names in x-gateway-destination-endpoint.
// Defaults for the llm-d data path, used when a caller passes a port that is
// not one.
const (
	defaultEnvoyListen = 8080
	defaultEnvoyAdmin  = 19000
	defaultEPPPort     = 9002
)

// portOr returns p when it is a usable TCP port, else fallback.
func portOr(p, fallback int) int {
	if p < 1 || p > 65535 {
		return fallback
	}
	return p
}

func EnvoyConfig(o EnvoyOptions) []byte {
	// These land in port_value fields, where an out-of-range number produces a
	// bootstrap Envoy refuses — surfacing far from the setting that caused it.
	// The node calls this, so a bad value falls back to the default rather
	// than taking the process down; `mfsh llmd init` rejects one up front.
	o.ListenPort = portOr(o.ListenPort, defaultEnvoyListen)
	o.AdminPort = portOr(o.AdminPort, defaultEnvoyAdmin)
	o.EPPPort = portOr(o.EPPPort, defaultEPPPort)
	listen := o.ListenAddress
	if listen == "" {
		listen = "127.0.0.1"
	}
	return []byte(fmt.Sprintf(`# Generated by ModelFabric (mfsh llmd init).
admin:
  address:
    socket_address: {address: 127.0.0.1, port_value: %d}

static_resources:
  listeners:
    - name: inference
      address:
        socket_address: {address: %s, port_value: %d}
      filter_chains:
        - filters:
            - name: envoy.filters.network.http_connection_manager
              typed_config:
                "@type": type.googleapis.com/envoy.extensions.filters.network.http_connection_manager.v3.HttpConnectionManager
                stat_prefix: inference
                route_config:
                  name: inference
                  virtual_hosts:
                    - name: inference
                      domains: ["*"]
                      routes:
                        - match: {prefix: "/"}
                          route:
                            cluster: original_destination_cluster
                            timeout: 600s
                            idle_timeout: 600s
                          # The address Envoy dialled, back to ModelFabric's
                          # front door, which maps it to a node and an engine.
                          # Without it a request the EPP placed was recorded
                          # with no serving node at all. Overwritten, so an
                          # engine cannot claim to be another.
                          response_headers_to_add:
                            - header:
                                key: x-fabric-upstream
                                value: "%%UPSTREAM_REMOTE_ADDRESS%%"
                              append_action: OVERWRITE_IF_EXISTS_OR_ADD
                http_filters:
                  # The ORIGINAL_DST cluster below takes its destination from
                  # x-gateway-destination-endpoint. That header is the EPP's to
                  # set, so a client-supplied one is removed here, before
                  # ext_proc runs — otherwise a caller could name the address
                  # Envoy forwards to.
                  - name: envoy.filters.http.header_mutation
                    typed_config:
                      "@type": type.googleapis.com/envoy.extensions.filters.http.header_mutation.v3.HeaderMutation
                      mutations:
                        request_mutations:
                          - remove: x-gateway-destination-endpoint
                  - name: envoy.filters.http.ext_proc
                    typed_config:
                      "@type": type.googleapis.com/envoy.extensions.filters.http.ext_proc.v3.ExternalProcessor
                      grpc_service:
                        envoy_grpc: {cluster_name: epp, authority: "localhost:%d"}
                        timeout: 10s
                      processing_mode:
                        request_header_mode: SEND
                        response_header_mode: SEND
                        request_body_mode: FULL_DUPLEX_STREAMED
                        response_body_mode: FULL_DUPLEX_STREAMED
                        request_trailer_mode: SEND
                        response_trailer_mode: SEND
                      message_timeout: 600s
                  - name: envoy.filters.http.router
                    typed_config:
                      "@type": type.googleapis.com/envoy.extensions.filters.http.router.v3.Router

  clusters:
    - name: epp
      type: STATIC
      connect_timeout: 86400s
      lb_policy: LEAST_REQUEST
      # The EPP serves ext_proc over TLS by default (--secure-serving, with a
      # self-signed certificate). Speaking plaintext here — as llm-d's own
      # no-Kubernetes example does — makes the handshake fail and every request
      # 500 with nothing in the EPP's log. Encryption without verification is
      # the honest match for a self-signed cert; supply --cert-path and a
      # validation_context to also authenticate the EPP.
      transport_socket:
        name: envoy.transport_sockets.tls
        typed_config:
          "@type": type.googleapis.com/envoy.extensions.transport_sockets.tls.v3.UpstreamTlsContext
          common_tls_context:
            alpn_protocols: ["h2"]
      typed_extension_protocol_options:
        envoy.extensions.upstreams.http.v3.HttpProtocolOptions:
          "@type": type.googleapis.com/envoy.extensions.upstreams.http.v3.HttpProtocolOptions
          explicit_http_config:
            http2_protocol_options: {}
      load_assignment:
        cluster_name: epp
        endpoints:
          - lb_endpoints:
              - endpoint:
                  address:
                    socket_address: {address: 127.0.0.1, port_value: %d}

    - name: original_destination_cluster
      type: ORIGINAL_DST
      connect_timeout: 600s
      lb_policy: CLUSTER_PROVIDED
      circuit_breakers:
        thresholds:
          - {max_connections: 10000, max_pending_requests: 10000, max_requests: 10000}
      original_dst_lb_config:
        use_http_header: true
        http_header_name: x-gateway-destination-endpoint
`, o.AdminPort, listen, o.ListenPort, o.EPPPort, o.EPPPort))
}
