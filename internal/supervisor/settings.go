package supervisor

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/gregbugaj/modelfabric/internal/runtime"
)

// Per-model defaults live in ~/.modelfabric/model-defaults/<model>.json;
// inference-only presets live in ~/.modelfabric/presets/<name>.json.
// Load precedence, lowest first: runtime, model.yaml, default preset, model
// defaults, named load preset, explicit load settings.

type ModelDefaults struct {
	Preset   string           `json:"preset,omitempty"`
	Settings runtime.Settings `json:"settings"`
}

type Preset struct {
	Name        string           `json:"name"`
	Description string           `json:"description,omitempty"`
	Settings    runtime.Settings `json:"settings"`
}

var presetName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9 ._-]{0,63}$`)

var inferenceFields = map[string]bool{
	"temperature": true, "top_k": true, "top_p": true, "min_p": true,
	"repeat_penalty": true, "presence_penalty": true, "frequency_penalty": true,
	"enable_thinking": true, "chat_template_kwargs": true, "seed": true,
}

func checkInference(s runtime.Settings) error {
	b, _ := json.Marshal(s)
	var fields map[string]any
	_ = json.Unmarshal(b, &fields)
	var load []string
	for k := range fields {
		if !inferenceFields[k] {
			load = append(load, k)
		}
	}
	if len(load) > 0 {
		sort.Strings(load)
		what := "is a load setting"
		if len(load) > 1 {
			what = "are load settings"
		}
		return fmt.Errorf("presets hold inference settings only; %s %s — save it as the model's defaults", strings.Join(load, ", "), what)
	}
	return nil
}

func (s *Supervisor) defaultsPath(model string) string {
	return filepath.Join(s.cfg.DataDir, "model-defaults", url.PathEscape(model)+".json")
}

func (s *Supervisor) presetPath(name string) string {
	return filepath.Join(s.cfg.DataDir, "presets", name+".json")
}

func (s *Supervisor) visionPath() string {
	return filepath.Join(s.cfg.DataDir, "vision-defaults.json")
}

// DefaultVisionSettings supplies 32k context and one slot for image models.
// One slot reduces KV allocation while the vision encoder processes images
// serially. Explicit context prevents fallback to 8k, which rejected an
// 18,107-token image prompt. Speculation remains independently configurable.
func DefaultVisionSettings() runtime.Settings {
	one, ctx := 1, 32768
	return runtime.Settings{ContextLength: &ctx, Parallel: &one}
}

// VisionDefaults returns this node's image-model settings, or
// DefaultVisionSettings when none are saved. Capacity varies by node.
func (s *Supervisor) VisionDefaults() (runtime.Settings, error) {
	b, err := os.ReadFile(s.visionPath())
	if os.IsNotExist(err) {
		return DefaultVisionSettings(), nil
	}
	if err != nil {
		return runtime.Settings{}, fmt.Errorf("read vision defaults: %w", err)
	}
	var out runtime.Settings
	if err := json.Unmarshal(b, &out); err != nil {
		return runtime.Settings{}, fmt.Errorf("vision defaults are unreadable: %w", err)
	}
	return out, nil
}

// SetVisionDefaults saves this node's vision settings; zero settings restore
// DefaultVisionSettings rather than leaving vision models unconfigured.
func (s *Supervisor) SetVisionDefaults(v runtime.Settings) error {
	if err := v.Validate(); err != nil {
		return err
	}
	if v.IsZero() {
		if err := os.Remove(s.visionPath()); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	return writeJSONFile(s.visionPath(), v)
}

func (s *Supervisor) isVision(model string) bool {
	// The catalog is nil before the first scan, and in any test that does not
	// need one; resolveSettings runs in both.
	c := s.Catalog()
	if c == nil {
		return false
	}
	m, ok := c.Lookup(model)
	return ok && m.Projector != ""
}

// Defaults returns the saved defaults for a model (zero if none).
//
// A file that cannot be read or parsed is an error, not an absence: treating
// it as "no defaults" dropped what the operator had saved and quietly changed
// how the model loads.
func (s *Supervisor) Defaults(model string) (ModelDefaults, error) {
	var d ModelDefaults
	b, err := os.ReadFile(s.defaultsPath(model))
	if os.IsNotExist(err) {
		return d, nil
	}
	if err != nil {
		return d, fmt.Errorf("read defaults for %s: %w", model, err)
	}
	if err := json.Unmarshal(b, &d); err != nil {
		return ModelDefaults{}, fmt.Errorf("defaults for %s are unreadable: %w", model, err)
	}
	return d, nil
}

// SetDefaults saves a model's defaults; empty defaults remove the file.
func (s *Supervisor) SetDefaults(model string, d ModelDefaults) error {
	if err := d.Settings.Validate(); err != nil {
		return err
	}
	if d.Preset != "" {
		if _, err := s.Preset(d.Preset); err != nil {
			return err
		}
	}
	p := s.defaultsPath(model)
	if d.Preset == "" && d.Settings.IsZero() {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	return writeJSONFile(p, d)
}

// Presets lists saved presets by name.
//
// An unreadable directory or a corrupt preset is reported rather than folded
// into an empty list, which the dashboard showed as "no presets".
func (s *Supervisor) Presets() ([]Preset, error) {
	entries, err := os.ReadDir(filepath.Join(s.cfg.DataDir, "presets"))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read presets: %w", err)
	}
	out := []Preset{}
	for _, e := range entries {
		name, ok := strings.CutSuffix(e.Name(), ".json")
		if !ok {
			continue
		}
		p, err := s.Preset(name)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (s *Supervisor) Preset(name string) (Preset, error) {
	if !presetName.MatchString(name) {
		return Preset{}, fmt.Errorf("invalid preset name %q", name)
	}
	b, err := os.ReadFile(s.presetPath(name))
	if os.IsNotExist(err) {
		return Preset{}, fmt.Errorf("no preset named %q", name)
	}
	if err != nil {
		return Preset{}, err
	}
	var p Preset
	if err := json.Unmarshal(b, &p); err != nil {
		return Preset{}, fmt.Errorf("preset %q: %w", name, err)
	}
	p.Name = name
	// SavePreset enforces this, but a file edited by hand or left by an older
	// build can carry load-only settings, and resolveSettings would merge them
	// into a load that never asked for them.
	if err := checkInference(p.Settings); err != nil {
		return Preset{}, fmt.Errorf("preset %q holds settings that are not inference settings (re-save it): %w", name, err)
	}
	return p, nil
}

func (s *Supervisor) SavePreset(p Preset) error {
	if !presetName.MatchString(p.Name) {
		return fmt.Errorf("preset names are letters, digits, space, dot, dash, underscore (max 64); got %q", p.Name)
	}
	if err := checkInference(p.Settings); err != nil {
		return err
	}
	if err := p.Settings.Validate(); err != nil {
		return err
	}
	return writeJSONFile(s.presetPath(p.Name), p)
}

// DeletePreset removes a preset only if no model defaults reference it;
// a missing named preset would fail that model's next load.
func (s *Supervisor) DeletePreset(name string) error {
	if !presetName.MatchString(name) {
		return fmt.Errorf("invalid preset name %q", name)
	}
	used, err := s.modelsUsingPreset(name)
	if err != nil {
		return err
	}
	if len(used) > 0 {
		return fmt.Errorf("preset %q is the default for %s; change those models' defaults first",
			name, strings.Join(used, ", "))
	}
	err = os.Remove(s.presetPath(name))
	if os.IsNotExist(err) {
		return fmt.Errorf("no preset named %q", name)
	}
	return err
}

func (s *Supervisor) modelsUsingPreset(name string) ([]string, error) {
	dir := filepath.Join(s.cfg.DataDir, "model-defaults")
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read model defaults: %w", err)
	}
	var out []string
	for _, e := range entries {
		base, ok := strings.CutSuffix(e.Name(), ".json")
		if !ok {
			continue
		}
		model, err := url.PathUnescape(base)
		if err != nil {
			continue
		}
		d, err := s.Defaults(model)
		if err != nil {
			return nil, err
		}
		if d.Preset == name {
			out = append(out, model)
		}
	}
	sort.Strings(out)
	return out, nil
}

// resolveSettings merges every layer for one load of a model. Named presets
// that no longer exist are an error: a load should not silently drop what it
// was asked to apply.
func (s *Supervisor) resolveSettings(model string, reqPreset string, req runtime.Settings) (runtime.Settings, error) {
	var out runtime.Settings
	// Vision defaults have the lowest precedence and apply only when the
	// projector is enabled.
	if s.isVision(model) && (req.Vision == nil || *req.Vision) {
		v, err := s.VisionDefaults()
		if err != nil {
			return out, err
		}
		out = out.Merge(v)
	}
	d, err := s.Defaults(model)
	if err != nil {
		return out, err
	}
	if d.Preset != "" {
		p, err := s.Preset(d.Preset)
		if err != nil {
			return out, fmt.Errorf("%s's default preset: %w", model, err)
		}
		out = out.Merge(p.Settings)
	}
	out = out.Merge(d.Settings)
	if reqPreset != "" {
		p, err := s.Preset(reqPreset)
		if err != nil {
			return out, err
		}
		out = out.Merge(p.Settings)
	}
	out = out.Merge(req)
	return out, out.Validate()
}

func writeJSONFile(path string, v any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	// A fixed "<path>.tmp" let two concurrent saves overwrite each other's
	// temp file: one rename could publish the other request's content, and the
	// other could fail with ENOENT. The HTTP handlers run concurrently.
	f, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	if _, err := f.Write(append(b, '\n')); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Chmod(tmp, 0o644); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

var lmStudioPredictionKeys = map[string]string{
	"llm.prediction.temperature":              "temperature",
	"llm.prediction.topKSampling":             "top_k",
	"llm.prediction.topPSampling":             "top_p",
	"llm.prediction.minPSampling":             "min_p",
	"llm.prediction.repeatPenalty":            "repeat_penalty",
	"llm.prediction.llama.presencePenalty":    "presence_penalty",
	"llm.prediction.llama.frequencyPenalty":   "frequency_penalty",
	"llm.prediction.seed":                     "seed",
	"llm.prediction.reasoning.enableThinking": "enable_thinking",
}

// neutral is the value that turns an optional sampler off, which is what
// LM Studio's unchecked {checked: false} means.
var neutral = map[string]float64{"top_p": 1, "min_p": 0, "repeat_penalty": 1, "presence_penalty": 0, "frequency_penalty": 0}

// ImportLMStudioPreset converts config-presets/*.json into an inference preset.
// Load settings are reported but excluded; unknown keys are listed.
func ImportLMStudioPreset(data []byte, name string) (Preset, []string, error) {
	var lm struct {
		Name      string `json:"name"`
		Operation struct {
			Fields []struct {
				Key   string `json:"key"`
				Value any    `json:"value"`
			} `json:"fields"`
		} `json:"operation"`
	}
	if err := json.Unmarshal(data, &lm); err != nil {
		return Preset{}, nil, fmt.Errorf("not an LM Studio preset: %w", err)
	}
	if name == "" {
		name = lm.Name
	}
	settings := map[string]any{}
	var skipped []string
	for _, f := range lm.Operation.Fields {
		key, ok := lmStudioPredictionKeys[f.Key]
		if !ok {
			skipped = append(skipped, f.Key)
			continue
		}
		v := f.Value
		if m, ok := v.(map[string]any); ok { // {checked, value}
			if on, _ := m["checked"].(bool); !on {
				n, has := neutral[key]
				if !has {
					continue
				}
				v = n
			} else {
				v = m["value"]
			}
		}
		settings[key] = v
	}
	if len(settings) == 0 {
		return Preset{}, skipped, errors.New("the preset has no inference settings ModelFabric can apply")
	}
	b, _ := json.Marshal(settings)
	var s runtime.Settings
	if err := json.Unmarshal(b, &s); err != nil {
		return Preset{}, skipped, err
	}
	return Preset{Name: name, Description: "imported from LM Studio", Settings: s}, skipped, nil
}
