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

// Per-model defaults and presets — LM Studio's gear icon and Presets.
//
//	~/.modelfabric/model-defaults/<model>.json  load and inference settings for one
//	                                     model, applied on every load of it
//	~/.modelfabric/presets/<name>.json          named inference settings, for any model
//
// As in LM Studio, a preset holds inference settings only (sampling,
// thinking, template variables); load settings belong to the model.
// Precedence at load, lowest first: runtime, model.yaml, the model's default
// preset, the model's default settings, a preset named at load, settings
// given at load.

// ModelDefaults are what an operator saved for one model.
type ModelDefaults struct {
	Preset   string           `json:"preset,omitempty"`
	Settings runtime.Settings `json:"settings"`
}

// Preset is a named bundle of inference settings.
type Preset struct {
	Name        string           `json:"name"`
	Description string           `json:"description,omitempty"`
	Settings    runtime.Settings `json:"settings"`
}

var presetName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9 ._-]{0,63}$`)

// inferenceFields are the settings a preset may hold.
var inferenceFields = map[string]bool{
	"temperature": true, "top_k": true, "top_p": true, "min_p": true,
	"repeat_penalty": true, "presence_penalty": true, "frequency_penalty": true,
	"enable_thinking": true, "chat_template_kwargs": true, "seed": true,
}

// checkInference refuses load settings in a preset, naming them.
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

// DefaultVisionSettings is what a model carrying an image projector loads with
// when the node has saved nothing of its own: 32k of context and one slot.
//
// It used to turn speculation off as well. That was a workaround for a llama.cpp
// defect in how it chunks images — the drafter had no representation for image
// positions, and a prompt carrying one failed the whole request with "failed to
// process mtmd chunk". The defect is gone from the builds this fleet runs:
// re-tested 2026-09-24 at the benchmark's own context and slot count with the
// projector loaded, two concurrent image requests of 986KB and 827KB succeeded
// with drafting live throughout, on CUDA and on Metal.
//
// It applied to every load of a model that has a projector, so on this fleet it
// was every load, and what it cost depends entirely on the work: measured on a
// 5090 with predictable output, 66 tok/s decoding against 134 with the model's
// own MTP head. On unpredictable output drafting costs a little instead —
// measured 2026-09-25 on the tuner's own summarise-this-nonsense prompts, 78
// against 83 on the 5090 and 22 against 24 on the Mac. So ModelFabric stops deciding:
// the head is used when the model has one, and a node that wants otherwise
// saves spec_mode off for itself, which is also the escape for an older build
// that still has the defect.
//
// The slot count stays. It is not only about the defect: -c is per-slot ×
// parallel, so four slots ask a GPU for four times the KV cache to serve work
// the vision encoder runs one image at a time anyway.
//
// The context is here because the slot count changes what it means. -c is
// ContextLength × Parallel and llama.cpp divides it back per slot, so dropping
// four slots to one without saying anything about context quartered the KV
// pool — correct, and the point — but it also meant a load that named no
// context fell to the 8k runtime default. An image prompt does not fit in 8k:
// one of 18107 tokens came back "exceeds the available context size (8192)"
// within minutes of the slot change. 32k is what an image prompt actually
// needs, and at one slot it still asks the GPU for a quarter of what four
// slots of 32k did.
func DefaultVisionSettings() runtime.Settings {
	one, ctx := 1, 32768
	return runtime.Settings{ContextLength: &ctx, Parallel: &one}
}

// VisionDefaults are this node's settings for every model that takes images.
//
// Per node, because capacity is: a Mac serving a 27B over Metal and a CUDA box
// do not have the same room for slots, and the fleet is deliberately mixed.
// A node that has saved nothing gets DefaultVisionSettings.
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

// isVision reports whether a model carries an image projector. An unknown
// model is not vision: the settings below it still apply, and a model this
// node cannot see is one it is not about to load.
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

// Preset reads one preset.
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

// SavePreset validates and stores a preset.
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

// DeletePreset removes a preset that no model's defaults name.
//
// The comment here used to say a model naming a deleted preset "keeps loading,
// without it". It does not: resolveSettings treats a missing named preset as
// an error, deliberately, so deleting one in use broke that model's next load
// with a message about a preset the operator had just removed. Refusing, and
// naming the models, is the honest half of that contract.
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

// modelsUsingPreset lists the models whose saved defaults name this preset.
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
	// The lowest layer, and only for a model that takes images: a preset, the
	// model's own defaults and the load request all still win over it. A load
	// that asked for the model without its projector is not image work, so
	// none of it applies — otherwise -vision off would still inherit the one
	// slot and the speculation-off that exist only for images.
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

// lmStudioPredictionKeys map LM Studio's preset and model.yaml field keys
// onto Settings.
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

// ImportLMStudioPreset converts an LM Studio preset file (config-presets/*.json)
// into a ModelFabric preset. Load settings in it are reported, not imported —
// LM Studio moved those out of presets too. Unknown keys are listed.
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
