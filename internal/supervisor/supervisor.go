// Package supervisor owns model instances on this node: loading, unloading,
// and reporting what is resident.
//
// It combines verified inputs, process ownership and a durable operation journal.
// Loading is manual unless JIT is enabled. Idle policies can unload models,
// but no background loop reloads one an operator intentionally stopped.
package supervisor

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"github.com/gregbugaj/modelfabric/internal/devlog"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gregbugaj/modelfabric/internal/catalog"
	"github.com/gregbugaj/modelfabric/internal/engineshim"
	"github.com/gregbugaj/modelfabric/internal/inputs"
	"github.com/gregbugaj/modelfabric/internal/mesh"
	"github.com/gregbugaj/modelfabric/internal/ops"
	"github.com/gregbugaj/modelfabric/internal/process"
	"github.com/gregbugaj/modelfabric/internal/runtime"
	"github.com/gregbugaj/modelfabric/internal/tokentap"
)

type Config struct {
	ModelsRoot   string
	LogDir       string
	DataDir      string
	LMStudioRoot string
	ExtraRoots   []string
	// EngineBind is the interface engines listen on. Loopback is the safe
	// default; a mesh-wide scheduler such as llm-d's EPP needs a routable
	// address instead, at which point tailnet ACLs are the only thing in front
	// of the engines.
	EngineBind     string
	SelfAddr       string
	PortMin        int
	PortMax        int
	StartupTimeout time.Duration
	StopTimeout    time.Duration

	// JIT loads a catalog model when no node serves it. Disabled by default.
	JIT bool
	// JITTTL is the idle time after which a JIT-loaded instance is unloaded;
	// zero keeps it until unloaded by hand. A request's "ttl" overrides it.
	JITTTL time.Duration
	// JITAutoEvict unloads other idle JIT-loaded instances before a JIT load,
	// so JIT never stacks models into VRAM. Manual loads are never evicted.
	JITAutoEvict bool
	// Entrypoint is a node that runs no models (config role "entrypoint");
	// loads are refused rather than attempted on a machine with no GPU.
	Entrypoint bool
	// MaxOutputTokens is the ceiling each engine shim fills into a request that
	// states none of its own; zero leaves such requests uncapped.
	MaxOutputTokens int
}

type ShimInfo struct {
	Port int `json:"port"`
	// KVGauge: it publishes the KV-cache use llama.cpp keeps in /slots as a
	// metric, which llm-d's scheduler scrapes.
	KVGauge    bool `json:"kv_gauge"`
	LiveTokens bool `json:"live_tokens"`
	// OutputCeiling is filled into requests that set no limit; 0 is none.
	OutputCeiling int  `json:"output_ceiling"`
	PromptCache   bool `json:"prompt_cache"`
}

// Shim describes the instance's shim, or nil when it has none: engines that
// export their own KV gauge (vLLM, SGLang, mlx-lm) are dialled directly.
func (i Instance) Shim() *ShimInfo {
	if i.shim == nil || i.ShimPort == 0 {
		return nil
	}
	return &ShimInfo{Port: i.ShimPort, KVGauge: true, LiveTokens: i.shim.Watching(),
		OutputCeiling: i.shim.MaxOutputTokens(), PromptCache: i.shim.Caching()}
}

// SlotCache is the disk tier of the prompt cache, as the supervisor needs it:
// told when an engine that can save slots arrives, and given the chance to
// save what one holds before it is stopped.
type SlotCache interface {
	Dir() string
	Attach(name, baseURL, sig string, slots int)
	Detach(ctx context.Context, name string)
	// Place is engineshim.Placer: the shim calls it for each request.
	Place(ctx context.Context, engine string, chain [][32]byte) (slot int, done func(status int))
}

func (c Config) withDefaults() Config {
	if c.EngineBind == "" {
		c.EngineBind = "127.0.0.1"
	}
	if c.PortMin == 0 {
		c.PortMin = 18000
	}
	if c.PortMax == 0 {
		c.PortMax = 18099
	}
	if c.StartupTimeout == 0 {
		c.StartupTimeout = 10 * time.Minute
	}
	if c.StopTimeout == 0 {
		c.StopTimeout = 20 * time.Second
	}
	return c
}

type Instance struct {
	ID    string `json:"id"`
	Model string `json:"model"`
	// Variant identifies the exact catalog weights loaded. Model keys can be
	// shared by different formats or quantizations. Empty on older instances.
	Variant    string          `json:"variant,omitempty"`
	Config     runtime.Applied `json:"config"`
	Port       int             `json:"port"`
	Runtime    string          `json:"runtime"`
	Generation int             `json:"generation"`
	State      string          `json:"state"`
	PID        int             `json:"pid,omitempty"`
	LogPath    string          `json:"log_path,omitempty"`
	StartedAt  time.Time       `json:"started_at"`
	// Origin is "jit" for an instance loaded on demand, else empty (manual).
	Origin string `json:"origin,omitempty"`
	// TTLSeconds unloads the instance after this long without requests;
	// zero means never.
	TTLSeconds int       `json:"ttl,omitempty"`
	LastUsed   time.Time `json:"last_used,omitempty"`

	// ShimPort is where ModelFabric republishes this engine's metrics with a KV
	// gauge it synthesizes (see internal/engineshim). Advertised to llm-d,
	// which scrapes whatever it routes to; zero when the engine needs none.
	ShimPort int `json:"shim_port,omitempty"`

	handle *process.Handle
	engine *mesh.Engine // the router's view: in-flight count and last use
	shim   *engineshim.Shim
}

type Supervisor struct {
	memory  memoryWatch
	cfg     Config
	rts     *runtime.Registry
	lch     *process.Launcher
	journal *ops.Journal
	mesh    *mesh.Mesh
	log     *slog.Logger

	mu        sync.RWMutex
	instances map[string]*Instance // by instance ID
	// tap is where shims publish the replies they carry, so the node running
	// the model can show its own output. nil until the server sets it.
	tap        *tokentap.Tap
	byModel    map[string]string // model key -> instance ID
	unloaded   map[string]bool
	generation map[string]int
	reserved   map[int]bool // ports handed to loads still starting

	cat   *catalog.Catalog
	catMu sync.RWMutex

	// pins make verification meaningful across runs: inputs are prepared once
	// and every later load is checked against that pin.
	pins *inputs.Store

	// slots is nil unless the node configured a disk cache.
	slots SlotCache

	// runtimeErr explains why rt is nil, so a load failure names the real cause.
	runtimeErr string

	stop chan struct{} // closed by Shutdown; ends the idle reaper

	// survey reads this machine's hardware. runtime.Survey, except in tests.
	survey func(context.Context) runtime.Hardware

	// dev is the node's developer log, nil until SetDevLog. See devtail.go.
	dev *devlog.Log
	// live is what each engine is writing per second right now, read from its log.
	live liveGen
}

func (s *Supervisor) SetRuntimeError(err error) {
	if err != nil {
		s.runtimeErr = err.Error()
	}
}

// SetSlotCache enables disk caching for future loads and adopted instances
// launched with slot saving enabled.
func (s *Supervisor) SetSlotCache(c SlotCache) {
	s.slots = c
	for _, inst := range s.Instances() {
		s.attachSlots(&inst)
	}
}

func (s *Supervisor) attachSlots(inst *Instance) {
	if s.slots == nil || inst.Config.SlotSavePath == "" || inst.Config.SlotSavePath != s.slots.Dir() {
		// Launched without it, or pointed at a directory that is no longer
		// the cache's: restores would look for files that are not there.
		return
	}
	// The endpoint the engine was registered at, which for an adopted
	// instance is the one recorded at launch and not today's bind address.
	url := runtime.LocalURL(s.cfg.EngineBind, inst.Port)
	if inst.engine != nil {
		url = inst.engine.BaseURL
	}
	s.slots.Attach(inst.ID, url, slotSig(inst), inst.Config.Parallel)
	// The cache places requests in the engine's shim, the hop every routing
	// method shares, and the router is pointed at the shim so its requests
	// pass that way too. Without a shim the cache saves on unload only.
	if inst.shim != nil && inst.ShimPort > 0 && inst.engine != nil {
		inst.shim.SetCache(inst.ID, s.slots)
		inst.engine.SetVia(runtime.LocalURL(s.cfg.EngineBind, inst.ShimPort))
	}
}

// slotSig includes exact weights, engine build and KV types. Same-shaped
// weights can accept each other's saved state but produce incorrect output.
func slotSig(inst *Instance) string {
	sum := sha256.Sum256([]byte(strings.Join([]string{
		inst.Model, inst.Variant, inst.Runtime, inst.Config.CacheTypeK, inst.Config.CacheTypeV,
	}, "\x00")))
	return hex.EncodeToString(sum[:8])
}

func (s *Supervisor) SetPinStore(store *inputs.Store) { s.pins = store }

// Repin accepts the current on-disk contents of a model as the new pinned
// state. Explicit, because it discards the previous guarantee.
func (s *Supervisor) Repin(ref string) (string, error) {
	model, err := s.Catalog().Resolve(ref)
	if err != nil {
		return "", err
	}
	if s.pins == nil {
		return "", fmt.Errorf("no pin store configured")
	}
	if err := s.pins.Unpin(model.Key); err != nil {
		return "", err
	}
	def, derr := s.rts.DefaultFor(model.Format)
	if derr != nil {
		return "", derr
	}
	if _, _, err := s.prepare(def, model); err != nil {
		return "", err
	}
	return model.Key, nil
}

// New builds a supervisor. rt may be nil when no engine binary is available:
// the catalog still works, so `ls` lists what is on disk and only `load`
// reports the missing runtime.
func New(cfg Config, rts *runtime.Registry, lch *process.Launcher, j *ops.Journal, m *mesh.Mesh, log *slog.Logger) *Supervisor {
	s := &Supervisor{
		cfg:        cfg.withDefaults(),
		rts:        rts,
		lch:        lch,
		journal:    j,
		mesh:       m,
		log:        log,
		instances:  map[string]*Instance{},
		byModel:    map[string]string{},
		unloaded:   map[string]bool{},
		generation: map[string]int{},
		reserved:   map[int]bool{},
		stop:       make(chan struct{}),
		survey:     runtime.Survey,
	}
	s.Rescan()
	if m != nil {
		m.SetInstanceProvider(s.instanceStates)
	}
	s.recover()
	go s.reapIdleLoop()
	return s
}

// instanceMeta is persisted with the ownership record so an adopted process can
// be rebuilt into a full Instance after a restart.
type instanceMeta struct {
	InstanceID string `json:"instance_id"`
	Model      string `json:"model"`
	// Variant is recorded so an adopted instance still knows which weights it
	// loaded; the disk cache's signature depends on it.
	Variant    string          `json:"variant,omitempty"`
	Config     runtime.Applied `json:"config"`
	Port       int             `json:"port"`
	Runtime    string          `json:"runtime"`
	Generation int             `json:"generation"`
	StartedAt  time.Time       `json:"started_at"`
	Origin     string          `json:"origin,omitempty"`
	TTLSeconds int             `json:"ttl,omitempty"`
}

// recover adopts engines that outlived a previous host process.
//
// A node killed rather than stopped leaves llama-server processes holding GPU
// memory. Without adoption they are invisible orphans that the next load then
// competes with for VRAM.
func (s *Supervisor) recover() {
	if s.lch == nil {
		return
	}
	for _, r := range s.lch.RecoverAll() {
		switch r.State {
		case process.StateUnknown:
			// A launch window we cannot resolve. It intentionally blocks
			// further launches for that model until an operator settles it.
			s.log.Warn("unresolved launch window; run `mfsh recover -clear <model>` once you have confirmed no orphan is running")
			continue
		case process.StateStopped:
			continue
		}
		if r.Handle == nil {
			continue
		}

		var meta instanceMeta
		if len(r.Meta) > 0 {
			_ = json.Unmarshal(r.Meta, &meta)
		}
		if meta.InstanceID == "" {
			meta.InstanceID = r.Handle.InstanceID
		}
		if meta.Model == "" {
			meta.Model = r.Handle.Model
		}
		if meta.Port == 0 {
			meta.Port = portFromEndpoint(r.Handle.Endpoint)
		}

		inst := &Instance{
			ID:         meta.InstanceID,
			Model:      meta.Model,
			Variant:    meta.Variant,
			Config:     meta.Config,
			Port:       meta.Port,
			Runtime:    meta.Runtime,
			Generation: meta.Generation,
			State:      "ready",
			PID:        r.Handle.PID,
			StartedAt:  meta.StartedAt,
			Origin:     meta.Origin,
			TTLSeconds: meta.TTLSeconds,
			handle:     r.Handle,
		}
		s.mu.Lock()
		s.instances[inst.ID] = inst
		if _, has := s.byModel[inst.Model]; !has {
			s.byModel[inst.Model] = inst.ID
		}
		if meta.Generation > s.generation[inst.Model] {
			s.generation[inst.Model] = meta.Generation
		}
		s.mu.Unlock()

		if s.mesh != nil {
			// The endpoint recorded at launch, not one recomputed from the
			// current config: the bind address may have changed since.
			url := r.Handle.Endpoint
			if url == "" {
				url = runtime.LocalURL(s.cfg.EngineBind, inst.Port)
			}
			inst.ShimPort, inst.shim = s.startShim(s.engineOf(inst.Runtime), url)
			eng := mesh.NewEngine(inst.ID, url)
			eng.Served = r.Handle.ServedModel
			applyTraits(eng, runtime.EngineTraits(s.engineOf(inst.Runtime)), inst.Config.VisionSkipped, inst.Config.Embedding)
			eng.MarkReady(inst.Model)
			eng.SetSlots(inst.Config.Parallel)
			eng.SetAskedContext(inst.Config.ContextLength)
			s.mesh.RegisterEngine(eng)
			s.mu.Lock()
			inst.engine = eng
			s.mu.Unlock()
		}
		s.log.Info("adopted running instance after restart",
			"model", inst.Model, "instance", inst.ID, "pid", inst.PID)
		go s.watch(inst)
	}
}

func (s *Supervisor) ClearUnresolved(ref string) (string, error) {
	model, err := s.Catalog().Resolve(ref)
	if err != nil {
		// Allow clearing by raw key too, since the model may have been removed.
		return ref, s.lch.ClearUnresolved(ref)
	}
	return model.Key, s.lch.ClearUnresolved(model.Key)
}

func portFromEndpoint(endpoint string) int {
	i := strings.LastIndexByte(endpoint, ':')
	if i < 0 {
		return 0
	}
	p, err := strconv.Atoi(endpoint[i+1:])
	if err != nil {
		return 0
	}
	return p
}

func (s *Supervisor) Rescan() error {
	hub := catalog.LoadHub(s.cfg.LMStudioRoot)
	for _, e := range hub.Entries() {
		if e.SpecErr != nil {
			s.log.Warn("model.yaml not used", "model", e.ID, "err", e.SpecErr)
		}
	}
	c, err := catalog.ScanRootsWithHub(hub, append([]string{s.cfg.ModelsRoot}, s.cfg.ExtraRoots...)...)
	if err != nil {
		return err
	}
	s.catMu.Lock()
	s.cat = c
	s.catMu.Unlock()
	return nil
}

func (s *Supervisor) Catalog() *catalog.Catalog {
	s.catMu.RLock()
	defer s.catMu.RUnlock()
	return s.cat
}

type LoadRequest struct {
	Model   string `json:"model"`
	Runtime string `json:"runtime,omitempty"`
	// Format selects a weights variant; empty uses the catalog's primary variant.
	Format string `json:"format,omitempty"`
	// Settings are this load's own options, inline in the JSON (so
	// context_length, gpu_layers, parallel, flash_attention stay where
	// clients already send them). They override the model's defaults.
	runtime.Settings
	Preset string `json:"preset,omitempty"`
	// AddInstance creates another replica. False reuses a resident instance.
	AddInstance bool `json:"add_instance,omitempty"`
	EchoConfig  bool `json:"echo_load_config,omitempty"`
	// TTL unloads the instance after this many idle seconds; zero means never.
	TTL int `json:"ttl,omitempty"`

	origin string // "jit" when loaded on demand
}

// Load starts a model and returns its operation. The bool is false when
// joining a compatible operation already in flight.
func (s *Supervisor) Load(req LoadRequest) (*ops.Operation, bool, error) {
	if s.cfg.Entrypoint {
		return nil, false, fmt.Errorf("this node is an entrypoint (role %q): it routes requests into the mesh and runs no models; load on a GPU node", "entrypoint")
	}
	if s.rts == nil || s.rts.Len() == 0 {
		return nil, false, fmt.Errorf("cannot load: %s", s.runtimeErr)
	}
	model, err := s.Catalog().ResolveFormat(req.Model, req.Format)
	if err != nil {
		return nil, false, err
	}
	def, err := s.rts.SelectFor(req.Runtime, model.Format)
	if err != nil {
		return nil, false, err
	}

	s.mu.RLock()
	existingID, already := s.byModel[model.Key]
	if already {
		// Two variants of one model share a key, so "already resident" has to
		// mean the same weights and not merely the same name. Asking for the
		// MLX build while the GGUF is loaded used to hand back the GGUF, which
		// made the other variant unloadable on that machine.
		if inst, ok := s.instances[existingID]; ok && inst.Variant != "" && inst.Variant != model.PathKey {
			already = false
		}
	}
	s.mu.RUnlock()
	if already && !req.AddInstance {
		op, _ := s.journal.Begin("load", model.Key, "resident:"+existingID)
		s.journal.Succeed(op.ID, existingID)
		settled, _ := s.journal.Get(op.ID)
		return settled, false, nil
	}

	settings, err := s.resolveSettings(model.Key, req.Preset, req.Settings)
	if err != nil {
		return nil, false, err
	}
	draftPath := ""
	if settings.DraftModel != nil && *settings.DraftModel != "" {
		dm, err := s.Catalog().Resolve(*settings.DraftModel)
		if err != nil {
			return nil, false, fmt.Errorf("draft model: %w", err)
		}
		if dm.Key == model.Key {
			return nil, false, fmt.Errorf("draft model: a model cannot draft for itself")
		}
		// The draft model runs in the engine too, so it is verified like
		// the model: pinned on first use, checked on every later load.
		if s.pins != nil {
			if _, _, err := s.pins.Prepare(dm.Key, parentDir(dm.Path), []string{dm.Path}, ""); err != nil {
				return nil, false, fmt.Errorf("draft model: %w", err)
			}
		}
		draftPath = dm.Path
	}
	applied, err := runtime.Apply(def, model, runtime.Requested{Settings: settings, DraftModelPath: draftPath})
	if err != nil {
		return nil, false, err
	}
	// A GPU that is not there would otherwise surface as an engine that
	// starts and offloads nothing, or does not start, a minute later.
	if settings.GPU != nil && *settings.GPU != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
		err := runtime.CheckGPUs(def, *settings.GPU, s.survey(ctx))
		cancel()
		if err != nil {
			return nil, false, err
		}
	}
	// llama-server refuses slot save and restore once a projector is loaded,
	// so a vision instance is left out rather than given a cache that would
	// fail on first use.
	// An embedding engine has no conversation to save.
	if s.slots != nil && def.Engine == mesh.EngineLlamaCPP && !applied.Vision && !applied.Embedding {
		applied.SlotSavePath = s.slots.Dir()
	}
	// A replica is new work by definition, so it must never join another
	// operation - two "add a replica" calls mean two replicas.
	dedupe := "load:" + model.Key + ":" + applied.Fingerprint()
	if req.AddInstance {
		dedupe += ":replica:" + newInstanceID()
	}
	op, created := s.journal.Begin("load", model.Key, dedupe)
	if !created {
		return op, false, nil
	}

	s.mu.Lock()
	delete(s.unloaded, model.Key) // an explicit load clears the stop intent
	// A replica joins the current generation instead of starting a new one:
	// bumping it would mark every in-flight sibling load as superseded.
	if !req.AddInstance || s.generation[model.Key] == 0 {
		s.generation[model.Key]++
	}
	generation := s.generation[model.Key]
	s.mu.Unlock()

	go s.runLoad(op.ID, def, model, applied, generation, req.AddInstance, req.origin, req.TTL)
	return op, true, nil
}

func (s *Supervisor) runLoad(opID string, def *runtime.Definition, model catalog.Model, applied runtime.Applied, generation int, replica bool, origin string, ttl int) {
	ctx, cancel := context.WithTimeout(context.Background(), s.cfg.StartupTimeout)
	defer cancel()

	fail := func(err error) {
		s.log.Error("load failed", "model", model.Key, "err", err)
		s.said(devlog.Error, model.Key, "", "load failed: "+err.Error(), nil)
		s.journal.Fail(opID, err)
	}

	// prepare revalidates inputs immediately before spawn; repeating it here
	// would rehash the same files.
	s.journal.Progress(opID, "verifying model inputs")
	if _, _, err := s.prepare(def, model); err != nil {
		fail(err)
		return
	}

	port, err := s.freePort()
	if err != nil {
		fail(err)
		return
	}
	// The reservation outlives this function only if the instance is
	// published; otherwise the port goes back to the pool.
	defer s.releasePort(port)

	instanceID := newInstanceID()
	spec, err := runtime.LaunchSpec(def, model, applied, s.cfg.EngineBind, port, generation, s.cfg.StartupTimeout, s.cfg.StopTimeout)
	if err != nil {
		fail(err)
		return
	}
	// The launcher allows one owned process per deployment, which is right for
	// the first copy (a second concurrent plain load must be refused) but
	// wrong for replicas: each is its own deployment.
	if replica {
		spec.DeploymentID = model.Key + "#" + instanceID
	}
	if s.cfg.LogDir != "" {
		if err := os.MkdirAll(s.cfg.LogDir, 0o755); err == nil {
			path := filepath.Join(s.cfg.LogDir, instanceID+".log")
			spec.StdoutPath, spec.StderrPath = path, path
		}
	}
	if meta, err := json.Marshal(instanceMeta{
		InstanceID: instanceID, Model: model.Key, Variant: model.PathKey, Config: applied, Port: port,
		Runtime: def.Name, Generation: generation, StartedAt: time.Now().UTC(),
		Origin: origin, TTLSeconds: ttl,
	}); err == nil {
		spec.Meta = meta
	}

	s.journal.Progress(opID, "starting engine")
	s.log.Info("loading model", "model", model.Key, "port", port, "generation", generation)
	loadStarted := time.Now()
	s.said(devlog.Info, model.Key, instanceID, "loading model", map[string]any{"runtime": def.Name, "port": port})

	// What is being read in: the model, and its projector when it is loaded
	// to see images.
	weights := model.SizeBytes
	if applied.Vision {
		weights += fileSize(model.Projector)
	}
	loaded := make(chan struct{})
	go s.reportLoad(opID, spec.DeploymentID, weights, loaded)
	handle, err := s.lch.Start(ctx, spec, instanceID)
	close(loaded)
	if err != nil {
		fail(err)
		return
	}

	inst := &Instance{
		ID:         instanceID,
		Model:      model.Key,
		Variant:    model.PathKey,
		Config:     applied,
		Port:       port,
		Runtime:    def.Name,
		Generation: generation,
		State:      "ready",
		PID:        handle.PID,
		LogPath:    spec.StdoutPath,
		StartedAt:  time.Now().UTC(),
		Origin:     origin,
		TTLSeconds: ttl,
		handle:     handle,
	}
	// Publish the engine's metrics with a KV gauge for schedulers that scrape
	// it, before the instance is routable.
	inst.ShimPort, inst.shim = s.startShim(def.Engine, spec.Endpoint)

	s.mu.Lock()
	// A newer generation started while we were loading: this instance is
	// already stale, so stop it rather than publishing it.
	if s.generation[model.Key] != generation || s.unloaded[model.Key] {
		s.mu.Unlock()
		_ = s.lch.Stop(context.Background(), handle, s.cfg.StopTimeout)
		fail(fmt.Errorf("load superseded before it became ready"))
		return
	}
	s.instances[instanceID] = inst
	if _, has := s.byModel[model.Key]; !has {
		s.byModel[model.Key] = instanceID
	}
	s.mu.Unlock()

	// Seed the engine as healthy so it is routable immediately rather than
	// after the next mesh poll.
	if s.mesh != nil {
		eng := mesh.NewEngine(instanceID, spec.Endpoint)
		eng.Served = spec.ServedModel
		applyTraits(eng, runtime.EngineTraits(def.Engine), applied.VisionSkipped, applied.Embedding)
		eng.MarkReady(model.Key)
		eng.SetSlots(applied.Parallel)
		eng.SetAskedContext(applied.ContextLength)
		s.mesh.RegisterEngine(eng)
		s.mu.Lock()
		inst.engine = eng
		s.mu.Unlock()
	}

	s.attachSlots(inst)

	s.log.Info("model ready", "model", model.Key, "instance", instanceID, "pid", handle.PID)
	took := time.Since(loadStarted).Round(100 * time.Millisecond)
	s.said(devlog.Info, model.Key, instanceID, fmt.Sprintf("model ready in %s", took),
		map[string]any{"ms": took.Milliseconds(), "pid": handle.PID})
	s.journal.Succeed(opID, instanceID)
	go s.watch(inst)
}

// watch withdraws engines that exit unexpectedly so requests stop reaching
// dead ports. Intentional unloads remove the instance before stopping it.
// Exited engines are not automatically restarted.
func (s *Supervisor) watch(inst *Instance) {
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		select {
		case <-s.stop:
			cancel()
		case <-ctx.Done():
		}
	}()
	how := s.lch.Wait(ctx, inst.handle)
	cancel()
	if how == "" {
		return
	}
	s.mu.Lock()
	if s.instances[inst.ID] != inst {
		s.mu.Unlock()
		return
	}
	delete(s.instances, inst.ID)
	if s.byModel[inst.Model] == inst.ID {
		delete(s.byModel, inst.Model)
		for id, other := range s.instances {
			if other.Model == inst.Model {
				s.byModel[inst.Model] = id
				break
			}
		}
	}
	s.mu.Unlock()
	s.stopShim(inst)
	if s.mesh != nil {
		s.mesh.UnregisterEngine(inst.ID)
	}
	_ = s.lch.Stop(context.Background(), inst.handle, s.cfg.StopTimeout) // clears the ownership record
	hint := "see the engine log"
	if strings.Contains(how, "killed") {
		hint = "SIGKILL is usually the kernel OOM killer (host RAM, not VRAM): check `journalctl -k`, and lower cache_ram or ctx_checkpoints"
	}
	s.log.Error("engine exited unexpectedly; instance withdrawn",
		"model", inst.Model, "instance", inst.ID, "pid", inst.PID, "exit", how, "log", inst.LogPath, "hint", hint)
	s.said(devlog.Error, inst.Model, inst.ID, fmt.Sprintf("engine exited unexpectedly (%s); %s", how, hint),
		map[string]any{"exit": how, "log": inst.LogPath})
}

// retainStuck puts an instance back after its engine refused to stop.
//
// It goes into the instance table only: byModel and the mesh stay as unload
// left them, so `mfsh ps` lists the process that is still running and another
// unload can retry it, while nothing routes to an engine that would not die.
// A newer instance that has taken the same id is never displaced.
func (s *Supervisor) retainStuck(instanceID string, inst *Instance) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, replaced := s.instances[instanceID]; !replaced {
		s.instances[instanceID] = inst
	}
}

func (s *Supervisor) prepare(def *runtime.Definition, model catalog.Model) (inputs.PreparedInputs, inputs.Manifests, error) {
	files := []string{model.Path}
	if model.Projector != "" {
		files = append(files, model.Projector)
	}
	root := parentDir(model.Path)

	var (
		mv       *inputs.VerifiedInput
		manifest []byte
		err      error
	)
	if s.pins != nil {
		mv, manifest, err = s.pins.Prepare(model.Key, root, files, "")
	} else {
		mv, manifest, err = inputs.VerifyTree(root, files, "")
	}
	if err != nil {
		return inputs.PreparedInputs{}, nil, err
	}
	rv, rmanifest, err := def.Verify()
	if err != nil {
		return inputs.PreparedInputs{}, nil, err
	}
	prepared := inputs.PreparedInputs{Runtime: rv, Model: *mv}
	manifests := inputs.Manifests{
		mv.ManifestSHA256: manifest,
		rv.ManifestSHA256: rmanifest,
	}
	return prepared, manifests, prepared.Validate()
}

// Unload stops an instance by exact ID; stale IDs cannot stop replacements.
func (s *Supervisor) Unload(instanceID string) (*ops.Operation, error) {
	return s.unload(instanceID, 0)
}

// unload removes an instance from routing and stops it. With drain > 0, it
// first waits up to drain for accepted requests to finish.
func (s *Supervisor) unload(instanceID string, drain time.Duration) (*ops.Operation, error) {
	s.mu.Lock()
	inst, ok := s.instances[instanceID]
	if !ok {
		s.mu.Unlock()
		return nil, fmt.Errorf("no loaded instance %q", instanceID)
	}
	delete(s.instances, instanceID)
	if s.byModel[inst.Model] == instanceID {
		delete(s.byModel, inst.Model)
		for id, other := range s.instances {
			if other.Model == inst.Model {
				s.byModel[inst.Model] = id
				break
			}
		}
	}
	// Record stop intent only when the model is gone entirely. Unloading one
	// replica must not abort a sibling replica that is still loading.
	if _, stillResident := s.byModel[inst.Model]; !stillResident {
		s.unloaded[inst.Model] = true
	}
	s.mu.Unlock()
	s.stopShim(inst)

	op, _ := s.journal.Begin("unload", inst.Model, "unload:"+instanceID)

	if s.mesh != nil {
		s.mesh.UnregisterEngine(instanceID)
	}
	if drain > 0 && inst.engine != nil {
		deadline := time.Now().Add(drain)
		for inst.engine.Inflight() > 0 && time.Now().Before(deadline) {
			time.Sleep(100 * time.Millisecond)
		}
	}
	// Save slots after draining and before stopping the process destroys them.
	if s.slots != nil {
		s.journal.Progress(op.ID, "saving prompt cache to disk")
		s.slots.Detach(context.Background(), instanceID)
	}
	if err := s.lch.Stop(context.Background(), inst.handle, s.cfg.StopTimeout); err != nil {
		// Retain failed stops for inspection and retry, but exclude them from
		// byModel and the mesh so no requests route to them.
		s.retainStuck(instanceID, inst)
		s.log.Error("engine did not stop; it is still running and still listed",
			"model", inst.Model, "instance", instanceID, "pid", inst.PID, "err", err)
		s.journal.Fail(op.ID, err)
		settled, _ := s.journal.Get(op.ID)
		return settled, err
	}
	s.log.Info("model unloaded", "model", inst.Model, "instance", instanceID)
	s.said(devlog.Info, inst.Model, instanceID, "model unloaded", nil)
	s.journal.Succeed(op.ID, instanceID)
	settled, _ := s.journal.Get(op.ID)
	return settled, nil
}

func (s *Supervisor) Instances() []Instance {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Instance, 0, len(s.instances))
	for _, i := range s.instances {
		c := *i
		c.LastUsed = i.lastUsed()
		out = append(out, c)
	}
	sort.Slice(out, func(a, b int) bool {
		if out[a].Model != out[b].Model {
			return out[a].Model < out[b].Model
		}
		return out[a].ID < out[b].ID
	})
	return out
}

func (s *Supervisor) instanceStates() []mesh.InstanceState {
	// Copy each instance with its engine pointer under one lock. Joining an
	// earlier engine scan to a later instance scan could report a replacement
	// with its predecessor's measurements.
	instances := s.Instances()
	out := make([]mesh.InstanceState, 0, len(instances))
	for _, i := range instances {
		var stats mesh.EngineStats
		var served string
		if i.engine != nil {
			stats = i.engine.Stats()
			served = i.engine.Served
		}
		// An engine that has not answered /props still knows the context it
		// was loaded with; zero would read as unknown rather than unconfirmed.
		stats.ContextLength = orElse(stats.ContextLength, i.Config.ContextLength)
		out = append(out, mesh.InstanceState{
			ID:          i.ID,
			Model:       i.Model,
			Source:      i.Model,
			Runtime:     i.Runtime,
			Engine:      s.engineOf(i.Runtime),
			ServedModel: served,
			MetricsPort: i.ShimPort,
			EngineStats: stats,
			Address:     s.advertisedAddr(),
			Port:        i.Port,
			Slots:       i.Config.Parallel,
			Vision:      i.Config.Vision,
			VisionOff:   i.Config.VisionSkipped != "",
			State:       i.State,
			PID:         i.PID,
			Started:     i.StartedAt,
		})
		st := &out[len(out)-1]
		st.CacheRAMMiB = i.Config.CacheRAMMiB
		st.GPU = i.Config.GPU
		st.GPUMemory = s.memory.gpu(i.PID)
		if v, ok := s.live.rate(i.ID, time.Now()); ok {
			st.OutputNowTokS = &v
		}
		st.CacheDropped, st.MemoryMB = s.memory.read(i.ID, i.LogPath, i.PID)
	}
	return out
}

// SetTokenTap gives the shims somewhere to publish the replies they carry.
// Set by the server, which owns the tap; nil leaves them silent.
func (s *Supervisor) SetTokenTap(t *tokentap.Tap) {
	s.tap = t
	// Shims already running were built before the server existed.
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, inst := range s.instances {
		if inst.shim != nil {
			inst.shim.Watch(t)
		}
	}
}

// startShim republishes an engine's metrics with the KV gauge ModelFabric
// synthesizes, on its own port beside the engine. Only engines that hold the
// number without exporting it need one; an engine that exports its own is
// scraped directly, and a failure here costs the KV gauge, never the load.
func (s *Supervisor) startShim(engine, endpoint string) (int, *engineshim.Shim) {
	if !runtime.EngineTraits(engine).KVUsageFromSlots {
		return 0, nil
	}
	sh, err := engineshim.New(endpoint, s.log)
	if err != nil {
		s.log.Warn("no metrics shim for this engine", "endpoint", endpoint, "err", err)
		return 0, nil
	}
	// llm-d dials the shim directly, so token capture must also run here.
	sh.Watch(s.tap)
	// Apply output limits here because llm-d bypasses the router.
	sh.SetMaxOutputTokens(s.cfg.MaxOutputTokens)
	port, err := s.freePort()
	if err != nil {
		s.log.Warn("no port for the metrics shim", "err", err)
		return 0, nil
	}
	bind := s.cfg.EngineBind
	if bind == "" {
		bind = "127.0.0.1"
	}
	if _, err := sh.Listen(net.JoinHostPort(bind, strconv.Itoa(port))); err != nil {
		s.releasePort(port)
		s.log.Warn("metrics shim did not start", "endpoint", endpoint, "err", err)
		return 0, nil
	}
	// The bound listener now reserves the port. Releasing the reservation
	// avoids acquiring s.mu during shim teardown, which previously deadlocked.
	s.releasePort(port)
	s.log.Info("engine metrics shim up", "engine", endpoint, "port", port)
	return port, sh
}

// stopShim closes an instance's shim. It takes no lock, so it is safe from
// anywhere; the port frees itself when the listener closes.
func (s *Supervisor) stopShim(inst *Instance) {
	if inst == nil || inst.shim == nil {
		return
	}
	_ = inst.shim.Close()
	inst.shim = nil
}

// applyTraits tells the mesh what this engine cannot be asked. visionSkipped
// is set only when the model has a projector this instance declined: it
// answers to a multimodal name and fails any request carrying an image. A
// model with no projector leaves it empty and is not treated as having lost
// anything.
func applyTraits(eng *mesh.Engine, t runtime.Traits, visionSkipped string, embedding bool) {
	eng.Embedding = embedding
	eng.FixedModels, eng.NoMetrics = t.FixedModels, t.NoMetrics
	eng.KVFromSlots = t.KVUsageFromSlots
	eng.NoConstrainedDecoding = t.NoConstrainedDecoding
	eng.NoVision = visionSkipped != ""
}

// engineOf is the engine family behind a runtime name, empty when unknown.
func (s *Supervisor) engineOf(runtimeName string) string {
	if s.rts == nil || runtimeName == "" {
		return ""
	}
	if d, ok := s.rts.Lookup(runtimeName); ok {
		return d.Engine
	}
	return ""
}

// advertisedAddr returns the engine bind address. Loopback-bound engines
// advertise 127.0.0.1 and are unreachable from peers.
func (s *Supervisor) advertisedAddr() string {
	if s.cfg.EngineBind == "127.0.0.1" || s.cfg.EngineBind == "localhost" {
		return "127.0.0.1"
	}
	if s.cfg.EngineBind != "0.0.0.0" && s.cfg.EngineBind != "" {
		return s.cfg.EngineBind
	}
	if s.cfg.SelfAddr != "" {
		return s.cfg.SelfAddr
	}
	return "127.0.0.1"
}

func (s *Supervisor) Runtimes() *runtime.Registry { return s.rts }

func (s *Supervisor) Shutdown() {
	select {
	case <-s.stop:
	default:
		close(s.stop)
	}
	for _, i := range s.Instances() {
		if _, err := s.Unload(i.ID); err != nil {
			s.log.Error("shutdown unload failed", "instance", i.ID, "err", err)
		}
	}
}

// freePort finds an unused port in the configured range by binding it. The
// bind is released immediately, so there is a small race - the engine's own
// bind failure is the authoritative check.
func (s *Supervisor) freePort() (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	used := map[int]bool{}
	for _, i := range s.instances {
		used[i.Port] = true
	}
	// Ports handed to loads still starting. Without this, two concurrent loads
	// both find the same port free and the second engine fails to bind.
	for p := range s.reserved {
		used[p] = true
	}

	for p := s.cfg.PortMin; p <= s.cfg.PortMax; p++ {
		if used[p] {
			continue
		}
		// Probe the interface the engine will actually bind; a port free on
		// loopback can be taken on a specific address, and vice versa.
		host := s.cfg.EngineBind
		if host == "" || host == "localhost" {
			host = "127.0.0.1"
		}
		ln, err := net.Listen("tcp", net.JoinHostPort(host, strconv.Itoa(p)))
		if err != nil {
			continue
		}
		_ = ln.Close()
		s.reserved[p] = true
		return p, nil
	}
	return 0, fmt.Errorf("no free port in range %d-%d", s.cfg.PortMin, s.cfg.PortMax)
}

// releasePort drops a reservation. A published instance keeps the port marked
// as used through the instances map, so releasing is always safe.
func (s *Supervisor) releasePort(p int) {
	s.mu.Lock()
	delete(s.reserved, p)
	s.mu.Unlock()
}

func parentDir(path string) string {
	for i := len(path) - 1; i >= 0; i-- {
		if path[i] == '/' {
			return path[:i]
		}
	}
	return "."
}

func newInstanceID() string {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("inst-%d", time.Now().UnixNano())
	}
	return "inst-" + hex.EncodeToString(b[:])
}

func (s *Supervisor) Journal() *ops.Journal { return s.journal }

// ModelsRoot is where this node keeps ModelFabric's own models: downloads land
// here, never in LM Studio's tree.
func (s *Supervisor) ModelsRoot() string { return s.cfg.ModelsRoot }

func (s *Supervisor) Entrypoint() bool { return s.cfg.Entrypoint }

func orElse(v, fallback int) int {
	if v != 0 {
		return v
	}
	return fallback
}
