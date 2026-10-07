package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/gregbugaj/modelfabric/internal/catalog"
	"github.com/gregbugaj/modelfabric/internal/engineshim"
)

// Requested is what a caller asked for. Zero values mean "use the default".
type Requested struct {
	// Settings are the merged options: per-model defaults and presets under
	// the load request's own. Nil fields fall through to model.yaml and the
	// runtime's defaults.
	Settings Settings
	// DraftModelPath is Settings.DraftModel resolved to a file by the caller,
	// which owns the catalog.
	DraftModelPath string
	// Speculative overrides the automatic choice; nil means "use the draft
	// head if the model has one". Kept for callers predating SpecMode.
	Speculative *bool
}

type Applied struct {
	Runtime       string `json:"runtime"`
	ContextLength int    `json:"context_length"`
	GPULayers     int    `json:"gpu_layers"`
	Parallel      int    `json:"parallel"`
	SlotsClamped  string `json:"slots_clamped,omitempty"`
	// VisionSkipped records a declined projector so routing excludes image
	// requests. Empty for models without a projector.
	VisionSkipped  string `json:"vision_skipped,omitempty"`
	FlashAttention bool   `json:"flash_attention"`
	Vision         bool   `json:"vision"`
	Embedding      bool   `json:"embedding,omitempty"`
	// Speculative reports whether speculative decoding is on; SpecType says
	// how: draft-mtp (the model's own head) or draft-simple (a draft model).
	Speculative bool `json:"speculative"`
	// Reasoning, ReasoningEffort and ReasoningBudget are llama.cpp's thinking
	// controls. Empty and nil mean the engine's own default, which is the
	// model template's.
	Reasoning       string  `json:"reasoning,omitempty"`
	ReasoningEffort string  `json:"reasoning_effort,omitempty"`
	ReasoningBudget *int    `json:"reasoning_budget,omitempty"`
	SpecType        string  `json:"spec_type,omitempty"`
	DraftModel      string  `json:"draft_model,omitempty"`
	DraftMax        int     `json:"draft_max,omitempty"`
	DraftMin        int     `json:"draft_min,omitempty"`
	DraftPMin       float64 `json:"draft_p_min,omitempty"`
	DraftGPULayers  *int    `json:"draft_gpu_layers,omitempty"`
	BatchSize       int     `json:"batch_size,omitempty"`
	UBatchSize      int     `json:"ubatch_size,omitempty"`
	CacheTypeK      string  `json:"cache_type_k,omitempty"`
	CacheTypeV      string  `json:"cache_type_v,omitempty"`
	LoadMode        string  `json:"load_mode,omitempty"`
	KVUnified       bool    `json:"kv_unified"`
	KVOffload       bool    `json:"kv_offload"`
	CacheReuse      int     `json:"cache_reuse,omitempty"`
	// CacheRAMMiB caps llama-server's host-RAM prompt cache; always set,
	// since 0 means off (see fitCacheRAM).
	CacheRAMMiB    int `json:"cache_ram_mib"`
	CtxCheckpoints int `json:"ctx_checkpoints,omitempty"`
	// SlotSavePath is where the engine may save and restore slots, for the
	// disk tier of the prompt cache (internal/slotcache). Empty leaves the
	// engine's slot actions disabled. Set by the supervisor from node
	// configuration, never from a request.
	SlotSavePath  string  `json:"slot_save_path,omitempty"`
	NCPUMoE       int     `json:"n_cpu_moe,omitempty"`
	RopeFreqBase  float64 `json:"rope_freq_base,omitempty"`
	RopeFreqScale float64 `json:"rope_freq_scale,omitempty"`
	Threads       int     `json:"threads,omitempty"`
	ThreadsBatch  int     `json:"threads_batch,omitempty"`
	Seed          *int    `json:"seed,omitempty"`
	// KVCacheTokens is the engine's total KV capacity: ContextLength for every
	// parallel slot at once. ContextLength itself is the per-request limit.
	KVCacheTokens int `json:"kv_cache_tokens"`
	// Sampling and TemplateKwargs are the engine's defaults for requests
	// that do not set their own: model.yaml's recommendations, overridden by
	// per-model settings and presets. A request still wins.
	Sampling       *catalog.Sampling `json:"sampling,omitempty"`
	TemplateKwargs map[string]any    `json:"chat_template_kwargs,omitempty"`
	ExtraArgs      []string          `json:"extra_args,omitempty"`
}

// Fingerprint identifies a configuration for operation de-duplication: two
// loads with the same fingerprint are the same work. Every applied field
// counts; encoding/json sorts map keys, so it is deterministic.
func (a Applied) Fingerprint() string {
	b, err := json.Marshal(a)
	if err != nil {
		// TemplateKwargs is map[string]any, so a value that cannot be encoded
		// (a non-finite float, say) lands here. Hashing nil would give every
		// such configuration the same fingerprint, and the fingerprint is what
		// decides whether two loads are the same work.
		return "unencodable-" + hex.EncodeToString([]byte(err.Error()))[:16]
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8])
}

func (llamaCPP) applyKV(a Applied) int {
	tokens := int64(a.ContextLength) * int64(max(a.Parallel, 1))
	if tokens > maxKVCacheTokens {
		return maxKVCacheTokens
	}
	return int(tokens)
}

// maxKVCacheTokens caps -c below the multiplication overflow limit.
const maxKVCacheTokens = 1 << 40

type engineAdapter interface {
	// Argv builds the command line. It never sees an inference request: every
	// value comes from the catalog or trusted configuration.
	Argv(d *Definition, m catalog.Model, a Applied, bind string, port int) []string
	Apply(d *Definition, m catalog.Model, req Requested) Applied
	// ServedModel is the id this engine answers to for a model: what its own
	// /v1/models reports, and what a forwarded request's "model" field must
	// say. It is the catalog key wherever the engine can be told to use it.
	ServedModel(m catalog.Model) string
	Ready(ctx context.Context, endpoint, served string) error
	Traits() Traits
}

// Traits describe engine capabilities. The zero value describes llama.cpp;
// adapters state only where they differ.
type Traits struct {
	// FixedModels means the engine's /v1/models does not report ModelFabric's model
	// ids, so the list it was launched with stands and that endpoint says only
	// whether it is alive.
	FixedModels bool
	// NoMetrics means the engine serves no Prometheus /metrics, so its prefill
	// rate cannot be measured from counters.
	NoMetrics bool
	// KVUsageMetric names the engine's KV utilization gauge, or is empty.
	// KVUsageFromSlots requires a shim to synthesize the gauge from /slots
	// for scrapers such as llm-d. Engines exporting a gauge need no shim.
	KVUsageMetric    string
	KVUsageFromSlots bool
	// NoConstrainedDecoding means the engine ignores response_format and grammar.
	// mlx-lm accepts these requests but returns unconstrained output, so routing
	// must select another engine.
	NoConstrainedDecoding bool
}

// EngineTraits are the traits of an engine family; the zero value for one ModelFabric
// does not know, which is llama.cpp's behaviour.
func EngineTraits(engine string) Traits {
	eng, ok := engines[engine]
	if !ok {
		return Traits{}
	}
	return eng.Traits()
}

var engines = map[string]engineAdapter{
	"llama.cpp": llamaCPP{},
	"mlx":       mlxLM{},
}

type llamaCPP struct{}

func (llamaCPP) Apply(d *Definition, m catalog.Model, req Requested) Applied {
	// Skipping the projector also skips vision-specific slot defaults.
	vision := m.Projector != "" && (req.Settings.Vision == nil || *req.Settings.Vision)
	visionSkipped := ""
	if m.Projector != "" && !vision {
		visionSkipped = "loaded text-only, so the projector is not in memory and images cannot be read"
	}
	a := Applied{
		Runtime:        d.Name,
		ContextLength:  d.ContextLength,
		GPULayers:      d.GPULayers,
		Parallel:       d.Parallel,
		FlashAttention: d.FlashAttention,
		Vision:         vision,
		VisionSkipped:  visionSkipped,
		DraftMax:       d.DraftMax,
		BatchSize:      d.BatchSize,
		UBatchSize:     d.UBatchSize,
		CacheTypeK:     d.CacheTypeK,
		CacheTypeV:     d.CacheTypeV,
		LoadMode:       d.LoadMode,
		KVUnified:      d.KVUnified != nil && *d.KVUnified,
		KVOffload:      true,
		CtxCheckpoints: d.CtxCheckpoints,
		Threads:        d.Threads,
	}
	if m.Spec != nil {
		if !m.Spec.Sampling.IsZero() {
			sp := m.Spec.Sampling
			a.Sampling = &sp
		}
		if len(m.Spec.TemplateVars) > 0 {
			a.TemplateKwargs = map[string]any{}
			for k, v := range m.Spec.TemplateVars {
				a.TemplateKwargs[k] = v
			}
		}
	}

	s := req.Settings
	setInt := func(dst *int, v *int) {
		if v != nil {
			*dst = *v
		}
	}
	if s.Reasoning != nil {
		a.Reasoning = *s.Reasoning
	}
	if s.ReasoningEffort != nil {
		a.ReasoningEffort = *s.ReasoningEffort
	}
	if s.ReasoningBudget != nil {
		v := *s.ReasoningBudget
		a.ReasoningBudget = &v
	}
	setInt(&a.ContextLength, s.ContextLength)
	setInt(&a.Parallel, s.Parallel)
	setInt(&a.GPULayers, s.GPULayers)
	if s.OffloadRatio != nil && m.Layers > 0 {
		// LM Studio's ratio of layers on the GPU. llama.cpp counts the
		// output layer too, so a full ratio means every layer.
		a.GPULayers = int(float64(m.Layers)**s.OffloadRatio + 0.5)
		if *s.OffloadRatio >= 1 {
			a.GPULayers = m.Layers + 1
		}
	}
	if s.FlashAttention != nil {
		a.FlashAttention = *s.FlashAttention
	}
	setInt(&a.BatchSize, s.BatchSize)
	setInt(&a.UBatchSize, s.UBatchSize)
	if s.CacheTypeK != nil {
		a.CacheTypeK = *s.CacheTypeK
	}
	if s.CacheTypeV != nil {
		a.CacheTypeV = *s.CacheTypeV
	}
	if s.KVUnified != nil {
		a.KVUnified = *s.KVUnified
	}
	if s.KVOffload != nil {
		a.KVOffload = *s.KVOffload
	}
	setInt(&a.CacheReuse, s.CacheReuse)
	setInt(&a.CtxCheckpoints, s.CtxCheckpoints)
	a.CacheRAMMiB = fitCacheRAM()
	setInt(&a.CacheRAMMiB, s.CacheRAM)
	setInt(&a.NCPUMoE, s.NCPUMoE)
	if s.CPUMoERatio != nil && m.Layers > 0 {
		a.NCPUMoE = int(float64(m.Layers)**s.CPUMoERatio + 0.5)
	}
	if s.RopeFreqBase != nil {
		a.RopeFreqBase = *s.RopeFreqBase
	}
	if s.RopeFreqScale != nil {
		a.RopeFreqScale = *s.RopeFreqScale
	}
	setInt(&a.Threads, s.Threads)
	setInt(&a.ThreadsBatch, s.ThreadsBatch)
	if s.Seed != nil {
		v := *s.Seed
		a.Seed = &v
	}

	// Memory: keep_in_memory and try_mmap map onto llama.cpp's --load-mode,
	// and mlock is still dropped where it cannot hold (see fitLoadMode).
	if s.KeepInMemory != nil || s.TryMmap != nil {
		mmap := s.TryMmap == nil || *s.TryMmap
		lock := s.KeepInMemory == nil && strings.Contains(a.LoadMode, "mlock") ||
			s.KeepInMemory != nil && *s.KeepInMemory
		switch {
		case mmap && lock:
			a.LoadMode = "mmap+mlock"
		case mmap:
			a.LoadMode = "mmap"
		case lock:
			a.LoadMode = "mlock"
		default:
			a.LoadMode = "none"
		}
	}
	a.LoadMode = fitLoadMode(a.LoadMode, m.SizeBytes)

	// Speculative decoding: auto uses the model's own MTP head when it has
	// one, else a draft model when one is configured.
	mode := "auto"
	if s.SpecMode != nil {
		mode = *s.SpecMode
	}
	if req.Speculative != nil && s.SpecMode == nil && !*req.Speculative {
		mode = "off"
	}
	autoSpec := mode == "auto"
	// Auto speculation uses an available MTP head for both text and vision.
	// Set spec_mode off for builds with the image-drafting mtmd chunk defect.
	// Vision defaults to one slot to reduce KV allocation (-c = context × slots);
	// the vision encoder processes images serially. Explicit parallel overrides it.
	if vision && s.Parallel == nil && a.Parallel > 1 {
		a.SlotsClamped = fmt.Sprintf("vision: %d slots would cost %d× the KV cache for work the image encoder serialises anyway", a.Parallel, a.Parallel)
		a.Parallel = 1
	}
	switch {
	case mode == "off":
	case (autoSpec || mode == "mtp") && m.DraftLayers > 0:
		a.Speculative, a.SpecType = true, "draft-mtp"
	case (autoSpec || mode == "draft") && req.DraftModelPath != "":
		a.Speculative, a.SpecType, a.DraftModel = true, "draft-simple", req.DraftModelPath
	}
	if a.Speculative {
		setInt(&a.DraftMax, s.DraftMax)
		setInt(&a.DraftMin, s.DraftMin)
		if s.DraftPMin != nil {
			a.DraftPMin = *s.DraftPMin
		}
		if s.DraftGPULayers != nil {
			v := *s.DraftGPULayers
			a.DraftGPULayers = &v
		}
	} else {
		a.DraftMax = 0
	}

	sp := catalog.Sampling{}
	if a.Sampling != nil {
		sp = *a.Sampling
	}
	for _, f := range []struct {
		dst **float64
		v   *float64
	}{{&sp.Temperature, s.Temperature}, {&sp.TopP, s.TopP}, {&sp.MinP, s.MinP},
		{&sp.RepeatPenalty, s.RepeatPenalty}, {&sp.PresencePenalty, s.PresencePenalty},
		{&sp.FrequencyPenalty, s.FrequencyPenalty}} {
		if f.v != nil {
			v := *f.v
			*f.dst = &v
		}
	}
	if s.TopK != nil {
		v := *s.TopK
		sp.TopK = &v
	}
	if !sp.IsZero() {
		a.Sampling = &sp
	}
	if s.EnableThinking != nil || len(s.TemplateKwargs) > 0 {
		kw := map[string]any{}
		for k, v := range a.TemplateKwargs {
			kw[k] = v
		}
		for k, v := range s.TemplateKwargs {
			kw[k] = v
		}
		if s.EnableThinking != nil {
			kw["enable_thinking"] = *s.EnableThinking
		}
		a.TemplateKwargs = kw
	}
	a.ExtraArgs = append([]string(nil), s.ExtraArgs...)

	if m.Type == "embedding" {
		embeddingLoad(&a, m, s)
	}

	// Compute total KV capacity after overrides. Use 64 bits and clamp because
	// context and parallel validation sets minimums but no maximums.
	a.KVCacheTokens = llamaCPP{}.applyKV(a)
	return a
}

// embeddingLoad enables /v1/embeddings; llama-server returns 501 without it.
// Inputs must fit the physical batch, so both batch sizes match the context.
// Context defaults to the model's training limit unless explicitly overridden.
// Generation-only settings are removed.
func embeddingLoad(a *Applied, m catalog.Model, s Settings) {
	a.Embedding = true
	if s.ContextLength == nil && m.MaxContextLength > 0 {
		a.ContextLength = min(a.ContextLength, m.MaxContextLength)
		if a.ContextLength <= 0 {
			a.ContextLength = m.MaxContextLength
		}
	}
	if s.BatchSize == nil {
		a.BatchSize = a.ContextLength
	}
	if s.UBatchSize == nil {
		a.UBatchSize = a.ContextLength
	}
	a.Vision, a.VisionSkipped = false, ""
	a.Speculative, a.SpecType, a.DraftModel, a.DraftMax = false, "", "", 0
	a.Sampling, a.TemplateKwargs = nil, nil
	a.Reasoning, a.ReasoningEffort, a.ReasoningBudget = "", "", nil
	a.CacheRAMMiB, a.CtxCheckpoints, a.CacheReuse, a.SlotSavePath = 0, 0, 0, ""
}

// ServedModel is the catalog key: --alias makes the engine report it.
func (llamaCPP) ServedModel(m catalog.Model) string { return m.Key }

// Traits: llama.cpp is the baseline - it reports our ids and serves metrics.
// Its one gap is KV usage, which it holds but does not export (measured on
// b11026 and b11040); ModelFabric computes that from /slots.
func (llamaCPP) Traits() Traits {
	return Traits{KVUsageMetric: engineshim.KVUsageMetric, KVUsageFromSlots: true}
}

// Ready checks that the engine serves the expected model, so an unrelated
// server already on the port cannot satisfy our launch.
func (llamaCPP) Ready(ctx context.Context, endpoint, served string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"/v1/models", nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %s", resp.Status)
	}
	var body struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return err
	}
	for _, d := range body.Data {
		if d.ID == served {
			return nil
		}
	}
	return fmt.Errorf("endpoint is up but does not serve %q", served)
}

func (llamaCPP) Argv(d *Definition, m catalog.Model, a Applied, bind string, port int) []string {
	argv := []string{
		d.Path(),
		"-m", m.Path,
		"--host", bind,
		"--port", strconv.Itoa(port),
		// --alias makes the engine report our catalog key at /v1/models, which
		// is exactly what the readiness probe checks.
		"--alias", m.Key,
		// -c is total KV capacity, shared with --kv-unified or divided between
		// slots. ContextLength × Parallel lets each slot reach the per-request limit.
		"-c", strconv.Itoa(a.KVCacheTokens),
		"-ngl", strconv.Itoa(a.GPULayers),
		"--parallel", strconv.Itoa(a.Parallel),
		"--metrics",
		// ModelFabric supplies the web UI.
		"--no-webui",
		// Use the embedded chat template; the generic fallback breaks reasoning
		// and tool-call parsing without reporting an error.
		"--jinja",
	}
	if a.Embedding {
		argv = append(argv, "--embeddings")
	}
	if a.FlashAttention {
		argv = append(argv, "-fa", "on")
	}
	if a.BatchSize > 0 {
		argv = append(argv, "--batch-size", strconv.Itoa(a.BatchSize))
	}
	if a.UBatchSize > 0 {
		argv = append(argv, "--ubatch-size", strconv.Itoa(a.UBatchSize))
	}
	if a.CacheTypeK != "" {
		argv = append(argv, "--cache-type-k", a.CacheTypeK)
	}
	if a.CacheTypeV != "" {
		argv = append(argv, "--cache-type-v", a.CacheTypeV)
	}
	if a.LoadMode != "" {
		argv = append(argv, "--load-mode", a.LoadMode)
	}
	if a.KVUnified {
		argv = append(argv, "--kv-unified")
	}
	if !a.KVOffload {
		// LM Studio's offloadKVCacheToGpu off: the KV cache stays in RAM,
		// trading speed for VRAM.
		argv = append(argv, "--no-kv-offload")
	}
	if a.CtxCheckpoints > 0 {
		argv = append(argv, "--ctx-checkpoints", strconv.Itoa(a.CtxCheckpoints))
	}
	if a.CacheReuse > 0 {
		argv = append(argv, "--cache-reuse", strconv.Itoa(a.CacheReuse))
	}
	argv = append(argv, "--cache-ram", strconv.Itoa(a.CacheRAMMiB))
	if a.SlotSavePath != "" {
		argv = append(argv, "--slot-save-path", a.SlotSavePath)
	}
	if a.NCPUMoE > 0 {
		// Mixture-of-experts: the first N layers' experts stay on the CPU,
		// which is how a large MoE model fits a small GPU.
		argv = append(argv, "--n-cpu-moe", strconv.Itoa(a.NCPUMoE))
	}
	if a.RopeFreqBase > 0 {
		argv = append(argv, "--rope-freq-base", strconv.FormatFloat(a.RopeFreqBase, 'f', -1, 64))
	}
	if a.RopeFreqScale > 0 {
		argv = append(argv, "--rope-freq-scale", strconv.FormatFloat(a.RopeFreqScale, 'f', -1, 64))
	}
	if a.Threads > 0 {
		argv = append(argv, "--threads", strconv.Itoa(a.Threads))
	}
	if a.ThreadsBatch > 0 {
		argv = append(argv, "--threads-batch", strconv.Itoa(a.ThreadsBatch))
	}
	if a.Seed != nil {
		argv = append(argv, "--seed", strconv.Itoa(*a.Seed))
	}
	if a.Speculative {
		n := a.DraftMax
		if n <= 0 {
			n = 3
		}
		argv = append(argv, "--spec-type", a.SpecType, "--spec-draft-n-max", strconv.Itoa(n))
		if a.SpecType == "draft-simple" && a.DraftModel != "" {
			argv = append(argv, "--spec-draft-model", a.DraftModel)
		}
		if a.DraftMin > 0 {
			argv = append(argv, "--spec-draft-n-min", strconv.Itoa(a.DraftMin))
		}
		if a.DraftPMin > 0 {
			argv = append(argv, "--spec-draft-p-min", strconv.FormatFloat(a.DraftPMin, 'f', -1, 64))
		}
		if a.DraftGPULayers != nil {
			argv = append(argv, "--spec-draft-ngl", strconv.Itoa(*a.DraftGPULayers))
		}
	}
	if s := a.Sampling; s != nil {
		flag := func(name string, v *float64) {
			if v != nil {
				argv = append(argv, name, strconv.FormatFloat(*v, 'f', -1, 64))
			}
		}
		flag("--temp", s.Temperature)
		if s.TopK != nil {
			argv = append(argv, "--top-k", strconv.Itoa(*s.TopK))
		}
		flag("--top-p", s.TopP)
		flag("--min-p", s.MinP)
		flag("--repeat-penalty", s.RepeatPenalty)
		flag("--presence-penalty", s.PresencePenalty)
		flag("--frequency-penalty", s.FrequencyPenalty)
	}
	if len(a.TemplateKwargs) > 0 {
		// Validate rejects unencodable kwargs. If any reach here, preserve the
		// flag as an empty object rather than omit a setting reported as applied.
		b, err := json.Marshal(a.TemplateKwargs)
		if err != nil {
			b = []byte("{}")
		}
		argv = append(argv, "--chat-template-kwargs", string(b))
	}
	if a.Reasoning != "" {
		argv = append(argv, "--reasoning", a.Reasoning)
	}
	if a.ReasoningEffort != "" {
		argv = append(argv, "--reasoning-effort", a.ReasoningEffort)
	}
	if a.ReasoningBudget != nil {
		argv = append(argv, "--reasoning-budget", strconv.Itoa(*a.ReasoningBudget))
	}
	// Without the projector a vision model loads and silently answers
	// text-only, so it is passed whenever the catalog found one.
	if a.Vision {
		argv = append(argv, "--mmproj", m.Projector)
	}
	// Operator-supplied extras go last so they can override generated flags:
	// the runtime's, then the load's own (Settings.Validate has already
	// refused any that would override a flag ModelFabric manages).
	argv = append(argv, d.Args...)
	return append(argv, a.ExtraArgs...)
}
