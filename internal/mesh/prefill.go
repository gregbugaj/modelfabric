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

// Display early rate estimates after a small sample, but require the larger
// minTrustedPrefillTokens threshold before using them for routing.
const (
	minPrefillTokens        = 1000
	minTrustedPrefillTokens = 20000
)

type EngineRates struct {
	// PrefillTokS and DecodeTokS are lifetime averages from the engine's own
	// counters, not a recent window: llama.cpp's *_seconds instantaneous
	// gauges read 0 unless a request is in flight at the moment of scraping.
	PrefillTokS float64
	DecodeTokS  float64
	// SpecAccepted is the retained draft fraction, 0..1, or -1 when not speculating.
	SpecAccepted float64
	// Trusted permits use of the prefill rate for placement. Preliminary rates
	// remain available for display.
	Trusted bool
	// PromptTokens, CachedTokens and OutputTokens are totals since engine start.
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
	// Reject read errors and truncated expositions to avoid publishing rates
	// from incomplete counters.
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

// fetchSlots derives KV utilization and busy-slot counts from one /slots request.
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

// fetchContext reads llama.cpp's /props context value without reconciling
// build-dependent units; see reconcileContext.
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
	// Read both the documented top-level n_ctx and the nested
	// default_generation_settings form used by installed builds.
	if props.Settings.NCtx > 0 {
		return props.Settings.NCtx, true
	}
	if props.NCtx > 0 {
		return props.NCtx, true
	}
	return 0, false
}
