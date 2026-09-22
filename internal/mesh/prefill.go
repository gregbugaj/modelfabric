package mesh

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/gregbugaj/modelfabric/internal/engineshim"
)

// Two floors, because one number was doing two jobs.
//
// The rate feeds routing — ModelFabric scores placement by it and publishes it to
// llm-d as a scheduling label — so a figure from a handful of short prompts
// misplaces real traffic. That is what minTrustedPrefillTokens guards, and it
// is unchanged.
//
// But waiting for it to show anything at all was its own problem: a slow
// engine took minutes of real traffic to cross 20k, and until then the
// dashboard showed a dash next to an engine that was visibly busy. A rate over
// a thousand tokens is rough, and rough-and-labelled beats absent. So the
// measurement is reported early and marked, and only trusted late.
const (
	minPrefillTokens        = 1000
	minTrustedPrefillTokens = 20000
)

// fetchPrefillRate measures an engine's prompt-processing rate from
// llama.cpp's own cumulative counters (prompt_tokens_total and
// prompt_seconds_total on /metrics). It is the average since the engine
// started, so it describes this engine on this model under the traffic it has
// actually seen. ok is false when the engine has no metrics or too little
// history.
// EngineRates is what an engine's counters say about its throughput.
//
// Prefill alone describes half the work. A coding agent reads a long
// conversation and then writes code: prefill is the reading, decode the
// writing, and the model's MTP head only moves the second — measured on a
// 5090, 66 tok/s without it against 134 with. A view that shows prefill and
// not decode cannot see the difference speculation makes.
type EngineRates struct {
	// PrefillTokS and DecodeTokS are lifetime averages from the engine's own
	// counters, not a recent window: llama.cpp's *_seconds instantaneous
	// gauges read 0 unless a request is in flight at the moment of scraping.
	PrefillTokS float64
	DecodeTokS  float64
	// SpecAccepted is the share of drafted tokens the model kept, 0..1, and
	// -1 when the engine is not speculating. It is the honest measure of
	// whether speculation is paying: drafts that are rejected cost time.
	SpecAccepted float64
	// Trusted says the engine has processed enough prompt for the rate to
	// decide where traffic goes. Below it the rate is still reported, because
	// a rough number a reader can see beats a dash, but nothing routes by it.
	Trusted bool
	// PromptTokens, CachedTokens and OutputTokens are lifetime totals since
	// the engine started. Rates say how fast an engine is; these say how much
	// of the fleet's work it was actually given, which is the question a
	// scheduler is answering and the one a rate cannot.
	PromptTokens, CachedTokens, OutputTokens int64
}

func (m *Mesh) fetchRates(ctx context.Context, base string) (EngineRates, bool) {
	ctx, cancel := context.WithTimeout(ctx, m.probe)
	defer cancel()
	var zero EngineRates
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/metrics", nil)
	if err != nil {
		return zero, false
	}
	resp, err := m.client.Do(req)
	if err != nil {
		return zero, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return zero, false
	}
	var tokens, seconds, outTokens, outSeconds, drafted, accepted, cached float64
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) != 2 {
			continue
		}
		v, err := strconv.ParseFloat(f[1], 64)
		if err != nil {
			continue
		}
		switch f[0] {
		case "llamacpp:prompt_tokens_total":
			tokens = v
		case "llamacpp:prompt_seconds_total":
			seconds = v
		case "llamacpp:tokens_predicted_total":
			outTokens = v
		case "llamacpp:tokens_predicted_seconds_total":
			outSeconds = v
		case "llamacpp:spec_decode_num_draft_tokens_total":
			drafted = v
		case "llamacpp:spec_decode_num_accepted_tokens_total":
			accepted = v
		case "llamacpp:prompt_tokens_cached_total":
			cached = v
		}
	}
	// A truncated body or a read error left whatever was parsed so far looking
	// like the whole exposition, which becomes a prefill rate the router
	// believes. Half a metrics page is not a measurement.
	if err := sc.Err(); err != nil {
		return zero, false
	}
	if tokens < minPrefillTokens || seconds <= 0 {
		return zero, false
	}
	out := EngineRates{
		PrefillTokS:  tokens / seconds,
		Trusted:      tokens >= minTrustedPrefillTokens,
		SpecAccepted: -1,
		PromptTokens: int64(tokens),
		CachedTokens: int64(cached),
		OutputTokens: int64(outTokens),
	}
	if outTokens > 0 && outSeconds > 0 {
		out.DecodeTokS = outTokens / outSeconds
	}
	if drafted > 0 {
		out.SpecAccepted = accepted / drafted
	}
	return out, true
}

// fetchKVUsage computes an engine's KV-cache utilization from /slots, for
// engines that hold the number but publish no gauge (llama.cpp). It is the
// same figure internal/engineshim republishes for llm-d; this is ModelFabric reading
// it for itself, so the dashboard and the router see it without anyone
// scraping through the shim.
// fetchSlots reads /slots once and derives both figures from it, so a poll
// costs one request rather than two.
func (m *Mesh) fetchSlots(ctx context.Context, base string) ([]engineshim.Slot, bool) {
	ctx, cancel := context.WithTimeout(ctx, m.probe)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/slots", nil)
	if err != nil {
		return nil, false
	}
	resp, err := m.client.Do(req)
	if err != nil {
		return nil, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, false // /slots can be turned off; that is not an error here
	}
	var slots []engineshim.Slot
	if err := json.NewDecoder(resp.Body).Decode(&slots); err != nil {
		return nil, false
	}
	return slots, true
}

// fetchContext asks an engine what context it is running, via llama.cpp's
// /props. The number's meaning differs by build — see reconcileContext — so what
// comes back is the engine's own figure, unreconciled.
func (m *Mesh) fetchContext(ctx context.Context, base string) (int, bool) {
	ctx, cancel := context.WithTimeout(ctx, m.probe)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/props", nil)
	if err != nil {
		return 0, false
	}
	resp, err := m.client.Do(req)
	if err != nil {
		return 0, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, false // mlx-lm serves no /props; that is not an error here
	}
	var props struct {
		NCtx     int `json:"n_ctx"`
		Settings struct {
			NCtx int `json:"n_ctx"`
		} `json:"default_generation_settings"`
	}
	if json.NewDecoder(resp.Body).Decode(&props) != nil {
		return 0, false
	}
	// Measured on this fleet: neither build serves a top-level n_ctx, both put
	// it under default_generation_settings. Both are read because a build that
	// serves the top-level one is the documented shape.
	if props.Settings.NCtx > 0 {
		return props.Settings.NCtx, true
	}
	if props.NCtx > 0 {
		return props.NCtx, true
	}
	return 0, false
}
