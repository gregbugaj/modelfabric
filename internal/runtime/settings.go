package runtime

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
)

// Settings are the load and inference options a caller can set for a model —
// the ones LM Studio exposes per model (its gear icon) and per load. Every
// field is optional: nil means "not set here", so Settings from several
// places merge field by field.
//
// Precedence, highest first: the load request, the operator's per-model
// defaults, the model's own model.yaml, the runtime's defaults.
//
// LM Studio names are noted where they differ.
type Settings struct {
	// Context and placement.
	ContextLength  *int     `json:"context_length,omitempty"`  // llm.load.contextLength
	Parallel       *int     `json:"parallel,omitempty"`        // numParallelSessions
	GPULayers      *int     `json:"gpu_layers,omitempty"`      // -ngl
	OffloadRatio   *float64 `json:"offload_ratio,omitempty"`   // acceleration.offloadRatio, 0..1 of the layers
	FlashAttention *bool    `json:"flash_attention,omitempty"` // flashAttention

	// Vision=false omits the image projector for text-only loads, saving its
	// VRAM. Nil loads the projector when available. This does not disable
	// speculation: auto still uses the model's MTP head when it has one.
	Vision *bool `json:"vision,omitempty"`

	// Reasoning. A thinking model spends tokens before it answers, and on a
	// classification task that is most of the work: a 27B spent 411 completion
	// tokens and 1803 characters of reasoning to return the word "Blank".
	// These are llama.cpp launch flags, not request fields — the server
	// accepts reasoning_budget in a request body and ignores it, which reads
	// as a control that does nothing.
	Reasoning *string `json:"reasoning,omitempty"` // --reasoning: on | off | auto
	// ReasoningEffort is handed to the chat template, which defines the levels
	// it accepts; a model.yaml's template_vars carries the publisher's default
	// (Qwen3.8-27B ships "xhigh", the most verbose it has).
	ReasoningEffort *string `json:"reasoning_effort,omitempty"` // --reasoning-effort
	// ReasoningBudget caps thinking in tokens: -1 unrestricted, 0 ends it at
	// once, N>0 is the allowance. Measured on qwen3-0.6b: 48 held reasoning to
	// 165-179 characters across runs, -1 gave 718-1105.
	ReasoningBudget *int `json:"reasoning_budget,omitempty"` // --reasoning-budget

	// Speculative decoding.
	SpecMode       *string  `json:"spec_mode,omitempty"`        // auto | mtp | draft | off
	DraftModel     *string  `json:"draft_model,omitempty"`      // speculativeDecoding.draftModel (a catalog key)
	DraftMax       *int     `json:"draft_max,omitempty"`        // --spec-draft-n-max
	DraftMin       *int     `json:"draft_min,omitempty"`        // --spec-draft-n-min
	DraftPMin      *float64 `json:"draft_p_min,omitempty"`      // --spec-draft-p-min
	DraftGPULayers *int     `json:"draft_gpu_layers,omitempty"` // --spec-draft-ngl

	// Batching and KV cache.
	BatchSize      *int    `json:"batch_size,omitempty"`      // evalBatchSize
	UBatchSize     *int    `json:"ubatch_size,omitempty"`     // physicalBatchSize
	CacheTypeK     *string `json:"cache_type_k,omitempty"`    // kCacheQuantizationType
	CacheTypeV     *string `json:"cache_type_v,omitempty"`    // vCacheQuantizationType
	KVUnified      *bool   `json:"kv_unified,omitempty"`      // useUnifiedKvCache
	KVOffload      *bool   `json:"kv_offload,omitempty"`      // offloadKVCacheToGpu
	CacheReuse     *int    `json:"cache_reuse,omitempty"`     // --cache-reuse
	CtxCheckpoints *int    `json:"ctx_checkpoints,omitempty"` // --ctx-checkpoints
	CacheRAM       *int    `json:"cache_ram,omitempty"`       // --cache-ram: host-RAM prompt cache, MiB; 0 is off

	// Memory.
	KeepInMemory *bool `json:"keep_in_memory,omitempty"` // keepModelInMemory (mlock)
	TryMmap      *bool `json:"try_mmap,omitempty"`       // tryMmap

	// Mixture of experts.
	NCPUMoE     *int     `json:"n_cpu_moe,omitempty"`     // --n-cpu-moe: expert layers kept on the CPU
	CPUMoERatio *float64 `json:"cpu_moe_ratio,omitempty"` // numCpuExpertLayersRatio, 0..1 of the layers

	// Model shape and threads.
	RopeFreqBase  *float64 `json:"rope_freq_base,omitempty"`  // ropeFrequencyBase
	RopeFreqScale *float64 `json:"rope_freq_scale,omitempty"` // ropeFrequencyScale
	Threads       *int     `json:"threads,omitempty"`         // cpuThreadPoolSize
	ThreadsBatch  *int     `json:"threads_batch,omitempty"`
	Seed          *int     `json:"seed,omitempty"`

	// Inference defaults: the engine's defaults for any request that does
	// not set its own (LM Studio's per-model prediction settings).
	Temperature      *float64       `json:"temperature,omitempty"`
	TopK             *int           `json:"top_k,omitempty"`
	TopP             *float64       `json:"top_p,omitempty"`
	MinP             *float64       `json:"min_p,omitempty"`
	RepeatPenalty    *float64       `json:"repeat_penalty,omitempty"`
	PresencePenalty  *float64       `json:"presence_penalty,omitempty"`
	FrequencyPenalty *float64       `json:"frequency_penalty,omitempty"`
	EnableThinking   *bool          `json:"enable_thinking,omitempty"`      // reasoning.enableThinking
	TemplateKwargs   map[string]any `json:"chat_template_kwargs,omitempty"` // any other template variables

	// ExtraArgs are appended to the engine's command line (LM Studio's
	// argumentsOverride). Flags ModelFabric manages are refused.
	ExtraArgs []string `json:"extra_args,omitempty"`
}

// Merge returns s with every field set in over replacing s's. Template
// variables merge key by key.
func (s Settings) Merge(over Settings) Settings {
	out := s
	ov := reflect.ValueOf(over)
	dst := reflect.ValueOf(&out).Elem()
	for i := 0; i < ov.NumField(); i++ {
		f := ov.Field(i)
		switch f.Kind() {
		case reflect.Ptr, reflect.Slice:
			if !f.IsNil() {
				dst.Field(i).Set(f)
			}
		}
	}
	if len(s.TemplateKwargs) > 0 || len(over.TemplateKwargs) > 0 {
		m := map[string]any{}
		for k, v := range s.TemplateKwargs {
			m[k] = v
		}
		for k, v := range over.TemplateKwargs {
			m[k] = v
		}
		out.TemplateKwargs = m
	}
	return out
}

// IsZero reports whether nothing is set.
func (s Settings) IsZero() bool {
	v := reflect.ValueOf(s)
	for i := 0; i < v.NumField(); i++ {
		f := v.Field(i)
		if (f.Kind() == reflect.Ptr || f.Kind() == reflect.Slice || f.Kind() == reflect.Map) && !f.IsNil() {
			return false
		}
	}
	return true
}

// SpecModes are the speculative decoding choices.
var SpecModes = []string{"auto", "mtp", "draft", "off"}

// ReasoningModes are llama.cpp's --reasoning choices.
var ReasoningModes = []string{"auto", "on", "off"}

// Reasoning effort levels are defined by the model's chat template, not by
// llama.cpp, so ModelFabric does not keep a list to check against. Qwen3.8-27B
// accepts xhigh (its default), medium and low, and raises a Jinja exception on
// anything else — "minimal", from llama.cpp's own help text, 500s every
// request. A list here would be right for one model and wrong for the next;
// the template's error names the levels it takes.

// CacheTypes are the KV cache types llama.cpp accepts.
var CacheTypes = []string{"f32", "f16", "bf16", "q8_0", "q4_0", "q4_1", "iq4_nl", "q5_0", "q5_1"}

// managedFlags are set by ModelFabric itself. An extra argument repeating one would
// win (llama.cpp takes the last occurrence) and could re-expose the engine on
// every interface, point it at another model, or break the readiness probe.
var managedFlags = []string{
	"--host", "--port", "--alias", "-a", "-m", "--model", "-mu", "--model-url",
	"-hf", "-hfr", "--hf-repo", "--api-key", "--api-key-file",
	"--ssl-key-file", "--ssl-cert-file", "--path", "--mmproj", "-mm",
	"-md", "--model-draft", "--spec-draft-model", // use draft_model instead
	// ModelFabric computes these and then reasons about them: -c is context ×
	// parallel, and the slot count is what the router and llm-d's room filter
	// use for capacity. Overriding them here would leave the engine serving
	// one shape while the mesh schedules for another. Use context_length and
	// parallel, which ModelFabric accounts for.
	"-c", "--ctx-size", "--parallel", "-np",
}

// CheckManagedFlags rejects arguments that would override a flag ModelFabric sets
// itself. Load settings go through Validate; a runtime definition's own Args
// land in the same override position and need the same rule, or a definition
// could re-expose the engine with --host or point it at another model.
func CheckManagedFlags(what string, args []string) error {
	var bad []string
	for _, a := range args {
		flag, _, _ := strings.Cut(a, "=")
		if contains(managedFlags, flag) {
			bad = append(bad, flag)
		}
	}
	if len(bad) > 0 {
		return fmt.Errorf("%s may not set %s: ModelFabric manages %s", what,
			strings.Join(bad, ", "), map[bool]string{true: "them", false: "it"}[len(bad) > 1])
	}
	return nil
}

// Validate rejects values llama.cpp would refuse or misread, and extra
// arguments that override what ModelFabric manages.
func (s Settings) Validate() error {
	var errs []string
	bad := func(format string, a ...any) { errs = append(errs, fmt.Sprintf(format, a...)) }
	posInt := func(name string, v *int, min int) {
		if v != nil && *v < min {
			bad("%s must be >= %d", name, min)
		}
	}
	unit := func(name string, v *float64) {
		if v != nil && (*v < 0 || *v > 1) {
			bad("%s must be between 0 and 1", name)
		}
	}
	posInt("context_length", s.ContextLength, 256)
	posInt("parallel", s.Parallel, 1)
	posInt("gpu_layers", s.GPULayers, 0)
	unit("offload_ratio", s.OffloadRatio)
	posInt("draft_max", s.DraftMax, 1)
	posInt("draft_min", s.DraftMin, 0)
	unit("draft_p_min", s.DraftPMin)
	posInt("draft_gpu_layers", s.DraftGPULayers, 0)
	posInt("batch_size", s.BatchSize, 1)
	posInt("ubatch_size", s.UBatchSize, 1)
	posInt("cache_reuse", s.CacheReuse, 0)
	posInt("ctx_checkpoints", s.CtxCheckpoints, 0)
	posInt("cache_ram", s.CacheRAM, 0)
	posInt("n_cpu_moe", s.NCPUMoE, 0)
	unit("cpu_moe_ratio", s.CPUMoERatio)
	posInt("threads", s.Threads, 1)
	posInt("threads_batch", s.ThreadsBatch, 1)
	posInt("top_k", s.TopK, 0)
	unit("top_p", s.TopP)
	unit("min_p", s.MinP)
	if s.Temperature != nil && *s.Temperature < 0 {
		bad("temperature must be >= 0")
	}
	if s.DraftMax != nil && s.DraftMin != nil && *s.DraftMin > *s.DraftMax {
		bad("draft_min (%d) exceeds draft_max (%d)", *s.DraftMin, *s.DraftMax)
	}
	if s.BatchSize != nil && s.UBatchSize != nil && *s.UBatchSize > *s.BatchSize {
		bad("ubatch_size (%d) exceeds batch_size (%d)", *s.UBatchSize, *s.BatchSize)
	}
	if s.GPULayers != nil && s.OffloadRatio != nil {
		bad("set gpu_layers or offload_ratio, not both")
	}
	if s.NCPUMoE != nil && s.CPUMoERatio != nil {
		bad("set n_cpu_moe or cpu_moe_ratio, not both")
	}
	if s.Reasoning != nil && !contains(ReasoningModes, *s.Reasoning) {
		bad("reasoning must be one of %s", strings.Join(ReasoningModes, ", "))
	}
	if s.ReasoningEffort != nil && strings.ContainsAny(*s.ReasoningEffort, " \t\n\"") {
		bad("reasoning_effort is a single word the model's template defines (often xhigh, medium or low)")
	}
	if s.ReasoningBudget != nil && *s.ReasoningBudget < -1 {
		bad("reasoning_budget must be -1 (unrestricted), 0, or a token count")
	}
	if s.SpecMode != nil && !contains(SpecModes, *s.SpecMode) {
		bad("spec_mode must be one of %s", strings.Join(SpecModes, ", "))
	}
	if s.SpecMode != nil && *s.SpecMode == "draft" && (s.DraftModel == nil || *s.DraftModel == "") {
		bad("spec_mode draft needs draft_model")
	}
	for name, v := range map[string]*float64{"rope_freq_base": s.RopeFreqBase, "rope_freq_scale": s.RopeFreqScale} {
		// llama.cpp treats these as "unset" at zero and misreads a negative,
		// but merging treated any present value as deliberate.
		if v != nil && *v <= 0 {
			bad("%s must be greater than 0", name)
		}
	}
	for name, v := range map[string]*string{"cache_type_k": s.CacheTypeK, "cache_type_v": s.CacheTypeV} {
		if v != nil && !contains(CacheTypes, *v) {
			bad("%s must be one of %s", name, strings.Join(CacheTypes, ", "))
		}
	}
	for _, a := range s.ExtraArgs {
		flag, _, _ := strings.Cut(a, "=")
		if contains(managedFlags, flag) {
			bad("extra_args may not set %s: ModelFabric manages it", flag)
		}
	}
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	return nil
}

// settingsFields lists the JSON names of every setting, for the CLI's help
// and the dashboard's form.
func SettingsFields() []string {
	t := reflect.TypeOf(Settings{})
	out := make([]string, 0, t.NumField())
	for i := 0; i < t.NumField(); i++ {
		name, _, _ := strings.Cut(t.Field(i).Tag.Get("json"), ",")
		out = append(out, name)
	}
	return out
}
