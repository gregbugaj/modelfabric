// Package mesh discovers local engines and peer nodes through Tailscale and
// HTTP probes. A successful /z/state response identifies a mesh peer.
package mesh

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net"
	"net/http"
	"runtime"
	"slices"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gregbugaj/modelfabric/internal/config"
	"github.com/gregbugaj/modelfabric/internal/engineshim"
	"github.com/gregbugaj/modelfabric/internal/osproc"
	engines "github.com/gregbugaj/modelfabric/internal/runtime"
	"github.com/gregbugaj/modelfabric/internal/tscli"
)

// NodeState is what a node publishes to its peers at /z/state.
type NodeState struct {
	Node string `json:"node"`
	Addr string `json:"addr,omitempty"`
	// Platform is the node's OS/architecture (linux/amd64, darwin/arm64).
	// Empty on older peers.
	Platform string `json:"platform,omitempty"`
	// OSVersion identifies the OS release, which Tailscale's node list omits.
	OSVersion string   `json:"os_version,omitempty"`
	Models    []string `json:"models"`
	Inflight  int64    `json:"inflight"`
	// Accepted counts requests held by the front door, including those queued
	// in Envoy or dispatched by llm-d that engine counters may not expose.
	Accepted int64 `json:"accepted,omitempty"`
	// Queued is how many requests this node's router is holding for a slot
	// (router.Queue), zero when it has no queue. They are counted in Accepted
	// and in no engine's load: no engine has been chosen for them yet.
	Queued int64 `json:"queued,omitempty"`
	// Held is those requests one by one, and Routed what this node's router
	// has sent to each node since it started. Both describe the router on the
	// node that took the requests, which is not the node that served them.
	Held   []HeldRequest `json:"held,omitempty"`
	Routed []RoutedTo    `json:"routed,omitempty"`
	// MemTotalMB and MemAvailableMB are the machine's RAM and how much of it
	// is free to use without swapping, zero when the node cannot say.
	MemTotalMB     int64 `json:"mem_total_mb,omitempty"`
	MemAvailableMB int64 `json:"mem_available_mb,omitempty"`
	// Scheduler describes llm-d running on this node. Each peer advertises its
	// own scheduler, which may run separately from the engines.
	Scheduler *SchedulerState `json:"scheduler,omitempty"`
	Engines   []EngineState   `json:"engines"`
	Instances []InstanceState `json:"instances,omitempty"`
	Updated   time.Time       `json:"updated"`
}

type SchedulerState struct {
	Model   string `json:"model"`
	Profile string `json:"profile"`
	Engines int    `json:"engines"`
}

func Platform() string { return runtime.GOOS + "/" + runtime.GOARCH }

type EngineState struct {
	Name    string   `json:"name"`
	Healthy bool     `json:"healthy"`
	Models  []string `json:"models"`
	EngineStats
	// Slots is how many requests the engine runs at once (llama.cpp's
	// --parallel); zero when unknown. A request beyond it queues.
	Slots int64  `json:"slots,omitempty"`
	Error string `json:"error,omitempty"`
}

// EngineStats is the common set of measurements advertised for an engine and
// its supervised instance. Embedding keeps the peer protocol's flat JSON keys.
type EngineStats struct {
	Inflight int64 `json:"inflight"`
	// ContextLength is per-conversation capacity; KVPoolTokens is total engine
	// capacity across slots. Zero means unknown, not unlimited.
	ContextLength int    `json:"context_length,omitempty"`
	KVPoolTokens  int    `json:"kv_pool_tokens,omitempty"`
	ContextNote   string `json:"context_note,omitempty"`
	// PrefillTokS is this engine's measured prompt-processing rate; zero until
	// it has served enough prompt tokens to measure. Engines differ widely
	// across a mixed fleet, so it is per engine, never pooled.
	PrefillTokS float64 `json:"prefill_tok_s,omitempty"`
	// DecodeTokS is generated tokens per second. SpecAccepted is the retained
	// draft fraction, or -1 when not speculating.
	DecodeTokS   float64 `json:"decode_tok_s,omitempty"`
	SpecAccepted float64 `json:"spec_accepted,omitempty"`
	// PrefillTrusted permits placement using the rate; preliminary rates are
	// displayed but excluded from scheduling.
	PrefillTrusted bool `json:"prefill_trusted,omitempty"`
	// PromptTokens, CachedTokens and OutputTokens are lifetime engine totals.
	PromptTokens int64 `json:"prompt_tokens,omitempty"`
	CachedTokens int64 `json:"cached_tokens,omitempty"`
	OutputTokens int64 `json:"output_tokens,omitempty"`
	// KVUsage is the share of this engine's KV pool in use, -1 when the engine
	// cannot be asked. See internal/engineshim for what it counts.
	KVUsage float64 `json:"kv_usage"`
	// LoadAvg is mean requests in flight over LoadWindow, or -1 before enough samples.
	LoadAvg float64 `json:"load_avg"`
}

type Engine struct {
	Name    string
	BaseURL string
	// via, when set, is where the router sends requests instead of BaseURL:
	// the engine's shim, which holds the disk prompt cache. BaseURL stays the
	// engine itself, for health polls and for measuring it (tune, bench),
	// which must not go through a cache.
	via atomic.Pointer[string]
	// Served is the model id this engine answers to, when it differs from the
	// catalog key: mlx-lm dispatches on the request's "model" field and only
	// recognises its own name for the model it was started with. Empty means
	// the engine answers to the catalog key, as llama.cpp does via --alias.
	Served string
	// FixedModels and NoMetrics mirror the engine's runtime traits: what this
	// engine cannot be asked. The zero values describe llama.cpp, so an engine
	// registered without them behaves exactly as before. KVFromSlots says its
	// KV utilization must be computed from /slots rather than read from a
	// gauge.
	FixedModels bool
	NoMetrics   bool
	KVFromSlots bool
	// NoConstrainedDecoding says this engine ignores response_format and
	// grammar instead of honouring them (mlx-lm). The router keeps requests
	// that ask for constrained output away from it.
	NoConstrainedDecoding bool
	// NoVision excludes image requests from an instance loaded without its
	// available projector.
	NoVision bool

	// Embedding says this engine was started to embed (--embeddings): it
	// serves /v1/embeddings and fails anything that generates text. Known for
	// this node's own engines; a peer applies the same rule to its own.
	Embedding bool

	mu      sync.RWMutex
	models  []string
	healthy bool
	lastErr string

	inflight atomic.Int64
	// lastUsed is when a request last started or finished here, in unix
	// nanoseconds; zero means never. Idle TTLs are measured from it.
	lastUsed atomic.Int64
	// slots is the engine's concurrent request capacity; zero is unknown.
	slots atomic.Int64
	// prefill is the measured prompt tokens per second (a float64's bits).
	prefill atomic.Uint64
	// decode stores generated tokens per second; specAccepted stores the
	// retained draft fraction as float64 bits.
	decode       atomic.Uint64
	specAccepted atomic.Uint64
	// askedContext is requested per-slot capacity; enginePool is reported total
	// capacity. contextLen reconciles them, and contextNote records mismatches.
	// Zero means unknown.
	askedContext atomic.Int64
	enginePool   atomic.Int64
	contextLen   atomic.Int64
	contextNote  atomic.Value // string
	// propsAsked stops the /props probe repeating: an engine's context cannot
	// change without a reload, and a reload makes a new Engine.
	propsAsked atomic.Bool

	prefillTrusted atomic.Bool
	promptTokens   atomic.Int64
	cachedTokens   atomic.Int64
	outputTokens   atomic.Int64
	// others counts engine-observed requests beyond this router's dispatches,
	// including llm-d traffic. See Inflight.
	others atomic.Int64
	// load is a short history of in-flight samples, for the rolling average.
	// Guarded by its own mutex: it is written from the poll loop and read by
	// every state render, and neither should wait on the model list.
	loadMu sync.Mutex
	load   []loadSample
	// kvUsage is the last measured KV-cache utilization (a float64's bits),
	// valid only once kvKnown is set: an idle engine measures a true zero,
	// which must not read the same as never having been measured.
	kvUsage atomic.Uint64
	kvKnown atomic.Bool
}

func (e *Engine) SetSlots(n int) { e.slots.Store(int64(n)) }

// SetAskedContext records the per-request context ModelFabric loaded this engine
// with. It is what was asked for, not necessarily what the engine did.
func (e *Engine) SetAskedContext(tokens int) {
	e.askedContext.Store(int64(tokens))
	e.reconcile()
}

func (e *Engine) SetEnginePool(tokens int) {
	e.enginePool.Store(int64(tokens))
	e.reconcile()
}

func (e *Engine) reconcile() {
	perRequest, note := reconcileContext(
		int(e.askedContext.Load()), int(e.slots.Load()), int(e.enginePool.Load()))
	e.contextLen.Store(int64(perRequest))
	e.contextNote.Store(note)
}

// Context returns per-request capacity, total KV pool, and any mismatch note.
// Zero means unknown, not unlimited.
func (e *Engine) Context() (perRequest, pool int, note string) {
	n, _ := e.contextNote.Load().(string)
	return int(e.contextLen.Load()), int(e.enginePool.Load()), n
}

func (e *Engine) Slots() int64 { return e.slots.Load() }

// PrefillRate reports the measured prompt tokens per second; zero until the
// engine has served enough to measure at all. It may be rough; see
// TrustedPrefillRate for the one placement uses.
func (e *Engine) PrefillRate() float64 { return math.Float64frombits(e.prefill.Load()) }

// TrustedPrefillRate returns zero until enough prompt tokens establish a rate.
// Routing and llm-d endpoint labels use this value.
func (e *Engine) TrustedPrefillRate() float64 {
	if !e.prefillTrusted.Load() {
		return 0
	}
	return e.PrefillRate()
}

func (e *Engine) PrefillTrusted() bool { return e.prefillTrusted.Load() }

// DecodeRate reports the measured generated tokens per second; zero until the
// engine has generated anything.
func (e *Engine) DecodeRate() float64 { return math.Float64frombits(e.decode.Load()) }

// SpecAccepted reports the share of drafted tokens the model kept, 0..1, or -1
// when the engine is not speculating or has not been measured.
func (e *Engine) SpecAccepted() float64 {
	v := e.specAccepted.Load()
	if v == 0 {
		return -1
	}
	return math.Float64frombits(v)
}

func (e *Engine) Tokens() (prompt, cached, output int64) {
	return e.promptTokens.Load(), e.cachedTokens.Load(), e.outputTokens.Load()
}

func (e *Engine) SetRates(r EngineRates) {
	e.prefill.Store(math.Float64bits(r.PrefillTokS))
	e.prefillTrusted.Store(r.Trusted)
	e.decode.Store(math.Float64bits(r.DecodeTokS))
	if r.SpecAccepted >= 0 {
		e.specAccepted.Store(math.Float64bits(r.SpecAccepted))
	}
	e.promptTokens.Store(r.PromptTokens)
	e.cachedTokens.Store(r.CachedTokens)
	e.outputTokens.Store(r.OutputTokens)
}

func (e *Engine) SetKVUsage(v float64) {
	e.kvUsage.Store(math.Float64bits(v))
	e.kvKnown.Store(true)
}

func (e *Engine) KVUsage() (float64, bool) {
	if !e.kvKnown.Load() {
		return 0, false
	}
	return math.Float64frombits(e.kvUsage.Load()), true
}

type loadSample struct {
	at time.Time
	n  int64
}

// LoadWindow smooths bursts over roughly 60 samples at the default poll interval.
// The node retains the average independently of dashboard sessions.
const LoadWindow = 2 * time.Minute

func (e *Engine) SetObservedInflight(n int) {
	e.setObserved(n, e.inflight.Load())
}

// setObserved is SetObservedInflight given the router's own count from before
// the engine was asked. The engine's answer describes a moment between that
// count and the one read now, and a request that started or ended in the gap
// is in one of the two: taking the larger keeps it from being counted as
// someone else's.
func (e *Engine) setObserved(n int, ownBefore int64) {
	e.others.Store(max(int64(n)-max(ownBefore, e.inflight.Load()), 0))
	e.recordLoad(int64(n))
}

func (e *Engine) recordLoad(n int64) {
	now := time.Now()
	e.loadMu.Lock()
	defer e.loadMu.Unlock()
	e.load = append(e.load, loadSample{at: now, n: n})
	cut := 0
	for cut < len(e.load) && now.Sub(e.load[cut].at) > LoadWindow {
		cut++
	}
	// Re-slice onto the front rather than keep a growing backing array: this
	// appends every poll for as long as the node runs.
	if cut > 0 {
		e.load = append(e.load[:0], e.load[cut:]...)
	}
}

// LoadAvg returns mean in-flight requests over LoadWindow and whether there
// are enough samples to report it.
func (e *Engine) LoadAvg() (float64, bool) {
	e.loadMu.Lock()
	defer e.loadMu.Unlock()
	if len(e.load) < 3 {
		return 0, false
	}
	var sum int64
	for _, s := range e.load {
		sum += s.n
	}
	return float64(sum) / float64(len(e.load)), true
}

// Inflight combines live local dispatch counts, including queued requests,
// with externally submitted work observed at the engine. Only the excess over
// local counts contributes from polls; using the full polled count would retain
// completed local requests until the next poll and falsely mark slots occupied.
func (e *Engine) Inflight() int64 {
	return e.inflight.Load() + e.others.Load()
}

func (e *Engine) kvPool() int64 {
	_, pool, _ := e.Context()
	return int64(pool)
}

func (e *Engine) LastUsed() time.Time {
	if n := e.lastUsed.Load(); n != 0 {
		return time.Unix(0, n)
	}
	return time.Time{}
}

func (e *Engine) snapshot() EngineState {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return EngineState{
		Name:        e.Name,
		Healthy:     e.healthy,
		Models:      append([]string(nil), e.models...),
		EngineStats: e.Stats(),
		Slots:       e.slots.Load(),
		Error:       e.lastErr,
	}
}

// Stats reads the same measurements for local engine and supervised-instance
// reports, including the sentinels that distinguish unknown from idle.
func (e *Engine) Stats() EngineStats {
	prompt, cached, output := e.Tokens()
	perRequest, pool, note := e.Context()
	return EngineStats{
		// The engine's observed count includes llm-d traffic that bypasses the
		// router. Using its raw counter made the local Serving page read 0/2
		// while peers correctly showed the same engine as busy.
		Inflight:       e.Inflight(),
		ContextLength:  perRequest,
		KVPoolTokens:   pool,
		ContextNote:    note,
		PrefillTokS:    e.PrefillRate(),
		DecodeTokS:     e.DecodeRate(),
		SpecAccepted:   e.SpecAccepted(),
		PrefillTrusted: e.PrefillTrusted(),
		PromptTokens:   prompt,
		CachedTokens:   cached,
		OutputTokens:   output,
		KVUsage:        kvOrUnknown(e),
		LoadAvg:        loadOrUnknown(e),
	}
}

// loadOrUnknown reports the engine's rolling load, or -1 when it has not been
// sampled enough; zero would read as "idle for two minutes".
func loadOrUnknown(e *Engine) float64 {
	if v, ok := e.LoadAvg(); ok {
		return v
	}
	return -1
}

// kvOrUnknown returns KV utilization, or -1 when unmeasured; zero means idle.
func kvOrUnknown(e *Engine) float64 {
	if v, ok := e.KVUsage(); ok {
		return v
	}
	return -1
}

func (e *Engine) has(model string) bool {
	e.mu.RLock()
	defer e.mu.RUnlock()
	if !e.healthy {
		return false
	}
	for _, m := range e.models {
		if m == model {
			return true
		}
	}
	return false
}

type HeldRequest struct {
	WaitedMs int64 `json:"waited_ms"`
	// Home is the engine holding its conversation, empty for a new one.
	Home string `json:"home,omitempty"`
	Why  string `json:"why,omitempty"`
}

// RoutedTo is what a node's router has sent to one node: requests, and how
// many of them were a conversation arriving from a different engine.
type RoutedTo struct {
	Node  string `json:"node"`
	Calls int64  `json:"calls"`
	Moved int64  `json:"moved"`
}

type Peer struct {
	Node    string
	Addr    string
	BaseURL string

	mu        sync.RWMutex
	platform  string
	osVersion string
	models    []string
	instances []InstanceState
	engines   []EngineState
	reported  int64
	// accepted and scheduler are what the peer said about itself: what its
	// front door is holding, and whether it is scheduling for the mesh.
	accepted  int64
	queued    int64
	held      []HeldRequest
	routed    []RoutedTo
	memTotal  int64
	memAvail  int64
	scheduler *SchedulerState
	lastSeen  time.Time
	alive     bool

	// sent counts this node's outstanding requests to the peer. Decrement on
	// completion so follow-up turns do not see their own finished request as
	// occupying a slot until the next poll.
	sent atomic.Int64
	// sentAtPoll is sent as it stood when reported was stored. What reported
	// holds beyond it came from somewhere else: another entry node, llm-d, a
	// client talking to the peer directly.
	sentAtPoll int64
}

// others is how much of a count the peer reported was not sent by this node.
// The caller holds p.mu.
func (p *Peer) others(reported int64) int64 {
	return max(reported-p.sentAtPoll, 0)
}

// identity returns the peer's name and base URL under its own lock, which is
// what refreshPeers takes when it updates them.
func (p *Peer) identity() (string, string) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.Node, p.BaseURL
}

func (p *Peer) has(model string) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if !p.alive {
		return false
	}
	for _, m := range p.models {
		if m == model {
			return true
		}
	}
	return false
}

// capacity returns model-specific in-flight load, total slots and fastest
// prefill rate. Load combines current local dispatches with reported external
// requests. Zero slots means unknown capacity.
func (p *Peer) capacity(model string) (inflight, slots int64, rate float64) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	for _, e := range p.engines {
		if !e.Healthy || !slices.Contains(e.Models, model) {
			continue
		}
		inflight += e.Inflight
		slots += e.Slots
		rate = max(rate, e.PrefillTokS)
	}
	return p.sent.Load() + p.others(inflight), slots, rate
}

// refusesImages reports whether every ready instance declined its projector.
// Such engines still serve the model name but cannot handle image requests.
func (p *Peer) refusesImages(model string) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	saw := false
	for _, i := range p.instances {
		if i.Model != model || i.State != "ready" {
			continue
		}
		saw = true
		if !i.VisionOff {
			return false
		}
	}
	// Without ready instance data, preserve image-routing compatibility with
	// older peers and defer filtering to their router.
	return saw
}

func (p *Peer) constrains(model string) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	saw := false
	for _, i := range p.instances {
		if i.Model != model || i.State != "ready" {
			continue
		}
		saw = true
		if i.Engine != EngineMLX {
			return true
		}
	}
	// Missing instance lists do not establish an engine capability restriction.
	return !saw
}

// pool totals model-specific KV capacity in tokens, or zero if any is unknown.
func (p *Peer) pool(model string) int64 {
	p.mu.RLock()
	defer p.mu.RUnlock()
	var total int64
	for _, e := range p.engines {
		if !e.Healthy || !slices.Contains(e.Models, model) {
			continue
		}
		if e.KVPoolTokens <= 0 {
			return 0
		}
		total += int64(e.KVPoolTokens)
	}
	return total
}

// load is the router's view of how busy a peer is: the requests this node has
// there right now, plus what the peer last reported beyond those.
func (p *Peer) load() int64 {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.sent.Load() + p.others(p.reported)
}

type PeerView struct {
	Node string `json:"node"`
	Addr string `json:"addr"`
	// BaseURL is the peer's mesh listener, which serves inference to other
	// nodes on the tailnet.
	BaseURL   string          `json:"base_url"`
	Platform  string          `json:"platform,omitempty"`
	OSVersion string          `json:"os_version,omitempty"`
	Instances []InstanceState `json:"instances,omitempty"`
	Alive     bool            `json:"alive"`
	Models    []string        `json:"models"`
	Inflight  int64           `json:"inflight"`
	// Accepted counts requests held by the front door, including those queued
	// in Envoy or dispatched by llm-d that engine counters may not expose.
	Accepted int64 `json:"accepted,omitempty"`
	// Queued is how many requests this node's router is holding for a slot
	// (router.Queue), zero when it has no queue. They are counted in Accepted
	// and in no engine's load: no engine has been chosen for them yet.
	Queued int64 `json:"queued,omitempty"`
	// Held is those requests one by one, and Routed what this node's router
	// has sent to each node since it started. Both describe the router on the
	// node that took the requests, which is not the node that served them.
	Held   []HeldRequest `json:"held,omitempty"`
	Routed []RoutedTo    `json:"routed,omitempty"`
	// MemTotalMB and MemAvailableMB are the machine's RAM and how much of it
	// is free to use without swapping, zero when the node cannot say.
	MemTotalMB     int64 `json:"mem_total_mb,omitempty"`
	MemAvailableMB int64 `json:"mem_available_mb,omitempty"`
	// Scheduler is llm-d on this peer, when it is scheduling for the mesh.
	// The node that schedules is not always the node you are asking: with an
	// entrypoint it has no GPUs at all.
	Scheduler *SchedulerState `json:"scheduler,omitempty"`
	LastSeen  time.Time       `json:"last_seen"`
}

// Engine families, as reported in InstanceState.Engine. An empty value is
// llama.cpp: it is what every node ran before the field existed.
const (
	EngineLlamaCPP = "llama.cpp"
	EngineMLX      = "mlx"
)

type InstanceState struct {
	ID      string `json:"id"`
	Model   string `json:"model"`
	Source  string `json:"source"`
	Runtime string `json:"runtime"`
	// Engine identifies the runtime family for external metrics and capacity
	// readers. Empty means llama.cpp for compatibility with older peers.
	Engine string `json:"engine,omitempty"`
	// ServedModel is the id this engine answers to when it is not Model, so
	// anything dialling it directly knows it must say that instead.
	ServedModel string `json:"served_model,omitempty"`
	// MetricsPort is where ModelFabric republishes this engine's metrics with a KV
	// gauge (internal/engineshim), for schedulers that scrape what they route
	// to; zero when the engine is scraped directly.
	MetricsPort int `json:"metrics_port,omitempty"`
	EngineStats
	GPU string `json:"gpu,omitempty"`
	// Address is where this instance is reachable from outside the node. It is
	// loopback unless engine_bind says otherwise, which matters to anything
	// scheduling across the mesh rather than through ModelFabric.
	Address string `json:"address"`
	Port    int    `json:"port"`
	// Slots and PrefillTokS describe the engine to schedulers outside ModelFabric
	// (they become llm-d endpoint labels); see EngineState.
	Slots int `json:"slots,omitempty"`
	// Vision says this instance takes images. It is published to llm-d as
	// an endpoint label, so a request carrying an image is scheduled only
	// onto an engine that can read one. False from a peer too old to say.
	Vision bool `json:"vision,omitempty"`
	// VisionOff marks a declined projector on an otherwise image-capable model;
	// models with no projector leave it false.
	VisionOff bool `json:"vision_off,omitempty"`
	// CacheRAMMiB caps the host-RAM prompt cache. CacheDropped counts evictions
	// since engine start; MemoryMB is current process RAM. Zero means unknown.
	CacheRAMMiB  int       `json:"cache_ram_mib,omitempty"`
	CacheDropped int64     `json:"cache_dropped,omitempty"`
	MemoryMB     int64     `json:"memory_mb,omitempty"`
	State        string    `json:"state"`
	PID          int       `json:"pid,omitempty"`
	Error        string    `json:"error,omitempty"`
	Started      time.Time `json:"started"`
}

type Mesh struct {
	cfg    config.Config
	node   string
	client *http.Client
	// infer sends generations; see New for why it is not client.
	infer *http.Client

	poll, probe, dead time.Duration

	// Engines change at runtime as the supervisor loads and unloads models, so
	// they get their own lock rather than riding the peer lock.
	engMu   sync.RWMutex
	engines []*Engine

	mu    sync.RWMutex
	peers map[string]*Peer // keyed by tailnet address

	// instances reports supervised instances for /z/state. Nil on a node that
	// only fronts externally-started engines.
	instances func() []InstanceState
	// accepted reports what the front door is holding; nil on a node whose
	// server has not attached one.
	accepted func() int64
	// queued reports what the router is holding for a slot; nil without one.
	queued     func() int64
	routerView func() ([]HeldRequest, []RoutedTo)
	// engineGone is told the name of a candidate whose engine has stopped or
	// been replaced: a local engine's name, or a peer's node. See
	// SetEngineGoneHook.
	engineGone func(name string)
	// hostMemory reports this machine's total and available memory in bytes;
	// the rest is its answer, kept a few seconds.
	hostMemory                 func() (total, available int64)
	hostMemMu                  sync.Mutex
	hostMemAt                  time.Time
	hostMemTotal, hostMemAvail int64
	// scheduler reports this node's llm-d, nil when it runs none.
	scheduler func() *SchedulerState

	// selfAddr is this node's own tailnet IPv4, advertised so peers can reach
	// instances directly rather than only through this node's proxy.
	selfAddr string

	prefMu    sync.RWMutex
	preferred string
}

func (m *Mesh) SetSelfAddr(addr string) { m.selfAddr = addr }

// SetPreferred names the node to resolve models to when several hold the same
// one. Empty means no preference.
func (m *Mesh) SetPreferred(node string) {
	m.prefMu.Lock()
	defer m.prefMu.Unlock()
	m.preferred = node
}

func (m *Mesh) Preferred() string {
	m.prefMu.RLock()
	defer m.prefMu.RUnlock()
	return m.preferred
}

// ProbeHeaderTimeout bounds how long a probe or measurement waits for a peer to
// start answering. It is read when a Mesh is made. A variable so a test can
// show, in milliseconds, that this bound does not apply to inference.
var ProbeHeaderTimeout = 120 * time.Second

func New(cfg config.Config, selfNode string) *Mesh {
	poll, probe, dead := cfg.Durations()
	m := &Mesh{
		cfg:   cfg,
		node:  selfNode,
		poll:  poll,
		probe: probe,
		dead:  dead,
		peers: map[string]*Peer{},
		// For probes and measurements, which answer at once or not at all: no
		// overall timeout, and a bound on the time to the first response header.
		client: &http.Client{
			Transport: &http.Transport{
				ResponseHeaderTimeout: ProbeHeaderTimeout,
				MaxIdleConnsPerHost:   64,
				IdleConnTimeout:       90 * time.Second,
			},
		},
		// Non-streaming engines may send headers only after generation completes.
		// Inference uses the router stall watchdog rather than the probe client's
		// header timeout; connection establishment remains bounded by the dial timeout.
		infer: &http.Client{
			Transport: &http.Transport{
				DialContext:         (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
				MaxIdleConnsPerHost: 64,
				IdleConnTimeout:     90 * time.Second,
			},
		},
	}
	for _, e := range cfg.Engines {
		m.engines = append(m.engines, &Engine{Name: e.Name, BaseURL: e.BaseURL})
	}
	return m
}

// rateWeighted reports whether candidates are compared by estimated time
// rather than queue depth. On unless explicitly disabled.
func (m *Mesh) rateWeighted() bool {
	return m.cfg.RateWeightedRouting == nil || *m.cfg.RateWeightedRouting
}

func (m *Mesh) Node() string         { return m.node }
func (m *Mesh) Client() *http.Client { return m.client }

// InferenceClient has no response-header timeout because non-streaming
// engines may send headers only after generation completes.
func (m *Mesh) InferenceClient() *http.Client { return m.infer }

// Engines returns a snapshot of the currently registered engines.
func (m *Mesh) Engines() []*Engine {
	m.engMu.RLock()
	defer m.engMu.RUnlock()
	return append([]*Engine(nil), m.engines...)
}

// RegisterEngine adds a routable engine, replacing any existing one with the
// same name. The supervisor calls this once an instance reports ready.
func (m *Mesh) RegisterEngine(e *Engine) {
	m.engMu.Lock()
	defer m.engMu.Unlock()
	for i, existing := range m.engines {
		if existing.Name == e.Name {
			m.engines[i] = e
			return
		}
	}
	m.engines = append(m.engines, e)
}

// UnregisterEngine removes an engine from routing. In-flight requests already
// dispatched to it are unaffected; they finish against the old handle.
func (m *Mesh) UnregisterEngine(name string) {
	m.engMu.Lock()
	found := false
	for i, e := range m.engines {
		if e.Name == name {
			m.engines = append(m.engines[:i], m.engines[i+1:]...)
			found = true
			break
		}
	}
	m.engMu.Unlock()
	if found && m.engineGone != nil {
		m.engineGone(name)
	}
}

// SetEngineGoneHook registers cache invalidation for stopped or replaced
// engines. It receives a local engine name or a peer node name when a reported
// instance disappears. Set it before polling starts.
func (m *Mesh) SetEngineGoneHook(f func(name string)) { m.engineGone = f }

func (m *Mesh) SetInstanceProvider(f func() []InstanceState) { m.instances = f }

func (m *Mesh) SetAcceptedProvider(f func() int64) { m.accepted = f }

func (m *Mesh) SetQueuedProvider(f func() int64) { m.queued = f }

func (m *Mesh) SetRouterViewProvider(f func() ([]HeldRequest, []RoutedTo)) { m.routerView = f }

func (m *Mesh) SetSchedulerProvider(f func() *SchedulerState) { m.scheduler = f }

func NewEngine(name, baseURL string) *Engine {
	return &Engine{Name: name, BaseURL: baseURL}
}

// SetVia routes requests for this engine through addr (its shim); "" sends
// them to the engine directly again.
func (e *Engine) SetVia(addr string) {
	if addr == "" {
		e.via.Store(nil)
		return
	}
	e.via.Store(&addr)
}

func (e *Engine) DispatchURL() string {
	if v := e.via.Load(); v != nil {
		return *v
	}
	return e.BaseURL
}

// MarkReady pre-seeds an engine as healthy with a known model, so it is
// routable immediately rather than after the next poll.
func (e *Engine) MarkReady(models ...string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.healthy, e.models, e.lastErr = true, models, ""
}

func (m *Mesh) Run(ctx context.Context) {
	m.refresh(ctx)
	t := time.NewTicker(m.poll)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			m.refresh(ctx)
		}
	}
}

func (m *Mesh) refresh(ctx context.Context) {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); m.refreshEngines(ctx) }()
	go func() { defer wg.Done(); m.refreshPeers(ctx) }()
	wg.Wait()
}

func (m *Mesh) refreshEngines(ctx context.Context) {
	var wg sync.WaitGroup
	// Over a snapshot: loading or unloading a model rewrites this slice while
	// the poll, a request and the peer view all walk it.
	for _, e := range m.Engines() {
		wg.Add(1)
		go func(e *Engine) {
			defer wg.Done()
			models, err := m.fetchModels(ctx, e.BaseURL)
			e.mu.Lock()
			defer e.mu.Unlock()
			if err != nil {
				e.healthy, e.lastErr = false, err.Error()
				e.models = nil
				return
			}
			e.healthy, e.lastErr = true, ""
			// An engine with a fixed model list has nothing to tell us here
			// beyond being alive: mlx-lm's /v1/models lists the Hugging Face
			// cache, not what it is serving, so what it was launched for stands.
			if !e.FixedModels {
				e.models = models
			}
			if e.KVFromSlots {
				// Read /slots for KV utilization and in-flight counts. Engine counts include
				// llm-d requests that bypass the router.
				ownBefore := e.inflight.Load()
				if slots, ok := m.fetchSlots(ctx, e.BaseURL); ok {
					if usage, ok := engineshim.KVUsage(slots); ok {
						e.SetKVUsage(usage)
					}
					if busy, ok := engineshim.BusySlots(slots); ok {
						e.setObserved(busy, ownBefore)
					}
				}
			}
			if e.NoMetrics {
				return
			}
			// Read context once per Engine; changing it requires a reload and new handle.
			if !e.propsAsked.Swap(true) {
				if pool, ok := m.fetchContext(ctx, e.BaseURL); ok {
					// Expose context mismatches to doctor and the dashboard so requests
					// are not sized against an ignored load setting.
					e.SetEnginePool(pool)
				}
			}
			if rates, ok := m.fetchRates(ctx, e.BaseURL); ok {
				e.SetRates(rates)
			}
		}(e)
	}
	wg.Wait()
}

type modelsResponse struct {
	Data []struct {
		ID string `json:"id"`
	} `json:"data"`
}

func (m *Mesh) fetchModels(ctx context.Context, base string) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, m.probe)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/v1/models", nil)
	if err != nil {
		return nil, err
	}
	resp, err := m.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET /v1/models: %s", resp.Status)
	}
	var mr modelsResponse
	if err := json.NewDecoder(resp.Body).Decode(&mr); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(mr.Data))
	for _, d := range mr.Data {
		if d.ID != "" {
			out = append(out, d.ID)
		}
	}
	sort.Strings(out)
	return out, nil
}

type tailscaleStatus struct {
	Self *tsNode            `json:"Self"`
	Peer map[string]*tsNode `json:"Peer"`
}

type tsNode struct {
	HostName     string   `json:"HostName"`
	DNSName      string   `json:"DNSName"`
	TailscaleIPs []string `json:"TailscaleIPs"`
	Online       bool     `json:"Online"`
	// Active reports a live Tailscale data-plane connection even when the
	// control plane reports Online=false.
	Active bool `json:"Active"`
	// LastSeen separates a peer whose control plane just dropped from one that
	// has been gone for days. Zero on a peer that is online now.
	LastSeen time.Time `json:"LastSeen"`
	Tags     []string  `json:"Tags"`
}

// reachable includes online, active, and recently seen peers. Tailscale's
// control-plane Online flag can be false while the data plane still serves
// requests; the HTTP probe determines actual availability.
func (n *tsNode) reachable(recently time.Duration) bool {
	return n.Online || n.Active || (!n.LastSeen.IsZero() && time.Since(n.LastSeen) < recently)
}

// peerGracePeriod is how long after tailscaled last saw a peer ModelFabric keeps
// probing it anyway.
const peerGracePeriod = 15 * time.Minute

func (n *tsNode) v4() string {
	for _, ip := range n.TailscaleIPs {
		// Tailscale lists IPv4 first; select it explicitly rather than by
		// position so an IPv6-only peer is skipped instead of mis-dialed.
		for i := 0; i < len(ip); i++ {
			if ip[i] == ':' {
				break
			}
			if ip[i] == '.' {
				return ip
			}
		}
	}
	return ""
}

func (n *tsNode) tagged(tag string) bool {
	if tag == "" {
		return true
	}
	for _, t := range n.Tags {
		if t == tag {
			return true
		}
	}
	return false
}

// SelfIdentity reports this machine's Tailscale hostname and IPv4 address.
func SelfIdentity(ctx context.Context) (name, addr string, err error) {
	st, err := queryTailscale(ctx)
	if err != nil {
		return "", "", err
	}
	if st.Self == nil || st.Self.HostName == "" {
		return "", "", fmt.Errorf("tailscale status: no self hostname")
	}
	return st.Self.HostName, st.Self.v4(), nil
}

func queryTailscale(ctx context.Context) (*tailscaleStatus, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := tscli.Run(ctx, "status", "--json")
	if err != nil {
		return nil, fmt.Errorf("tailscale status: %w", err)
	}
	var st tailscaleStatus
	if err := json.Unmarshal(out, &st); err != nil {
		return nil, fmt.Errorf("parse tailscale status: %w", err)
	}
	return &st, nil
}

func (m *Mesh) knownAddrs() map[string]bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make(map[string]bool, len(m.peers))
	for addr := range m.peers {
		out[addr] = true
	}
	return out
}

// refreshPeers probes every eligible tailnet peer and folds the responses into
// the peer table. Peers that stop answering for DeadAfter are dropped.
func (m *Mesh) refreshPeers(ctx context.Context) {
	st, err := queryTailscale(ctx)
	if err != nil {
		// Losing tailscaled should not wipe the mesh; let DeadAfter age peers out.
		m.expire()
		return
	}

	// A peer ModelFabric already knows is probed whatever tailscaled says; see
	// tsNode.reachable for the rest.
	known := m.knownAddrs()

	type cand struct{ node, addr string }
	var cands []cand
	for _, p := range st.Peer {
		if !p.tagged(m.cfg.Tag) {
			continue
		}
		if !p.reachable(peerGracePeriod) && !known[p.v4()] {
			continue
		}
		addr := p.v4()
		if addr == "" {
			continue
		}
		name := p.HostName
		if name == "" {
			name = addr
		}
		cands = append(cands, cand{node: name, addr: addr})
	}

	// Bound concurrent probes to avoid one open request per tailnet device.
	var wg sync.WaitGroup
	slots := make(chan struct{}, maxPeerProbes)
	for _, c := range cands {
		wg.Add(1)
		go func(c cand) {
			defer wg.Done()
			slots <- struct{}{}
			defer func() { <-slots }()
			base := fmt.Sprintf("http://%s:%d", c.addr, m.cfg.MeshPort)
			state, err := m.probePeer(ctx, base)
			if err != nil {
				return // not a mesh node, or momentarily unreachable
			}
			if state.Node == m.node {
				return
			}
			m.upsert(c.addr, c.node, base, state)
		}(c)
	}
	wg.Wait()
	m.expire()
}

const maxPeerProbes = 16

func (m *Mesh) probePeer(ctx context.Context, base string) (*NodeState, error) {
	ctx, cancel := context.WithTimeout(ctx, m.probe)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/z/state", nil)
	if err != nil {
		return nil, err
	}
	resp, err := m.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET /z/state: %s", resp.Status)
	}
	var s NodeState
	if err := json.NewDecoder(resp.Body).Decode(&s); err != nil {
		return nil, err
	}
	return &s, nil
}

func (m *Mesh) upsert(addr, node, base string, s *NodeState) {
	m.mu.Lock()
	p, ok := m.peers[addr]
	if !ok {
		p = &Peer{Node: node, Addr: addr, BaseURL: base}
		m.peers[addr] = p
	}
	m.mu.Unlock()

	p.mu.Lock()
	if s.Node != "" {
		p.Node = s.Node
	}
	// An instance the peer reported last time and does not now has stopped:
	// unloaded, reloaded under a new id, or lost with the node. Its caches
	// are gone whichever it was.
	gone := false
	for _, old := range p.instances {
		if !slices.ContainsFunc(s.Instances, func(i InstanceState) bool { return i.ID == old.ID }) {
			gone = true
			break
		}
	}
	goneNode := p.Node
	p.platform, p.osVersion = s.Platform, s.OSVersion
	p.models = s.Models
	p.instances = s.Instances
	p.engines = s.Engines
	p.reported = s.Inflight
	// Read here, beside the report it is compared with. A request that ends
	// between the peer taking its count and this line is counted as another's
	// until the next poll: a window of one round trip, where it used to be the
	// whole interval.
	p.sentAtPoll = p.sent.Load()
	p.accepted = s.Accepted
	p.queued = s.Queued
	p.held, p.routed = s.Held, s.Routed
	p.memTotal, p.memAvail = s.MemTotalMB, s.MemAvailableMB
	p.scheduler = s.Scheduler
	p.lastSeen = time.Now()
	p.alive = true
	p.mu.Unlock()
	if gone && m.engineGone != nil {
		m.engineGone(goneNode)
	}
}

func (m *Mesh) expire() {
	cutoff := time.Now().Add(-m.dead)
	m.mu.Lock()
	defer m.mu.Unlock()
	for addr, p := range m.peers {
		p.mu.Lock()
		if p.lastSeen.Before(cutoff) {
			p.alive = false
		}
		stale := p.lastSeen.Before(cutoff.Add(-4 * m.dead))
		p.mu.Unlock()
		if stale {
			delete(m.peers, addr)
		}
	}
}

func (m *Mesh) State() NodeState {
	seen := map[string]bool{}
	var models []string
	var engines []EngineState
	var inflight int64
	for _, e := range m.Engines() {
		es := e.snapshot()
		engines = append(engines, es)
		inflight += es.Inflight
		if !es.Healthy {
			continue
		}
		for _, mod := range es.Models {
			if !seen[mod] {
				seen[mod] = true
				models = append(models, mod)
			}
		}
	}
	sort.Strings(models)
	var instances []InstanceState
	if m.instances != nil {
		instances = m.instances()
	}
	var accepted int64
	if m.accepted != nil {
		accepted = m.accepted()
	}
	var queued int64
	if m.queued != nil {
		queued = m.queued()
	}
	var held []HeldRequest
	var routed []RoutedTo
	if m.routerView != nil {
		held, routed = m.routerView()
	}
	memTotal, memAvail := m.hostMemoryMB()
	var sched *SchedulerState
	if m.scheduler != nil {
		sched = m.scheduler()
	}
	return NodeState{
		Node:       m.node,
		Addr:       m.selfAddr,
		Platform:   Platform(),
		OSVersion:  osproc.OSVersion(),
		Models:     models,
		Inflight:   inflight,
		Accepted:   accepted,
		Queued:     queued,
		Held:       held,
		Routed:     routed,
		MemTotalMB: memTotal, MemAvailableMB: memAvail,
		Scheduler: sched,
		Engines:   engines,
		Instances: instances,
		Updated:   time.Now().UTC(),
	}
}

func (m *Mesh) Peers() []PeerView {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]PeerView, 0, len(m.peers))
	for _, p := range m.peers {
		p.mu.RLock()
		out = append(out, PeerView{
			Node:       p.Node,
			Addr:       p.Addr,
			BaseURL:    p.BaseURL,
			Platform:   p.platform,
			OSVersion:  p.osVersion,
			Alive:      p.alive,
			Instances:  append([]InstanceState(nil), p.instances...),
			Models:     append([]string(nil), p.models...),
			Inflight:   p.sent.Load() + p.others(p.reported),
			Accepted:   p.accepted,
			Queued:     p.queued,
			Held:       p.held,
			Routed:     p.routed,
			MemTotalMB: p.memTotal, MemAvailableMB: p.memAvail,
			Scheduler: p.scheduler,
			LastSeen:  p.lastSeen,
		})
		p.mu.RUnlock()
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Node < out[j].Node })
	return out
}

func (m *Mesh) Models() []string {
	seen := map[string]bool{}
	for _, mod := range m.State().Models {
		seen[mod] = true
	}
	m.mu.RLock()
	for _, p := range m.peers {
		p.mu.RLock()
		if p.alive {
			for _, mod := range p.models {
				seen[mod] = true
			}
		}
		p.mu.RUnlock()
	}
	m.mu.RUnlock()

	out := make([]string, 0, len(seen))
	for mod := range seen {
		out = append(out, mod)
	}
	sort.Strings(out)
	return out
}

type Candidate struct {
	Local   bool
	Node    string
	Name    string
	BaseURL string
	Score   float64
	// Inflight and Slots say whether the candidate has room: a request sent
	// to one with Inflight >= Slots queues. Slots is zero when unknown. For a
	// peer they cover that node's engines serving the model.
	Inflight int64
	Slots    int64
	// PrefillTokS is the measured prompt rate (for a peer, its fastest engine
	// for the model); zero when not yet measured. It is what weights this
	// candidate's score: a slow engine's queue costs more per entry (cost.go).
	PrefillTokS float64
	// KVPool is how many tokens the candidate's engines hold across all their
	// slots, zero when unknown. Slots share it, so a free slot does not mean
	// there is room for what would go in it.
	KVPool int64
	// RateUnmeasurable says this candidate can never report a throughput rate:
	// its engine serves no metrics at all (mlx-lm). That is different from a
	// rate not measured yet, and the two are scored differently; see cost.go.
	RateUnmeasurable bool
	// outstanding is the candidate's own load measure; a local engine's
	// in-flight count, or a peer's reported load plus what has been dispatched
	// since its last poll. Scoring happens after the whole set is collected, so
	// it is carried rather than folded into Score on the spot.
	outstanding int64
	// ServedModel is the id a local engine answers to, when it is not the
	// catalog key (see Engine.Served). Empty for a peer: forwarding sends the
	// catalog key, and that node's own router translates it.
	ServedModel string
	// NoConstrainedDecoding says a request asking for response_format or a
	// grammar would come back unconstrained here. For a local engine that is
	// its trait; for a peer it means every engine it has for this model
	// ignores them, so forwarding could only produce free prose. A peer with a
	// mix is left capable: its own router applies this same rule.
	NoConstrainedDecoding bool
	// NoVision says a request carrying an image would fail here: the engine
	// was loaded without its projector. For a peer it means every engine it
	// has for this model was; a peer with a mix is left capable, because its
	// own router applies this same rule.
	NoVision bool
	// Embedding says a local engine embeds and does not generate. Always
	// false for a peer, whose own router decides.
	Embedding bool

	engine *Engine
	peer   *Peer
}

// Full reports whether a request sent here would queue behind the engine's
// slots. Unknown capacity is never full.
func (c Candidate) Full() bool { return c.Slots > 0 && c.Inflight >= c.Slots }

// Load returns an unweighted request count: local in-flight requests or
// current peer dispatches plus reported external load. Score is rate-weighted.
func (c Candidate) Load() int64 {
	if c.Local {
		return c.Inflight
	}
	return c.outstanding
}

// Acquire marks the candidate busy for the duration of the request, on a local
// engine and on a peer alike. The returned function must be called when the
// request completes.
func (c Candidate) Acquire() func() {
	switch {
	case c.engine != nil:
		c.engine.inflight.Add(1)
		c.engine.lastUsed.Store(time.Now().UnixNano())
		return func() {
			// Stamp before decrementing, so an idle check that sees zero in
			// flight also sees this request's finish time.
			c.engine.lastUsed.Store(time.Now().UnixNano())
			c.engine.inflight.Add(-1)
		}
	case c.peer != nil:
		// Counted in and out here, like a local engine. The peer's own report
		// is only consulted for what others have sent it (see Peer.sent).
		c.peer.sent.Add(1)
		return func() { c.peer.sent.Add(-1) }
	}
	return func() {}
}

// Candidates returns every destination able to serve model, cheapest first.
// When localOnly is set, peers are excluded; used to stop a forwarded request
// from being forwarded again.
func (m *Mesh) Candidates(model string, localOnly bool) []Candidate {
	var out []Candidate
	for _, e := range m.Engines() {
		if !e.has(model) {
			continue
		}
		// The engine observes llm-d traffic too. Keep the router's count as a
		// floor for dispatches since the last poll, without adding counts
		// that include the same requests.
		n := max(e.inflight.Load(), e.Inflight())
		out = append(out, Candidate{
			Local:            true,
			Node:             m.node,
			Name:             e.Name,
			BaseURL:          e.DispatchURL(),
			Inflight:         n,
			Slots:            e.slots.Load(),
			PrefillTokS:      e.TrustedPrefillRate(),
			KVPool:           e.kvPool(),
			RateUnmeasurable: e.NoMetrics,
			ServedModel:      e.Served,

			NoConstrainedDecoding: e.NoConstrainedDecoding,
			NoVision:              e.NoVision,
			Embedding:             e.Embedding,
			engine:                e,
		})
	}
	if !localOnly {
		m.mu.RLock()
		for _, p := range m.peers {
			if !p.has(model) {
				continue
			}
			inflight, slots, rate := p.capacity(model)
			unmeasurable := p.rateUnmeasurable(model)
			// Read under the peer's own lock: m.mu guards the map, not the
			// fields, and refreshPeers writes Node while requests are routing.
			node, baseURL := p.identity()
			out = append(out, Candidate{
				Node:             node,
				Name:             node,
				BaseURL:          baseURL,
				Inflight:         inflight,
				RateUnmeasurable: unmeasurable,
				outstanding:      p.load(),
				Slots:            slots,
				PrefillTokS:      rate,
				KVPool:           p.pool(model),

				NoConstrainedDecoding: !p.constrains(model),
				NoVision:              p.refusesImages(model),
				peer:                  p,
			})
		}
		m.mu.RUnlock()
	}
	Rank(out, m.rateWeighted(), m.cfg.LocalBias, m.Preferred())
	return out
}

// Rank scores and sorts candidates in place, cheapest first. Replay tools use
// the same function to avoid divergence from production ranking.
func Rank(out []Candidate, rateWeighted bool, localBias float64, preferred string) {
	// Scored here rather than as each candidate is built: the weighting is
	// relative to the fastest engine *among these*, which is not known until
	// they have all been collected (see cost.go).
	ref := referenceRate(out)
	slowest := slowestRate(out)
	for i := range out {
		outstanding := out[i].outstanding
		if out[i].Local {
			outstanding = out[i].Inflight
		}
		if rateWeighted {
			out[i].Score = cost(outstanding, assumedRate(out[i], slowest), ref, localBias, out[i].Local)
			continue
		}
		out[i].Score = float64(outstanding)
		if out[i].Local {
			out[i].Score -= localBias
		}
	}

	sort.Slice(out, func(i, j int) bool {
		// A preferred node takes precedence over load. Unavailable nodes never
		// enter the candidate set, so normal ranking remains the fallback.
		pi := preferred != "" && out[i].Node == preferred
		pj := preferred != "" && out[j].Node == preferred
		if pi != pj {
			return pi
		}
		// A candidate with a free slot beats one where the request would
		// queue, whatever the scores: a queued request on a llama.cpp engine
		// waits for a slot and then evicts another conversation's cache.
		if fi, fj := out[i].Full(), out[j].Full(); fi != fj {
			return fj
		}
		if out[i].Score != out[j].Score {
			return out[i].Score < out[j].Score
		}
		if out[i].PrefillTokS != out[j].PrefillTokS {
			return out[i].PrefillTokS > out[j].PrefillTokS
		}
		return out[i].Name < out[j].Name
	})
}

// rateUnmeasurable is true only when every instance serving the model lacks
// metrics. A mixed peer can be scored using its measurable engines.
func (p *Peer) rateUnmeasurable(model string) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	found := false
	for _, i := range p.instances {
		if i.Model != model {
			continue
		}
		found = true
		// An instance from a node too old to name its engine is llama.cpp by
		// history, which does report metrics.
		if i.Engine == "" || !engines.EngineTraits(i.Engine).NoMetrics {
			return false
		}
	}
	return found
}

// hostMemoryMB returns total and available RAM in MiB, or zeros when unknown.
// Cache briefly because macOS probes spawn processes and peer polling is frequent.
func (m *Mesh) hostMemoryMB() (total, available int64) {
	if m.hostMemory == nil {
		return 0, 0
	}
	m.hostMemMu.Lock()
	defer m.hostMemMu.Unlock()
	if time.Since(m.hostMemAt) > 5*time.Second {
		m.hostMemAt = time.Now()
		t, a := m.hostMemory()
		m.hostMemTotal, m.hostMemAvail = max(t>>20, 0), max(a>>20, 0)
	}
	return m.hostMemTotal, m.hostMemAvail
}

// SetHostMemoryProvider attaches the reading of this machine's total and
// available memory, in bytes.
func (m *Mesh) SetHostMemoryProvider(f func() (total, available int64)) { m.hostMemory = f }
