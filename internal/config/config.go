// Package config loads ModelFabric node configuration.
package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gregbugaj/modelfabric/internal/runtime"
)

// Engine is a local OpenAI-compatible inference server that this node fronts,
// e.g. a llama-server or `vllm serve` process listening on loopback.
type Engine struct {
	Name    string `json:"name"`
	BaseURL string `json:"base_url"`
}

// Config is the on-disk node configuration. Every field has a usable default,
// so an empty file yields a working node.
type Config struct {
	// Node is this node's advertised name. Defaults to the Tailscale hostname.
	Node string `json:"node"`

	// Role is "entrypoint" for a node that runs no models and only takes
	// requests into the mesh — a cloud VM as the public front door to GPUs at
	// home. Empty is a normal node.
	Role string `json:"role,omitempty"`

	// Listen is the local OpenAI-compatible address apps point at: the front
	// door. Inference is routed from here, or handed to llm-d for the one model
	// it schedules.
	Listen string `json:"listen"`

	// RequireAPIKey makes Listen check the node's API key (OpenAI-style
	// "Authorization: Bearer <key>") on inference. Off by default, as LM
	// Studio's is: Listen is loopback.
	RequireAPIKey bool `json:"require_api_key,omitempty"`

	// MCPAllowEphemeral lets a request to /api/v1/chat name an MCP server
	// for this node to call ("ephemeral_mcp"). Off by default: it has the
	// node open a connection wherever a caller says, from inside your network.
	MCPAllowEphemeral bool `json:"mcp_allow_ephemeral,omitempty"`
	// MCPAllowConfigured lets a request use the servers in mcp.json
	// ("mcp/<name>"). Off by default: a local server there is a process this
	// node starts, and its tools act with this node's user's rights.
	MCPAllowConfigured bool `json:"mcp_allow_configured,omitempty"`

	// PublicListen is a second front door meant for exposure (Tailscale
	// Funnel, a TLS proxy): inference only, the API key always required, no
	// dashboard or management. Empty means none. Keep it on loopback and let
	// the proxy provide TLS.
	PublicListen string `json:"public_listen,omitempty"`

	// CORSOrigins are the web origins (scheme://host[:port]) allowed to call
	// the OpenAI-shaped API (/v1) from a browser; "*" allows any. Empty, the
	// default, allows none. Never applies to management or the dashboard.
	CORSOrigins []string `json:"cors_origins,omitempty"`

	// MeshAdmin says who may manage this node from another node's
	// dashboard: "same-owner" (default) accepts devices Tailscale reports as
	// the same user, or sharing a tag; "off" accepts none.
	MeshAdmin string `json:"mesh_admin,omitempty"`

	// WebUI serves the dashboard at Listen; nil means on. Never served on the
	// tailnet or public listeners either way.
	WebUI *bool `json:"web_ui,omitempty"`

	// MeshPort is the port peers are probed on. Every node in the mesh must
	// agree on it, since discovery works by probing rather than a registry.
	MeshPort int `json:"mesh_port"`

	// Engines are the local inference servers this node exposes to the mesh.
	Engines []Engine `json:"engines"`

	// Tag optionally restricts discovery to peers carrying this Tailscale tag
	// (e.g. "tag:modelfabric"). Empty means probe every online peer.
	Tag string `json:"tag"`

	// PollInterval is how often peers and local engines are re-checked.
	PollInterval string `json:"poll_interval"`

	// ProbeTimeout bounds a single peer or engine state check.
	ProbeTimeout string `json:"probe_timeout"`

	// DeadAfter is how long a peer may fail probes before it leaves the mesh.
	DeadAfter string `json:"dead_after"`

	// RequestStall is how long an in-flight request may make no progress at all
	// — no byte read from the engine, none written to the caller — before it is
	// abandoned and its slot released. "off" holds requests indefinitely, which
	// is what ModelFabric did before this existed and is almost never what you want:
	// a client that timed out without closing its connection stopped reading,
	// backpressure stalled the engine mid-generation, and one slot stayed
	// occupied for ninety minutes while two idle nodes went unused.
	//
	// It bounds silence, not duration. Silence is normal during prefill — a 60k
	// prompt on an Apple Silicon node is minutes before the first token — so
	// the default is generous on purpose.
	RequestStall string `json:"request_stall"`

	// MaxOutputTokens is the output ceiling ModelFabric fills into a request that
	// names none of its own. A request with no limit is asking for the context
	// window, which no client means and no shared GPU should grant: measured
	// here, one such request generated ~130,000 tokens of a 131,072 window over
	// ninety minutes for an answer already abandoned. Zero restores the old
	// unbounded behaviour. A request that states its own limit is never
	// overridden.
	MaxOutputTokens int `json:"max_output_tokens"`

	// RateWeightedRouting compares candidates by how long each would take to
	// reach a new request rather than by how many requests it already has. Nil
	// means on. Off restores comparison by queue depth alone, which treats a
	// slot on a 300 tok/s engine as equal to one on a 2,200 tok/s engine — see
	// internal/mesh/cost.go for what that cost on this fleet.
	RateWeightedRouting *bool `json:"rate_weighted_routing,omitempty"`

	// LocalBias favors local engines over peers when load is otherwise equal.
	// Expressed in units of outstanding requests.
	LocalBias float64 `json:"local_bias"`

	// ModelsRoot is the directory scanned for GGUF models. Empty disables the
	// supervisor, leaving a node that routes but hosts nothing.
	ModelsRoot string `json:"models_root"`

	// RuntimesRoot is where `mfsh runtime get` installs engine builds
	// (default ~/.modelfabric/runtimes). ModelFabric never installs into LM Studio's tree.
	RuntimesRoot string `json:"runtimes_root,omitempty"`

	// LLMDListen is where llm-d's Envoy listens when llm-d is enabled
	// (`mfsh llmd enable <model>`); loopback by default.
	LLMDListen string `json:"llmd_listen,omitempty"`

	// StateDir holds ownership records and the operation journal.
	StateDir string `json:"state_dir"`

	// LlamaServer is the llama.cpp server binary; a bare name is resolved on
	// PATH and then pinned by content.
	LlamaServer string `json:"llama_server"`

	// PortMin/PortMax bound the ports assigned to loaded instances.
	PortMin int `json:"port_min"`
	PortMax int `json:"port_max"`

	// Engine defaults applied to a load unless the request overrides them.
	ContextLength  int  `json:"context_length"`
	GPULayers      int  `json:"gpu_layers"`
	Parallel       int  `json:"parallel"`
	FlashAttention bool `json:"flash_attention"`

	// StartupTimeout bounds one model load.
	StartupTimeout string `json:"startup_timeout"`

	// EngineBind is the interface loaded engines listen on. Loopback, the
	// default, keeps them private to this machine. "tailnet" binds them to
	// this node's Tailscale address, looked up at each start, for when
	// something off-box must reach them directly, such as llm-d's EPP —
	// tailnet ACLs then become the only thing in front of them. An explicit
	// address still works, but the tailnet's own is Tailscale's to assign:
	// typed in, a config could not be shared between nodes, and a node given
	// a new address would fail every load.
	EngineBind string `json:"engine_bind"`

	// LLMDEndpointsFile, when set, is kept up to date with the mesh's model
	// servers in llm-d's file-discovery format.
	LLMDEndpointsFile string `json:"llmd_endpoints_file"`

	// Runtimes are explicitly declared inference runtimes. Each records its
	// origin, version, backend and entry point, so a launch is reproducible
	// rather than dependent on whatever is on PATH.
	Runtimes []*runtime.Definition `json:"runtimes,omitempty"`

	// PreferredNode resolves a model to this node when several hold it. It is
	// a preference, not a pin: if that node is offline or lacks the model, the
	// usual load-based choice applies.
	PreferredNode string `json:"preferred_node,omitempty"`

	// DefaultRuntime names the runtime used when a load does not pick one.
	DefaultRuntime string `json:"default_runtime,omitempty"`

	// DisableLMStudioRuntimes turns off discovery of installed LM Studio
	// engine packages.
	DisableLMStudioRuntimes bool `json:"disable_lmstudio_runtimes,omitempty"`

	// DisableLMStudioModels turns off use of an LM Studio install's models
	// directory. Models there are read-only to ModelFabric; it never writes to it.
	DisableLMStudioModels bool `json:"disable_lmstudio_models,omitempty"`

	// PrefixAffinity routes requests that share a prompt prefix to the engine
	// that last served it, so its KV cache is reused. Nil means on.
	PrefixAffinity *bool `json:"prefix_affinity,omitempty"`

	// Placement selects how the router chooses among engines serving a model,
	// when prefix affinity is on. Empty is the default: stay with the
	// engine holding the prompt, balance by tokens waiting to be read, and
	// keep a conversation off an engine that already has one living in every
	// slot (the room rule). "no-room-rule" is the default without that last
	// part, for measuring what it is worth. "home-slot" is the earlier rule:
	// the engine holding the prompt while it has a free slot, otherwise the
	// fewest requests in flight.
	Placement string `json:"placement,omitempty"`

	// QueueWait turns on the router's queue and is the longest a request is
	// held in it: when no engine has a slot the request should take, it waits
	// on this node for one, where otherwise it is sent at once to wait inside
	// an engine. Empty or "0" is off. After this long the request is placed
	// as if there were no queue, so nothing is refused for having waited.
	QueueWait string `json:"queue_wait,omitempty"`
	// QueueGrace is how long a slot must have stayed free before a waiting
	// request that is not its conversation may take it. Empty means 2s, which
	// 92 to 96% of an agent's follow-up calls arrive within.
	QueueGrace string `json:"queue_grace,omitempty"`

	// CacheDiskMiB turns on the disk tier of the prompt cache and caps it:
	// a conversation's KV state is saved when its slot is taken or its engine
	// unloaded, and restored when the conversation returns. Zero, the default,
	// is off, because the saved state contains the conversation and nothing
	// else here writes prompts to disk unasked. llama.cpp engines only.
	CacheDiskMiB int `json:"cache_disk_mib,omitempty"`
	// CacheDiskDir is where it is kept; empty means a "slots" directory under
	// the node's state directory.
	CacheDiskDir string `json:"cache_disk_dir,omitempty"`

	// JITLoad loads a model from this node's catalog when a request names one
	// no node is serving, and lists every downloaded model in /v1/models, as
	// LM Studio does with JIT on. Off by default.
	JITLoad bool `json:"jit_load,omitempty"`
	// JITTTL unloads a JIT-loaded model after this long idle ("60m", the
	// default, as in LM Studio). "0" keeps it loaded. A request's "ttl" field
	// (seconds) overrides it.
	JITTTL string `json:"jit_ttl,omitempty"`
	// JITAutoEvict unloads idle JIT-loaded models before a JIT load of
	// another, so at most one on-demand model holds VRAM. Nil means on.
	// Manually loaded models are never evicted.
	JITAutoEvict *bool `json:"jit_auto_evict,omitempty"`

	// ExtraModelRoots are additional directories to scan for models.
	ExtraModelRoots []string `json:"extra_model_roots,omitempty"`

	// LogBodies starts the node with request and response capture already on,
	// so the dashboard's Activity page and `mfsh log` show what was sent and
	// returned. It can also be switched at runtime from the dashboard; this is
	// just the starting state.
	//
	// Bodies live in the in-memory ring and are dropped as it rolls over.
	// Nothing reaches disk unless LogBodiesFile is set.
	LogBodies bool `json:"log_bodies,omitempty"`
	// LogBodiesMax caps how much of each body is kept, in bytes (default 32768).
	LogBodiesMax int `json:"log_bodies_max,omitempty"`
	// LogBodiesKeep is how many requests the ring holds (default 200, max
	// 2000). The dashboard can change it while the node runs.
	LogBodiesKeep int `json:"log_bodies_keep,omitempty"`
	// LogBodiesFile, when set, also appends every captured request to this
	// file as one JSON object per line. Unlike the ring, a file forgets
	// nothing — which is the point, and the reason it is config rather than a
	// button. Written 0600.
	LogBodiesFile string `json:"log_bodies_file,omitempty"`

	// VerifyFull re-hashes every pinned model file on every load instead of
	// trusting the size+mtime fast path. Off by default: re-hashing 16GB costs
	// seconds, and size+mtime catches the failures that actually happen
	// (corruption, truncation, a partial re-download, a swapped file). Turn it
	// on where an attacker who can preserve both is in scope.
	VerifyFull bool `json:"verify_full,omitempty"`
}

// Stall resolves RequestStall. A negative result means "hold indefinitely",
// which is what the router takes as disabled — spelled "off" in the config,
// because a bare 0 in a timeout field reads as a mistake rather than a choice.
func (c Config) Stall() time.Duration {
	if strings.EqualFold(strings.TrimSpace(c.RequestStall), "off") {
		return -1
	}
	return dur(c.RequestStall, 15*time.Minute)
}

// LoadTimeouts resolves supervisor durations.
func (c Config) LoadTimeouts() (startup, stop time.Duration) {
	return dur(c.StartupTimeout, 10*time.Minute), 20 * time.Second
}

func Default() Config {
	return Config{
		Listen:          "127.0.0.1:1234",
		MeshPort:        1234,
		LlamaServer:     "llama-server",
		PortMin:         18000,
		PortMax:         18099,
		ContextLength:   8192,
		GPULayers:       99,
		Parallel:        4,
		FlashAttention:  true,
		StartupTimeout:  "10m",
		PollInterval:    "2s",
		ProbeTimeout:    "1500ms",
		RequestStall:    "15m",
		MaxOutputTokens: 16384,
		DeadAfter:       "15s",
		LocalBias:       0.5,
	}
}

// Load reads a config file, filling unset fields with defaults. A missing file
// is not an error: the defaults alone describe a valid (engine-less) node.
func Load(path string) (Config, error) {
	c := Default()
	if path == "" {
		return c, nil
	}
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return c, nil
	}
	if err != nil {
		return c, err
	}
	return parse(b, path)
}

func parse(b []byte, path string) (Config, error) {
	c := Default()
	// An empty or whitespace-only file is an absent config, not a broken one:
	// `touch config.json` is a normal thing to do, and it used to stop the
	// node with a parse error.
	if len(bytes.TrimSpace(b)) == 0 {
		b = []byte("{}")
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return c, fmt.Errorf("parse %s: %w", path, err)
	}
	if c.Listen == "" {
		c.Listen = "127.0.0.1:1234"
	}
	if c.MeshPort == 0 {
		c.MeshPort = 1234
	}
	if err := ValidateCORSOrigins(c.CORSOrigins); err != nil {
		return c, fmt.Errorf("%s: %w", path, err)
	}
	if err := c.checkPublicPort(); err != nil {
		return c, fmt.Errorf("%s: %w", path, err)
	}
	for i, e := range c.Engines {
		if e.BaseURL == "" {
			return c, fmt.Errorf("engine %d (%q): base_url is required", i, e.Name)
		}
		if e.Name == "" {
			c.Engines[i].Name = fmt.Sprintf("engine-%d", i)
		}
	}
	return c, nil
}

// Durations resolves the string-valued interval fields, substituting defaults
// for anything unparseable.
func (c Config) Durations() (poll, probe, dead time.Duration) {
	return dur(c.PollInterval, 2*time.Second),
		dur(c.ProbeTimeout, 1500*time.Millisecond),
		dur(c.DeadAfter, 15*time.Second)
}

// JITPolicy returns the idle TTL and auto-evict setting for JIT loads.
func (c Config) JITPolicy() (ttl time.Duration, autoEvict bool, err error) {
	autoEvict = c.JITAutoEvict == nil || *c.JITAutoEvict
	// Trimmed once, here: the switch below compared a trimmed value while the
	// duration parse used the original, so " 15m " matched no case and then
	// failed to parse.
	raw := strings.TrimSpace(c.JITTTL)
	switch raw {
	case "":
		return 60 * time.Minute, autoEvict, nil
	case "0", "off", "never":
		return 0, autoEvict, nil
	}
	ttl, err = time.ParseDuration(raw)
	if err != nil || ttl < 0 {
		return 0, autoEvict, fmt.Errorf("jit_ttl %q: want a duration such as 60m, or 0 for none", c.JITTTL)
	}
	return ttl, autoEvict, nil
}

func dur(s string, def time.Duration) time.Duration {
	if s == "" {
		return def
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return def
	}
	return d
}

// RoleEntrypoint is a node that runs no models and routes into the mesh.
const RoleEntrypoint = "entrypoint"

// Entrypoint reports whether this node only routes (see Role).
func (c Config) Entrypoint() bool { return c.Role == RoleEntrypoint }

// WebUIEnabled reports whether the dashboard is served (default on).
func (c Config) WebUIEnabled() bool { return c.WebUI == nil || *c.WebUI }

// ValidateCORSOrigins checks each entry is "*" or a bare origin. A path or a
// trailing slash is refused rather than trimmed: browsers send the origin
// alone, so "http://localhost:3000/app" would silently match nothing.
func ValidateCORSOrigins(origins []string) error {
	for _, o := range origins {
		if o == "*" {
			continue
		}
		u, err := url.Parse(o)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" ||
			(u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
			return fmt.Errorf("cors_origins: %q is not an origin; use scheme://host[:port], for example http://localhost:3000, or \"*\"", o)
		}
		if u.Path == "/" {
			return fmt.Errorf("cors_origins: %q has a trailing slash, which a browser never sends; use %s", o, strings.TrimSuffix(o, "/"))
		}
	}
	return nil
}

// Update changes the named keys of the config file at path and leaves every
// other key exactly as it was written, including ones this build does not
// know. A nil value removes the key, so the default applies. The result is
// validated as Load would before anything is written; the previous file is
// kept beside it as path+".bak", and the new one replaces it in one rename.
//
// Keys come out sorted: the file is JSON, so there are no comments to lose,
// but the order a person wrote them in is not kept.
func Update(path string, set map[string]any) (Config, error) {
	raw := map[string]json.RawMessage{}
	old, err := os.ReadFile(path)
	switch {
	case os.IsNotExist(err):
		old = nil
	case err != nil:
		return Config{}, err
	case len(bytes.TrimSpace(old)) > 0:
		if err := json.Unmarshal(old, &raw); err != nil {
			return Config{}, fmt.Errorf("parse %s: %w; fix it by hand before saving settings over it", path, err)
		}
	}
	for k, v := range set {
		if v == nil {
			delete(raw, k)
			continue
		}
		enc, err := json.Marshal(v)
		if err != nil {
			return Config{}, fmt.Errorf("%s: %w", k, err)
		}
		raw[k] = enc
	}
	out, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		return Config{}, err
	}
	out = append(out, '\n')
	c, err := parse(out, path)
	if err != nil {
		return Config{}, err
	}
	if _, _, err := c.JITPolicy(); err != nil {
		return Config{}, err
	}

	mode := os.FileMode(0o644)
	if fi, err := os.Stat(path); err == nil {
		mode = fi.Mode().Perm()
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return Config{}, err
	}
	if old != nil {
		if err := os.WriteFile(path+".bak", old, mode); err != nil {
			return Config{}, fmt.Errorf("keep the previous config as %s.bak: %w", path, err)
		}
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".config-*")
	if err != nil {
		return Config{}, err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return Config{}, err
	}
	if _, err := tmp.Write(out); err != nil {
		tmp.Close()
		return Config{}, err
	}
	if err := tmp.Close(); err != nil {
		return Config{}, err
	}
	return c, os.Rename(tmp.Name(), path)
}

// checkPublicPort refuses a public listener on the port the front door or the
// mesh already holds. On every interface (the dashboard's "Serve on Network")
// it would collide with both — the mesh listener is tailnet-IP:mesh_port — and
// the node would fail to bind at the next start instead of when it was saved.
func (c Config) checkPublicPort() error {
	if c.PublicListen == "" {
		return nil
	}
	_, pub, err := net.SplitHostPort(c.PublicListen)
	if err != nil {
		return fmt.Errorf("public_listen %q is not host:port", c.PublicListen)
	}
	if _, front, err := net.SplitHostPort(c.Listen); err == nil && front == pub {
		return fmt.Errorf("public_listen uses port %s, which listen already has; pick another port", pub)
	}
	if pub == strconv.Itoa(c.MeshPort) {
		return fmt.Errorf("public_listen uses port %s, which is the mesh port; pick another port", pub)
	}
	return nil
}

// EngineBindTailnet is the engine_bind value meaning "this node's tailnet
// address", resolved when the node starts.
const EngineBindTailnet = "tailnet"

// BindsTailnet reports whether engine_bind means this node's tailnet address:
// "tailnet", or any address in Tailscale's ranges. The second is how the
// setting used to be written, from advice that said to put the node's
// tailnet IP there. Tailscale assigns that address and can change it, and a
// tailnet address that is not this node's cannot be bound at all, so a
// literal one is read as what it always meant: the tailnet.
func (c Config) BindsTailnet() bool {
	return c.EngineBind == EngineBindTailnet || IsTailnetAddr(c.EngineBind)
}

// IsTailnetAddr reports an address in Tailscale's ranges: 100.64.0.0/10 or
// fd7a:115c:a1e0::/48.
func IsTailnetAddr(h string) bool {
	ip := net.ParseIP(strings.Trim(h, "[]"))
	if ip == nil {
		return false
	}
	if v4 := ip.To4(); v4 != nil {
		return v4[0] == 100 && v4[1] >= 64 && v4[1] <= 127
	}
	return len(ip) == net.IPv6len && ip[0] == 0xfd && ip[1] == 0x7a &&
		ip[2] == 0x11 && ip[3] == 0x5c && ip[4] == 0xa1 && ip[5] == 0xe0
}

// ResolveEngineBind is the address engines bind to, given this node's tailnet
// address (empty when Tailscale gave it none). Anything that means the tailnet
// is Tailscale's current address for this node, whatever was written. With no
// tailnet address it says so and falls back to loopback: narrower than asked,
// never wider, and the node still serves this machine.
func (c Config) ResolveEngineBind(selfAddr string) (bind string, warning string) {
	if !c.BindsTailnet() {
		return c.EngineBind, ""
	}
	if selfAddr == "" {
		return "", "engine_bind means the tailnet, but this node has no tailnet address (is Tailscale up?); engines bind to loopback, so nothing off this machine can reach them"
	}
	if c.EngineBind != EngineBindTailnet && c.EngineBind != selfAddr {
		return selfAddr, "engine_bind " + c.EngineBind + " is not this node's tailnet address any more; binding engines to " + selfAddr + ", Tailscale's current one. Set engine_bind to \"tailnet\" to say so"
	}
	return selfAddr, ""
}

// Queue reports the router queue's settings: how long a slot must have stayed
// free, and the longest wait. on is false when the queue is not configured.
func (c Config) Queue() (grace, maxWait time.Duration, on bool) {
	maxWait = dur(c.QueueWait, 0)
	return dur(c.QueueGrace, 2*time.Second), maxWait, maxWait > 0
}
