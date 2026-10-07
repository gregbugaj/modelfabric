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
	// requests into the mesh; a cloud VM as the public front door to GPUs at
	// home. Empty is a normal node.
	Role string `json:"role,omitempty"`

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

	Engines []Engine `json:"engines"`

	// Tag optionally restricts discovery to peers carrying this Tailscale tag
	// (e.g. "tag:modelfabric"). Empty means probe every online peer.
	Tag string `json:"tag"`

	PollInterval string `json:"poll_interval"`

	ProbeTimeout string `json:"probe_timeout"`

	// DeadAfter is how long a peer may fail probes before it leaves the mesh.
	DeadAfter string `json:"dead_after"`

	// RequestStall bounds inactivity in upstream reads and client writes.
	// "off" disables it. The default allows long prefills while bounding
	// stalled connections that would otherwise hold engine slots indefinitely.
	RequestStall string `json:"request_stall"`

	// MaxOutputTokens supplies an output ceiling only when the request has none.
	// Zero disables the default ceiling. Explicit client limits take precedence.
	MaxOutputTokens int `json:"max_output_tokens"`

	// RateWeightedRouting compares estimated service delay using prefill rates.
	// Nil enables it; false compares queue depth alone. See internal/mesh/cost.go.
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

	PortMin int `json:"port_min"`
	PortMax int `json:"port_max"`

	// Engine defaults applied to a load unless the request overrides them.
	ContextLength  int  `json:"context_length"`
	GPULayers      int  `json:"gpu_layers"`
	Parallel       int  `json:"parallel"`
	FlashAttention bool `json:"flash_attention"`

	StartupTimeout string `json:"startup_timeout"`

	// EngineBind defaults to loopback. "tailnet" resolves this node's current
	// Tailscale address at each start; engine access then relies on tailnet ACLs.
	// Other explicit addresses are used as configured.
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

	DefaultRuntime string `json:"default_runtime,omitempty"`

	DisableLMStudioRuntimes bool `json:"disable_lmstudio_runtimes,omitempty"`

	// DisableLMStudioModels turns off use of an LM Studio install's models
	// directory. Models there are read-only to ModelFabric; it never writes to it.
	DisableLMStudioModels bool `json:"disable_lmstudio_models,omitempty"`

	// PrefixAffinity routes requests that share a prompt prefix to the engine
	// that last served it, so its KV cache is reused. Nil means on.
	PrefixAffinity *bool `json:"prefix_affinity,omitempty"`

	// Placement selects the prefix-affinity policy. Empty uses prompt affinity,
	// prefill backlog, and conversation residency. "no-room-rule" omits the
	// residency check. "home-slot" prefers the cached engine with a free slot,
	// then the fewest requests in flight.
	Placement string `json:"placement,omitempty"`

	// QueueWait enables router-side waiting for a suitable slot and sets its
	// maximum duration. Empty or "0" disables it. Expiry dispatches using
	// placement order rather than rejecting the request.
	QueueWait string `json:"queue_wait,omitempty"`
	// QueueGrace is how long a slot must have stayed free before a waiting
	// request that is not its conversation may take it. Empty means 2s, which
	// 92 to 96% of an agent's follow-up calls arrive within.
	QueueGrace string `json:"queue_grace,omitempty"`

	// CacheDiskMiB caps persisted llama.cpp KV state in MiB. Zero disables it.
	// Snapshots contain conversation tokens, so disk caching requires opt-in.
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

	ExtraModelRoots []string `json:"extra_model_roots,omitempty"`

	// LogBodies enables in-memory request and response capture at startup.
	// It can be toggled at runtime. Disk persistence requires LogBodiesFile.
	LogBodies bool `json:"log_bodies,omitempty"`
	// LogBodiesMax caps how much of each body is kept, in bytes (default 32768).
	LogBodiesMax int `json:"log_bodies_max,omitempty"`
	// LogBodiesKeep is how many requests the ring holds (default 200, max
	// 2000). The dashboard can change it while the node runs.
	LogBodiesKeep int `json:"log_bodies_keep,omitempty"`
	// LogBodiesFile appends captured requests as JSON lines with mode 0600.
	// Persistence requires configuration; the dashboard toggle controls capture only.
	LogBodiesFile string `json:"log_bodies_file,omitempty"`

	// VerifyFull re-hashes pinned model files on each load. Otherwise verification
	// uses size and mtime, which cannot detect edits preserving both values.
	VerifyFull bool `json:"verify_full,omitempty"`
}

// Stall returns the inactivity timeout, or a negative value when set to "off".
func (c Config) Stall() time.Duration {
	if strings.EqualFold(strings.TrimSpace(c.RequestStall), "off") {
		return -1
	}
	return dur(c.RequestStall, 15*time.Minute)
}

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

func (c Config) JITPolicy() (ttl time.Duration, autoEvict bool, err error) {
	autoEvict = c.JITAutoEvict == nil || *c.JITAutoEvict
	// Normalize once so keyword matching and duration parsing use the same value.
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

const RoleEntrypoint = "entrypoint"

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

// Update changes named keys, preserving unknown keys and removing nil values.
// It validates before writing, saves the previous file as path+".bak", and
// replaces it atomically. Output keys are sorted.
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
// it would collide with both; the mesh listener is tailnet-IP:mesh_port; and
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

// BindsTailnet accepts "tailnet" and legacy literal addresses in Tailscale
// ranges. Both resolve to this node's current address, which may change.
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

// ResolveEngineBind resolves tailnet settings to this node's current address.
// If unavailable, it reports the error and falls back to loopback.
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
