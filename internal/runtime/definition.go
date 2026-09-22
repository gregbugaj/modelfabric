// Package runtime turns a versioned runtime definition plus a catalog model
// into a concrete engine launch.
//
// The spec calls for "a small versioned runtime definition embedded in the
// deployment configuration or operator-controlled file", and separates three
// decisions that a hardcoded binary name conflates:
//
//	Origin:            LM Studio package, public upstream build, or Marie runtime
//	Execution:         native process or script (Track B), or an external endpoint
//	Inference protocol: OpenAI-compatible, LM Studio-native, or engine-specific
//
// So a runtime is declared, not assumed. That is what lets one node offer a
// CUDA build and another a CPU build of the same engine, and what makes the
// launch reproducible: "record an immutable package/image reference and digest,
// platform/backend, entry point, typed parameters, model location, endpoint and
// readiness contract, and stop behavior".
package runtime

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/gregbugaj/modelfabric/internal/inputs"
)

// Origin is where a runtime came from.
type Origin string

const (
	OriginUpstream Origin = "upstream" // a public upstream build
	OriginLMStudio Origin = "lmstudio" // an LM Studio engine package
	OriginMarie    Origin = "marie"    // built and shipped by us
	OriginCustom   Origin = "custom"   // operator-provided
)

// Protocol is the inference API an engine speaks.
type Protocol string

const (
	ProtocolOpenAI Protocol = "openai"
)

// Definition is one declared runtime.
type Definition struct {
	Name    string `json:"name"`
	Engine  string `json:"engine"`  // "llama.cpp"
	Origin  Origin `json:"origin"`  // upstream | lmstudio | marie | custom
	Version string `json:"version"` // engine build identifier, e.g. "b4321"
	// LlamaBuild is the upstream llama.cpp build number underneath, whoever
	// packaged it (LM Studio's 2.41.0 is b11026). Zero when unknown. It is
	// what makes builds from different packagers comparable.
	LlamaBuild int `json:"llama_build,omitempty"`
	// DisplayName is the package's human name ("CUDA 12 llama.cpp (Linux)"),
	// from its display-data.json as LM Studio shows it.
	DisplayName string   `json:"display_name,omitempty"`
	Backend     string   `json:"backend"` // cuda | rocm | metal | vulkan | cpu
	Protocol    Protocol `json:"protocol"`

	// Entrypoint is the executable or shell script to run. A script is a
	// supported launch mechanism: it "can initialize the packaged environment
	// and invoke the server".
	Entrypoint string `json:"entrypoint"`

	// Args are extra engine arguments appended to the generated ones. They are
	// typed configuration, never assembled from an inference request.
	Args []string `json:"args,omitempty"`

	// Env is added to the child environment.
	Env map[string]string `json:"env,omitempty"`

	// Digest pins the runtime's verified artifact set by content: it is the
	// manifest hash from VerifyTree, which covers the entrypoint and every
	// file the package declares — not the entrypoint's own file hash, as this
	// comment used to say. Computed on first use when empty.
	Digest string `json:"digest,omitempty"`

	// Defaults applied to a load unless the request overrides them.
	ContextLength  int    `json:"context_length,omitempty"`
	GPULayers      int    `json:"gpu_layers,omitempty"`
	Parallel       int    `json:"parallel,omitempty"`
	FlashAttention bool   `json:"flash_attention,omitempty"`
	DraftMax       int    `json:"draft_max,omitempty"`
	BatchSize      int    `json:"batch_size,omitempty"`
	UBatchSize     int    `json:"ubatch_size,omitempty"`
	CacheTypeK     string `json:"cache_type_k,omitempty"`
	CacheTypeV     string `json:"cache_type_v,omitempty"`
	// LoadMode is llama.cpp's model loading mode: auto|none|mmap|mlock, and
	// combinations such as "mmap+mlock".
	LoadMode string `json:"load_mode,omitempty"`
	// KVUnified shares one KV cache across parallel slots instead of splitting
	// the context between them.
	KVUnified *bool `json:"kv_unified,omitempty"`
	// CtxCheckpoints bounds how many context checkpoints are retained.
	CtxCheckpoints int `json:"ctx_checkpoints,omitempty"`
	// Threads for generation; 0 leaves the engine's own default.
	Threads int `json:"threads,omitempty"`

	// resolved is the absolute entrypoint path, filled by Resolve.
	resolved string
	// pkgDir is the package root when this runtime came from a package.
	pkgDir string
	// vendorDirs are dependency packages outside pkgDir.
	vendorDirs []string
	// domains is what the engine serves ("llm", "embedding").
	domains []string
	// req is what the package declares it needs, for compatibility checks.
	req Requirements
	// provenance is set for packages ModelFabric installed itself.
	provenance *PackageProvenance

	mu       sync.Mutex
	verified *inputs.VerifiedInput
	manifest []byte
}

// PinKind describes how completely a runtime is pinned.
type PinKind string

const (
	// PinContent means the entrypoint itself is content-addressed.
	PinContent PinKind = "content"
	// PinPartial means the entrypoint is a script: its own bytes are pinned,
	// but the environment it initializes is not. The spec is explicit — "a
	// script digest alone does not pin its Python environment or image" — so
	// this is reported rather than glossed over.
	PinPartial PinKind = "partial"
)

// Pin reports how completely this runtime is pinned.
func (d *Definition) Pin() PinKind {
	if d.isScript() {
		return PinPartial
	}
	return PinContent
}

func (d *Definition) isScript() bool {
	switch strings.ToLower(filepath.Ext(d.resolvedOrEntrypoint())) {
	case ".sh", ".bash", ".py":
		return true
	}
	// A file starting with a shebang is a script whatever it is called.
	f, err := os.Open(d.resolvedOrEntrypoint())
	if err != nil {
		return false
	}
	defer f.Close()
	var head [2]byte
	if n, _ := f.Read(head[:]); n == 2 && head[0] == '#' && head[1] == '!' {
		return true
	}
	return false
}

func (d *Definition) resolvedOrEntrypoint() string {
	if d.resolved != "" {
		return d.resolved
	}
	return d.Entrypoint
}

// Path is the absolute entrypoint, once resolved.
func (d *Definition) Path() string { return d.resolvedOrEntrypoint() }

// Resolve locates the entrypoint and applies defaults. A bare name is looked up
// on PATH and then pinned by content, because "a mutable system environment is
// unverified and cannot be reported as a pinned runtime".
func (d *Definition) Resolve() error {
	if d.Name == "" {
		return fmt.Errorf("runtime has no name")
	}
	if d.Engine == "" {
		d.Engine = "llama.cpp"
	}
	if d.Protocol == "" {
		d.Protocol = ProtocolOpenAI
	}
	if d.Origin == "" {
		d.Origin = OriginUpstream
	}
	if d.Entrypoint == "" {
		return fmt.Errorf("runtime %q has no entrypoint", d.Name)
	}
	if _, ok := engines[d.Engine]; !ok {
		return fmt.Errorf("runtime %q: unsupported engine %q", d.Name, d.Engine)
	}

	path := d.Entrypoint
	if !strings.ContainsRune(path, os.PathSeparator) {
		found, err := exec.LookPath(path)
		if err != nil {
			return fmt.Errorf("runtime %q: %q not found: %w", d.Name, path, err)
		}
		path = found
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return fmt.Errorf("runtime %q: %w", d.Name, err)
	}
	if info.IsDir() {
		return fmt.Errorf("runtime %q: entrypoint %s is a directory", d.Name, abs)
	}
	if info.Mode()&0o111 == 0 {
		return fmt.Errorf("runtime %q: entrypoint %s is not executable", d.Name, abs)
	}
	d.resolved = abs

	// A definition's own Args are appended in the same override position as a
	// load's extra_args, which Validate screens; without the same rule here a
	// runtime could set --host or --model and ModelFabric would not notice.
	if err := CheckManagedFlags(fmt.Sprintf("runtime %q args", d.Name), d.Args); err != nil {
		return err
	}

	// Only zero meant "unset", so a negative value passed straight through to
	// the engine's flags (-c, --parallel, -ngl) and to KVCacheTokens, where it
	// is multiplied out. A configuration mistake became an engine that failed
	// to start, or started with nonsense.
	for _, f := range []struct {
		name string
		val  int
	}{
		{"context_length", d.ContextLength}, {"gpu_layers", d.GPULayers},
		{"parallel", d.Parallel}, {"draft_max", d.DraftMax},
		{"batch_size", d.BatchSize}, {"ubatch_size", d.UBatchSize},
	} {
		if f.val < 0 {
			return fmt.Errorf("runtime %q: %s must not be negative (got %d)", d.Name, f.name, f.val)
		}
	}

	if d.ContextLength == 0 {
		d.ContextLength = 8192
	}
	if d.GPULayers == 0 {
		d.GPULayers = 99
	}
	if d.Parallel == 0 {
		d.Parallel = 4
	}
	if d.DraftMax == 0 {
		d.DraftMax = 3
	}
	if d.BatchSize == 0 {
		d.BatchSize = 2048
	}
	if d.UBatchSize == 0 {
		d.UBatchSize = 512
	}
	if d.CacheTypeK == "" {
		d.CacheTypeK = "f16"
	}
	if d.CacheTypeV == "" {
		d.CacheTypeV = "f16"
	}
	if d.LoadMode == "" {
		// Matches what LM Studio passes: memory-map the weights and keep them
		// resident rather than letting them be paged back out.
		d.LoadMode = "mmap+mlock"
	}
	if d.KVUnified == nil {
		t := true
		d.KVUnified = &t
	}
	if d.CtxCheckpoints == 0 {
		// Checkpoints live in host RAM, per slot, and grow with the
		// conversation: ~150 MiB + ~4 KiB/token each for Qwen3.8-27B. At
		// llama.cpp's 32 one slot held 7.4 GB after 25 agent turns at 23k
		// tokens, and 90k-token SWE-bench conversations OOM-killed a 15 GB
		// host. An agent's prompt diverges only near its end (the template
		// drops earlier reasoning), so the newest checkpoint is the one used:
		// 32, 4 and 2 did identical prefill work on that replay.
		d.CtxCheckpoints = 4
	}
	return nil
}

// Verify pins the entrypoint by content, caching the result.
func (d *Definition) Verify() (*inputs.VerifiedInput, []byte, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.verified != nil {
		return d.verified, d.manifest, nil
	}
	// Verify the artifact set the package declares, not just the entrypoint:
	// llama-server here is a 17KB loader that dlopens the real engine, so
	// hashing it alone would pin almost nothing.
	files, err := d.ArtifactFiles()
	if err != nil {
		return nil, nil, err
	}
	root := d.Dir()
	vi, manifest, err := inputs.VerifyTree(root, files, d.Version)
	if err != nil {
		return nil, nil, err
	}
	// A declared digest is a claim about which build this is; honour it.
	if d.Digest != "" && !strings.EqualFold(d.Digest, vi.ManifestSHA256) {
		return nil, nil, fmt.Errorf(
			"runtime %q: entrypoint digest %s does not match the pinned %s",
			d.Name, vi.ManifestSHA256, strings.ToLower(d.Digest))
	}
	d.Digest = vi.ManifestSHA256
	d.verified, d.manifest = vi, manifest
	return vi, manifest, nil
}

// Registry holds the runtimes this node can launch.
type Registry struct {
	mu       sync.RWMutex // runtimes can be installed and removed while serving
	defs     []*Definition
	fallback string // name of the default runtime
	hw       *Hardware
}

// SetHardware lets default selection skip runtimes that cannot run here.
func (r *Registry) SetHardware(hw Hardware) {
	c := hw.clone()
	r.mu.Lock()
	defer r.mu.Unlock()
	r.hw = &c
}

// Hardware reports the surveyed machine, if a survey was attached.
// Hardware returns a copy of the surveyed hardware. The registry used to hand
// out its own pointer, so a caller could mutate what every fit decision reads —
// and CPUFlags and GPUs are reference types, so even the struct copy that
// SetHardware stored still shared them with the caller's value.
func (r *Registry) Hardware() *Hardware {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.hw == nil {
		return nil
	}
	c := r.hw.clone()
	return &c
}

// Pinned is the explicitly selected default, or "" for automatic selection.
func (r *Registry) Pinned() string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.fallback
}

// SetDefault makes name the default for loads that do not pick a runtime.
func (r *Registry) SetDefault(name string) error {
	if name == "" {
		r.mu.Lock()
		r.fallback = ""
		r.mu.Unlock()
		return nil
	}
	if _, ok := r.Lookup(name); !ok {
		return fmt.Errorf("no runtime named %q", name)
	}
	r.mu.Lock()
	r.fallback = name
	r.mu.Unlock()
	return nil
}

// Reload replaces the runtime set, keeping the default and the hardware
// survey. Used after `mfsh runtime get` or `remove` changes what is installed.
// A default that no longer exists falls back to automatic selection.
func (r *Registry) Reload(defs []*Definition) []error {
	fresh, errs := NewRegistry(defs, "")
	r.mu.Lock()
	defer r.mu.Unlock()
	r.defs = fresh.defs
	if r.fallback != "" {
		found := false
		for _, d := range r.defs {
			found = found || d.Name == r.fallback
		}
		if !found {
			r.fallback = ""
		}
	}
	return errs
}

// NewRegistry resolves each definition. A definition that fails to resolve is
// kept out of the registry but reported, so `mfsh runtime` can explain why a
// declared runtime is unavailable rather than silently omitting it.
func NewRegistry(defs []*Definition, defaultName string) (*Registry, []error) {
	r := &Registry{fallback: defaultName}
	var errs []error
	for _, d := range defs {
		if err := d.Resolve(); err != nil {
			errs = append(errs, err)
			continue
		}
		r.defs = append(r.defs, d)
	}
	sort.Slice(r.defs, func(i, j int) bool { return r.defs[i].Name < r.defs[j].Name })
	return r, errs
}

func (r *Registry) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.defs)
}

// All returns every usable runtime.
func (r *Registry) All() []*Definition {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return append([]*Definition(nil), r.defs...)
}

// usable drops runtimes known not to run on this machine, so the automatic
// choice never lands on, say, a CUDA build the GPU architecture cannot run.
// Unknown fit is kept: an old package that omits its targets may still work.
func (r *Registry) usable() []*Definition {
	if r.hw == nil {
		return r.defs
	}
	var out []*Definition
	for _, d := range r.defs {
		if fit, _ := d.Check(*r.hw); fit != FitNo {
			out = append(out, d)
		}
	}
	if len(out) == 0 {
		return r.defs
	}
	// Prefer a runtime that is known to fit over one that merely might.
	var known []*Definition
	for _, d := range out {
		if fit, _ := d.Check(*r.hw); fit == FitYes {
			known = append(known, d)
		}
	}
	if len(known) > 0 {
		return known
	}
	return out
}

// Lookup finds a runtime by name.
func (r *Registry) Lookup(name string) (*Definition, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, d := range r.defs {
		if d.Name == name {
			return d, true
		}
	}
	return nil, false
}

// Default returns the runtime used when a load names none, for a model in any
// format.
func (r *Registry) Default() (*Definition, error) { return r.DefaultFor("") }

// DefaultFor returns the runtime used when a load names none, for a model in
// the given format ("gguf", "mlx"; empty means any). A machine can have engines
// for several formats — a Mac runs GGUF on llama.cpp and MLX weights on mlx-lm
// — so the format decides which engines are even candidates.
func (r *Registry) DefaultFor(format string) (*Definition, error) {
	r.mu.RLock()
	n, fallback := len(r.defs), r.fallback
	r.mu.RUnlock()
	if n == 0 {
		return nil, fmt.Errorf("no usable inference runtime is configured")
	}
	if fallback != "" {
		d, ok := r.Lookup(fallback)
		if !ok {
			return nil, fmt.Errorf("default_runtime %q is not among the usable runtimes", fallback)
		}
		// A configured default that cannot read this model is not an error:
		// it simply does not apply here, and the best fitting engine is used.
		if d.LoadsFormat(format) {
			return d, nil
		}
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	// Prefer an accelerator this machine actually has, then the newest build.
	// Taking defs[0] would pick the alphabetically first name, which is the
	// oldest CPU build on a typical install.
	var candidates []*Definition
	for _, d := range r.usable() {
		if d.LoadsFormat(format) {
			candidates = append(candidates, d)
		}
	}
	if len(candidates) == 0 {
		return nil, fmt.Errorf("no installed runtime can load %s weights", format)
	}
	return best(candidates), nil
}

// SelectFor resolves a runtime by name for a model in the given format, or the
// default for that format when name is empty. A named runtime that cannot read
// the format is refused rather than launched to fail.
func (r *Registry) SelectFor(name, format string) (*Definition, error) {
	if name == "" {
		return r.DefaultFor(format)
	}
	d, ok := r.Lookup(name)
	if !ok {
		return nil, fmt.Errorf("no runtime named %q", name)
	}
	if !d.LoadsFormat(format) {
		return nil, fmt.Errorf("runtime %q is %s and cannot load %s weights", name, d.Engine, format)
	}
	return d, nil
}

// LoadsFormat reports whether this runtime's engine reads that weights format.
// An empty format, or one ModelFabric has no mapping for, matches everything: the
// engine will say so itself, and refusing here would block a format ModelFabric
// simply does not know about yet.
func (d *Definition) LoadsFormat(format string) bool {
	if format == "" {
		return true
	}
	for engine, formats := range engineFormats {
		for _, f := range formats {
			if strings.EqualFold(f, format) {
				return strings.EqualFold(engine, d.Engine)
			}
		}
	}
	return true
}
