package catalog

import (
	"fmt"
	"math"
	"os"
	"path/filepath"

	"github.com/gregbugaj/modelfabric/internal/yamlite"
)

// model.yaml (modelyaml.org) supplies publisher sampling defaults, capabilities,
// memory requirements, and chat-template variables. Unsupported YAML is ignored,
// leaving engine defaults in effect.

type ModelSpec struct {
	Vision         *bool    `json:"vision,omitempty"`
	Reasoning      *bool    `json:"reasoning,omitempty"`
	ToolUse        *bool    `json:"tool_use,omitempty"`
	MinMemoryBytes int64    `json:"min_memory_bytes,omitempty"`
	Sampling       Sampling `json:"sampling"`
	// TemplateVars are chat-template variables with the publisher's defaults
	// (enable_thinking, reasoning_effort, ...). A request can still set its
	// own through chat_template_kwargs.
	TemplateVars map[string]any `json:"template_vars,omitempty"`
}

// Sampling holds recommended sampler settings. Nil means "not specified";
// a disabled sampler is its neutral value (top-p 1, min-p 0, penalty 1/0).
type Sampling struct {
	Temperature      *float64 `json:"temperature,omitempty"`
	TopK             *int     `json:"top_k,omitempty"`
	TopP             *float64 `json:"top_p,omitempty"`
	MinP             *float64 `json:"min_p,omitempty"`
	RepeatPenalty    *float64 `json:"repeat_penalty,omitempty"`
	PresencePenalty  *float64 `json:"presence_penalty,omitempty"`
	FrequencyPenalty *float64 `json:"frequency_penalty,omitempty"`
}

func (s Sampling) IsZero() bool { return s == Sampling{} }

// samplerFields maps LM Studio's config keys to a setter and the value that
// turns the sampler off. LM Studio stores optional samplers as
// {checked, value}; unchecked means the sampler is not applied, which for
// llama.cpp is the neutral value; not llama.cpp's own default, which is on.
var samplerFields = map[string]struct {
	off float64
	set func(*Sampling, float64)
}{
	"llm.prediction.temperature":            {-1, func(s *Sampling, v float64) { s.Temperature = &v }},
	"llm.prediction.topKSampling":           {0, func(s *Sampling, v float64) { setTopK(s, v) }},
	"llm.prediction.topPSampling":           {1, func(s *Sampling, v float64) { s.TopP = &v }},
	"llm.prediction.minPSampling":           {0, func(s *Sampling, v float64) { s.MinP = &v }},
	"llm.prediction.repeatPenalty":          {1, func(s *Sampling, v float64) { s.RepeatPenalty = &v }},
	"llm.prediction.llama.presencePenalty":  {0, func(s *Sampling, v float64) { s.PresencePenalty = &v }},
	"llm.prediction.llama.frequencyPenalty": {0, func(s *Sampling, v float64) { s.FrequencyPenalty = &v }},
}

func ReadModelYAML(path string) (*ModelSpec, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ParseModelYAML(data)
}

// ParseModelYAML interprets model.yaml content. Unknown keys are ignored;
// known keys of the wrong shape are errors.
func ParseModelYAML(data []byte) (*ModelSpec, error) {
	v, err := yamlite.Parse(data)
	if err != nil {
		return nil, err
	}
	doc, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("model.yaml: top level is not a mapping")
	}
	spec := &ModelSpec{}

	if meta, ok := doc["metadataOverrides"].(map[string]any); ok {
		spec.Vision = boolPtr(meta["vision"])
		spec.Reasoning = boolPtr(meta["reasoning"])
		spec.ToolUse = boolPtr(meta["trainedForToolUse"])
		// A value past int64 (1e100 is legal YAML) converts to an
		// implementation-defined number, so the range is checked here.
		if n, ok := number(meta["minMemoryUsageBytes"]); ok && n > 0 && n < math.MaxInt64 {
			spec.MinMemoryBytes = int64(n)
		}
	}

	if cfg, ok := doc["config"].(map[string]any); ok {
		if op, ok := cfg["operation"].(map[string]any); ok {
			fields, _ := op["fields"].([]any)
			for _, f := range fields {
				field, ok := f.(map[string]any)
				if !ok {
					continue
				}
				key, _ := field["key"].(string)
				sf, known := samplerFields[key]
				if !known {
					continue
				}
				val, on, err := checkedValue(field["value"])
				if err != nil {
					return nil, fmt.Errorf("model.yaml: %s: %w", key, err)
				}
				switch {
				case on:
					sf.set(&spec.Sampling, val)
				case sf.off >= 0:
					sf.set(&spec.Sampling, sf.off)
				}
			}
		}
	}

	custom, _ := doc["customFields"].([]any)
	for _, c := range custom {
		field, ok := c.(map[string]any)
		if !ok {
			continue
		}
		def, hasDefault := field["defaultValue"]
		if !hasDefault || def == nil {
			continue
		}
		effects, _ := field["effects"].([]any)
		for _, e := range effects {
			eff, ok := e.(map[string]any)
			if !ok || eff["type"] != "setJinjaVariable" {
				continue
			}
			name, _ := eff["variable"].(string)
			if name == "" {
				continue
			}
			if spec.TemplateVars == nil {
				spec.TemplateVars = map[string]any{}
			}
			spec.TemplateVars[name] = def
		}
	}
	return spec, nil
}

// checkedValue reads a sampler value, either bare or {checked, value}.
func checkedValue(v any) (val float64, on bool, err error) {
	if m, ok := v.(map[string]any); ok {
		checked, _ := m["checked"].(bool)
		n, ok := number(m["value"])
		if !ok {
			// {checked: false} with no value is LM Studio saying "this
			// sampler is off". Demanding a number there rejected a document
			// that is perfectly ordinary.
			if !checked {
				return 0, false, nil
			}
			return 0, false, fmt.Errorf("value is not a number")
		}
		return n, checked, nil
	}
	n, ok := number(v)
	if !ok {
		return 0, false, fmt.Errorf("value is not a number")
	}
	return n, true, nil
}

// number reads a YAML scalar as a float. NaN and infinities are refused here:
// these values become engine arguments and memory estimates, and "1e400" or a
// NaN reaching that far shows up as a nonsensical flag, not as an error.
func number(v any) (float64, bool) {
	switch n := v.(type) {
	case int64:
		return float64(n), true
	case float64:
		if math.IsNaN(n) || math.IsInf(n, 0) {
			return 0, false
		}
		return n, true
	}
	return 0, false
}

// wholeInRange narrows a YAML number to an int, refusing a fraction or a value
// no int can hold. topKSampling used to be int(v) straight from the document.
func wholeInRange(v float64, lo, hi int) (int, bool) {
	if v != math.Trunc(v) || v < float64(lo) || v > float64(hi) {
		return 0, false
	}
	return int(v), true
}

func setTopK(s *Sampling, v float64) {
	if k, ok := wholeInRange(v, 0, 1<<20); ok {
		s.TopK = &k
	}
}

func boolPtr(v any) *bool {
	if b, ok := v.(bool); ok {
		return &b
	}
	return nil
}

func readHubSpec(dir string) (*ModelSpec, error) {
	spec, err := ReadModelYAML(filepath.Join(dir, "model.yaml"))
	if os.IsNotExist(err) {
		return nil, nil
	}
	return spec, err
}
