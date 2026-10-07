package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"syscall"
	"time"
)

// Engine activity includes llm-d traffic, which bypasses this node's router and request log.

type engineSample struct {
	node, id     string
	model        string
	inflight     int64
	slots        int64
	kv           float64
	prefillTokS  float64
	decodeTokS   float64
	specAccepted float64
	// Lifetime totals measure work distribution; exclude them from busy() to avoid printing every poll.
	promptTokens int64
	cachedTokens int64
	outputTokens int64
	state        string
}

func (s engineSample) key() string { return s.node + "/" + s.id }

// busy tracks slot and KV changes; fluctuating rates would cause output on every poll.
func (s engineSample) busy() string {
	return fmt.Sprintf("%d/%d %.0f", s.inflight, s.slots, s.kv*100)
}

func engineActivityCmd(addr string, asJSON bool) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Send banners to stderr so redirected JSON contains only samples.
	fmt.Fprintln(os.Stderr, dim("Following every engine in the mesh: requests in flight, KV cache, prefill rate. Ctrl-C to stop."))
	if asJSON {
		fmt.Fprintln(os.Stderr, dim("One JSON object per line, every engine every second, whether or not it changed — a recording is a time series and gaps in it are not silence."))
	} else {
		fmt.Fprintln(os.Stderr, dim("A line is printed when an engine's load changes, so a quiet fleet is quiet here too."))
	}
	enc := json.NewEncoder(os.Stdout)

	last := map[string]string{}
	seen := false
	for {
		samples, err := sampleEngines(ctx, addr)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			fmt.Fprintln(os.Stderr, yellow("mesh unreadable: "+err.Error()))
		}
		if len(samples) == 0 && !seen {
			fmt.Fprintln(os.Stderr, dim("No engines are running anywhere in the mesh."))
			seen = true
		}
		now := time.Now()
		for _, s := range samples {
			seen = true
			if asJSON {
				// Record every sample so unchanged activity can be distinguished from missing data.
				if err := enc.Encode(engineSampleJSON{
					Time: now.UTC().Format(time.RFC3339Nano), Node: s.node, Instance: s.id,
					Model: s.model, Inflight: s.inflight, Slots: s.slots,
					KVUsage: round3(s.kv), PrefillTokS: round1(s.prefillTokS),
					DecodeTokS: round1(s.decodeTokS), SpecAccepted: round3(s.specAccepted),
					PromptTokens: s.promptTokens, CachedTokens: s.cachedTokens,
					OutputTokens: s.outputTokens, State: s.state,
				}); err != nil {
					return err
				}
				continue
			}
			if last[s.key()] == s.busy() {
				continue
			}
			last[s.key()] = s.busy()
			fmt.Printf("%s  %-10s %s  %s  %s\n",
				dim(now.Format("15:04:05")),
				s.node,
				inflightCell(s),
				kvCell(s.kv),
				dim(rateCell(s)))
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(time.Second):
		}
	}
}

func inflightCell(s engineSample) string {
	txt := fmt.Sprintf("%d/%d", s.inflight, s.slots)
	switch {
	case s.slots > 0 && s.inflight >= s.slots:
		return yellow(txt + " full")
	case s.inflight > 0:
		return txt + "     "
	}
	return dim(txt + " idle")
}

func rateCell(s engineSample) string {
	out := fmt.Sprintf("%5.0f pp/s  %5.0f tg/s", s.prefillTokS, s.decodeTokS)
	if s.specAccepted >= 0 {
		out += fmt.Sprintf("  %3.0f%% drafts kept", s.specAccepted*100)
	}
	return out
}

func kvCell(v float64) string {
	if v < 0 {
		return dim("  kv   ?")
	}
	return fmt.Sprintf("kv %3.0f%%", v*100)
}

func sampleEngines(ctx context.Context, addr string) ([]engineSample, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var mesh struct {
		Self  meshNodeView   `json:"self"`
		Peers []meshNodeView `json:"peers"`
	}
	if err := call(ctx, addr, http.MethodGet, "/z/mesh", nil, &mesh); err != nil {
		return nil, err
	}
	var out []engineSample
	for _, n := range append([]meshNodeView{mesh.Self}, mesh.Peers...) {
		for _, i := range n.Instances {
			out = append(out, engineSample{
				node: n.Node, id: i.ID, model: i.Model, inflight: i.Inflight, slots: i.Slots,
				kv: i.KVUsage, prefillTokS: i.PrefillTokS, decodeTokS: i.DecodeTokS,
				specAccepted: i.SpecAccepted, promptTokens: i.PromptTokens,
				cachedTokens: i.CachedTokens, outputTokens: i.OutputTokens,
				state: i.State,
			})
		}
	}
	sort.Slice(out, func(a, b int) bool { return out[a].key() < out[b].key() })
	return out, nil
}

// engineSampleJSON uses mesh API field names for recorded samples.
type engineSampleJSON struct {
	Time         string  `json:"time"`
	Node         string  `json:"node"`
	Instance     string  `json:"instance"`
	Model        string  `json:"model"`
	Inflight     int64   `json:"inflight"`
	Slots        int64   `json:"slots"`
	KVUsage      float64 `json:"kv_usage"`
	PrefillTokS  float64 `json:"prefill_tok_s"`
	DecodeTokS   float64 `json:"decode_tok_s"`
	SpecAccepted float64 `json:"spec_accepted"`
	// Record lifetime totals to preserve work distribution after an engine reload resets its counters.
	PromptTokens int64  `json:"prompt_tokens"`
	CachedTokens int64  `json:"cached_tokens"`
	OutputTokens int64  `json:"output_tokens"`
	State        string `json:"state"`
}

func round3(v float64) float64 { return math.Round(v*1000) / 1000 }
func round1(v float64) float64 { return math.Round(v*10) / 10 }

type meshNodeView struct {
	Node      string `json:"node"`
	Instances []struct {
		ID            string  `json:"id"`
		Model         string  `json:"model"`
		Inflight      int64   `json:"inflight"`
		Slots         int64   `json:"slots"`
		ContextLength int     `json:"context_length"`
		ContextNote   string  `json:"context_note"`
		KVUsage       float64 `json:"kv_usage"`
		PrefillTokS   float64 `json:"prefill_tok_s"`
		DecodeTokS    float64 `json:"decode_tok_s"`
		SpecAccepted  float64 `json:"spec_accepted"`
		PromptTokens  int64   `json:"prompt_tokens"`
		CachedTokens  int64   `json:"cached_tokens"`
		OutputTokens  int64   `json:"output_tokens"`
		State         string  `json:"state"`
	} `json:"instances"`
}
