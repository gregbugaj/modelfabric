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

// Following the engines rather than the requests.
//
// `mfsh log` streams what this node's router placed, which is the whole story
// until llm-d owns a model: then Envoy dials the engines itself, and a benchmark
// can run for hours across three machines while `mfsh log` prints its banner and
// nothing else. The engines still know what they are doing, and ModelFabric already
// reads it for its own routing — this prints that.

// engineSample is one engine at one moment.
type engineSample struct {
	node, id     string
	model        string
	inflight     int64
	slots        int64
	kv           float64
	prefillTokS  float64
	decodeTokS   float64
	specAccepted float64
	// Lifetime totals. A rate says how fast an engine is; these say what it
	// was given, which is the question a routing benchmark asks. They are
	// deliberately absent from busy(), which decides when to print a line:
	// they move every second, so keying on them would print every engine at
	// every poll.
	promptTokens int64
	cachedTokens int64
	outputTokens int64
	state        string
}

func (s engineSample) key() string { return s.node + "/" + s.id }

// busy is what a line is printed for: slots in use and KV. The rates drift by
// a token per second between polls, so keying on them would print every
// engine every second and bury the thing worth seeing.
func (s engineSample) busy() string {
	return fmt.Sprintf("%d/%d %.0f", s.inflight, s.slots, s.kv*100)
}

func engineActivityCmd(addr string, asJSON bool) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// The banners go to stderr so `mfsh log -engines -json > run.ndjson`
	// records only the samples: a header line in the middle of the data is
	// what makes a recording awkward to read back.
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
				// Every sample, not only the changes: a recording is joined
				// against a run by time, and a reader cannot tell a missing
				// second from an unchanged one. Suppressing repeats is right
				// for a person watching and wrong for a file.
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
				continue // nothing moved; printing it again is noise
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

// inflightCell reads "2/2 busy" at capacity, because a full engine is the
// thing worth noticing: the next request for it queues.
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

// rateCell is both halves of the work: reading the conversation and writing
// the answer. Prefill alone hid the thing speculative decoding changes —
// decode was 66 tok/s without the model's MTP head and 134 with it, and
// nothing in this view moved.
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

// engineSampleJSON is one engine at one moment, as a recording line. Field
// names match the mesh API so a reader that already parses /z/mesh needs no
// second vocabulary.
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
	// Lifetime totals, so a recording can answer what share of the fleet's
	// work each engine was given. Without them a run's record holds only
	// rates, and the share has to be read live before the next reload wipes
	// the counters.
	PromptTokens int64  `json:"prompt_tokens"`
	CachedTokens int64  `json:"cached_tokens"`
	OutputTokens int64  `json:"output_tokens"`
	State        string `json:"state"`
}

// KV usage is a ratio and prefill a rate; full float64 precision here is
// noise that makes a recording harder to read and no more accurate.
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
