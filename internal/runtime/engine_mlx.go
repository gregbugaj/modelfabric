package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/gregbugaj/modelfabric/internal/catalog"
)

// MLXServedModel is the id mlx-lm maps to its loaded model. Other ids can
// trigger Hugging Face downloads; HF_HUB_OFFLINE prevents network access.
// MLX uses the context from config.json and allocates KV per request.
// Readiness uses /health; the server exposes neither /metrics nor /props.
const MLXServedModel = "default_model"

type mlxLM struct{}

func (mlxLM) Apply(d *Definition, m catalog.Model, req Requested) Applied {
	a := Applied{
		Runtime:        d.Name,
		ContextLength:  d.ContextLength,
		Parallel:       d.Parallel,
		BatchSize:      d.BatchSize,
		CtxCheckpoints: d.CtxCheckpoints,
		DraftMax:       d.DraftMax,
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
	setInt(&a.ContextLength, s.ContextLength)
	setInt(&a.Parallel, s.Parallel)
	setInt(&a.BatchSize, s.BatchSize)
	setInt(&a.CtxCheckpoints, s.CtxCheckpoints)
	setInt(&a.DraftMax, s.DraftMax)
	if s.CacheRAM != nil {
		a.CacheRAMMiB = *s.CacheRAM
	}
	// Inference defaults: per-model settings and presets over model.yaml. MLX
	// takes only these four; the penalties it has no flag for are left to the
	// request, which can still set them.
	sp := catalog.Sampling{}
	if a.Sampling != nil {
		sp = *a.Sampling
	}
	for _, f := range []struct {
		dst **float64
		v   *float64
	}{{&sp.Temperature, s.Temperature}, {&sp.TopP, s.TopP}, {&sp.MinP, s.MinP}} {
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
		if a.TemplateKwargs == nil {
			a.TemplateKwargs = map[string]any{}
		}
		if s.EnableThinking != nil {
			a.TemplateKwargs["enable_thinking"] = *s.EnableThinking
		}
		for k, v := range s.TemplateKwargs {
			a.TemplateKwargs[k] = v
		}
	}
	a.ExtraArgs = append(a.ExtraArgs, s.ExtraArgs...)

	// A draft model is only used when one was resolved for us; MLX has no
	// equivalent of llama.cpp's built-in MTP head.
	if req.DraftModelPath != "" && (req.Speculative == nil || *req.Speculative) {
		a.DraftModel = req.DraftModelPath
		a.Speculative = true
		a.SpecType = "draft-model"
	}

	a.Parallel = max(a.Parallel, 1)
	// mlx-lm has no context flag and grows KV per request. Report the model's
	// context, since routing must not rely on an unenforced lower limit.
	if m.MaxContextLength > 0 {
		a.ContextLength = m.MaxContextLength
	}
	// KV grows per request; zero indicates there is no fixed pool.
	a.KVCacheTokens = 0
	return a
}

func (mlxLM) Argv(d *Definition, m catalog.Model, a Applied, bind string, port int) []string {
	argv := []string{
		d.Path(),
		"--model", m.Path,
		"--host", bind,
		"--port", strconv.Itoa(port),
	}
	if a.Parallel > 1 {
		// Batched decode and prefill: the engine's own concurrency, which is
		// what a slot means here.
		argv = append(argv, "--decode-concurrency", strconv.Itoa(a.Parallel),
			"--prompt-concurrency", strconv.Itoa(a.Parallel))
	}
	if a.BatchSize > 0 {
		argv = append(argv, "--prefill-step-size", strconv.Itoa(a.BatchSize))
	}
	if a.CtxCheckpoints > 0 {
		argv = append(argv, "--prompt-cache-size", strconv.Itoa(a.CtxCheckpoints))
	}
	if a.CacheRAMMiB > 0 {
		// Shifted in 64 bits and clamped: validation only bars negatives, and
		// an extreme value used to wrap to a negative byte count here.
		bytes := int64(a.CacheRAMMiB) << 20
		if bytes > maxPromptCacheBytes || bytes < 0 {
			bytes = maxPromptCacheBytes
		}
		argv = append(argv, "--prompt-cache-bytes", strconv.FormatInt(bytes, 10))
	}
	if a.DraftModel != "" {
		argv = append(argv, "--draft-model", a.DraftModel)
		if a.DraftMax > 0 {
			argv = append(argv, "--num-draft-tokens", strconv.Itoa(a.DraftMax))
		}
	}
	if s := a.Sampling; s != nil {
		// Defaults for requests that set none of their own, as model.yaml
		// recommends them. A request still overrides each one.
		if s.Temperature != nil {
			argv = append(argv, "--temp", formatFloat(*s.Temperature))
		}
		if s.TopP != nil {
			argv = append(argv, "--top-p", formatFloat(*s.TopP))
		}
		if s.TopK != nil {
			argv = append(argv, "--top-k", strconv.Itoa(*s.TopK))
		}
		if s.MinP != nil {
			argv = append(argv, "--min-p", formatFloat(*s.MinP))
		}
	}
	if len(a.TemplateKwargs) > 0 {
		// Applied reports these as applied, so omitting them because they
		// would not encode launched a model with different chat-template
		// behaviour than every report claimed.
		b, err := json.Marshal(a.TemplateKwargs)
		if err != nil {
			b = []byte("{}")
		}
		argv = append(argv, "--chat-template-args", string(b))
	}
	argv = append(argv, d.Args...)
	argv = append(argv, a.ExtraArgs...)
	return argv
}

func (mlxLM) ServedModel(catalog.Model) string { return MLXServedModel }

// Traits: mlx-lm's /v1/models lists the Hugging Face cache rather than what it
// is serving, and it exposes no Prometheus metrics.
func (mlxLM) Traits() Traits {
	return Traits{FixedModels: true, NoMetrics: true, NoConstrainedDecoding: true}
}

// Ready checks mlx-lm's own health endpoint. Its /v1/models lists what is in
// the Hugging Face cache rather than what is loaded, so it cannot confirm this
// engine is serving the model we launched it with.
func (mlxLM) Ready(ctx context.Context, endpoint, _ string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"/health", nil)
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
	// Require JSON to avoid treating an unrelated service's 200 as readiness.
	// mlx-lm returns {"status":"ok"}; tolerate a missing status field for
	// compatibility with upstream response changes.
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return err
	}
	var health struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(body, &health); err != nil {
		return fmt.Errorf("/health did not answer JSON; something other than mlx-lm is on %s", endpoint)
	}
	if health.Status != "" && !strings.EqualFold(health.Status, "ok") {
		return fmt.Errorf("/health reports %q", health.Status)
	}
	return nil
}

// maxPromptCacheBytes caps --prompt-cache-bytes at 1 TiB to prevent overflow.
const maxPromptCacheBytes = 1 << 40

func formatFloat(f float64) string { return strconv.FormatFloat(f, 'g', -1, 64) }
