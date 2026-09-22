// Package tuner measures how many slots a machine should run for a model.
//
// Slots are set per node and usually set once, to the same number everywhere,
// because there is nothing to base a different answer on. On a mesh that is
// wrong. Measured on a three-node fleet, a Mac and an RTX 5090 given two slots
// each took 49% and 43% of the fleet's prefill and produced 5,824 and 64,145
// tokens: the Mac was advertising capacity it could not honour, and every
// router in front of it believed the advertisement.
//
// The answer cannot be calculated. KV cost per token needs attention-head
// geometry a GGUF does not always carry, and a hybrid-attention model — 16 of
// 64 layers full attention, in the case that prompted this — makes the obvious
// arithmetic wrong by about four times before hardware enters into it. So this
// tries it: load at each slot count, push that many requests through at once,
// and find where more slots stop buying throughput and start buying latency.
//
// The sweep lives here rather than in the CLI because it also runs from the
// dashboard, where it must survive the browser closing: it reloads engines
// several times, and a sweep abandoned halfway leaves a node on whatever slot
// count was being tested.
package tuner

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// Config is what to measure.
type Config struct {
	Model string `json:"model"`
	// Context is per request; the engine is loaded with Context × slots.
	Context int   `json:"context"`
	Prompt  int   `json:"prompt"` // approximate prompt tokens per request
	Output  int   `json:"output"` // tokens to generate per request
	Slots   []int `json:"slots"`
}

// Fill supplies the defaults a caller did not.
func (c *Config) Fill() {
	if c.Context <= 0 {
		c.Context = 32768
	}
	if c.Prompt <= 0 {
		c.Prompt = 3000
	}
	if c.Output <= 0 {
		c.Output = 300
	}
	// Empty means "find out": start at one and double until the engine will
	// not load or the throughput stops improving. Asking the operator which
	// slot counts to try is asking for part of the answer they ran this to
	// get, and the ceiling is a fact about the machine rather than a setting.
	sort.Ints(c.Slots)
}

// Row is one slot count, measured.
type Row struct {
	Slots int  `json:"slots"`
	Fit   bool `json:"fit"`
	// AskedContext and GotContext differ when the engine did not get what it
	// was asked for. A quiet mismatch here has cost this project a benchmark:
	// `mfsh ps` reported 65536 while the engine ran 32768, and every long
	// conversation routed there overflowed with nothing saying why.
	AskedContext int     `json:"asked_context"`
	GotContext   int     `json:"got_context"`
	Aggregate    float64 `json:"aggregate_tok_s"`   // across all concurrent requests
	PerRequest   float64 `json:"per_request_tok_s"` // within one request
	// DecodeTokS is generation alone: tokens divided by the time after the first
	// one arrived. Aggregate and PerRequest both include the prefill, which on a
	// slow machine dominates them — helion read a 3000-token prompt in 16s and
	// then generated at twice the rate the headline number suggested. Comparing
	// two engines, or a Mac against a GPU, needs the halves apart.
	DecodeTokS float64 `json:"decode_tok_s,omitempty"`
	TTFTP50Ms  int64   `json:"ttft_p50_ms"`
	TTFTMaxMs  int64   `json:"ttft_max_ms"`
	WallMs     int64   `json:"wall_ms"`
	Error      string  `json:"error,omitempty"`
	// Starved says the row was refused or abandoned because the machine ran
	// short of memory rather than because the engine said no. It stops the
	// sweep: the next slot count allocates more (see memory.go).
	Starved bool `json:"starved,omitempty"`
}

// Report is the sweep.
type Report struct {
	Node        string   `json:"node"`
	Model       string   `json:"model"`
	Context     int      `json:"context"`
	Rows        []Row    `json:"rows"`
	Recommended int      `json:"recommended"`
	Why         string   `json:"why"`
	Warnings    []string `json:"warnings,omitempty"`
	// StoppedBecause is set when the doubling sweep ended itself, so a reader
	// can tell "this is the ceiling" from "this is all that was asked for".
	StoppedBecause string    `json:"stopped_because,omitempty"`
	RestoredTo     int       `json:"restored_to,omitempty"`
	At             time.Time `json:"at"`
}

// Engine is the machine being tuned. The CLI drives one over HTTP; the server
// drives its own supervisor directly.
type Engine interface {
	// Reload brings the model up with this context and slot count.
	Reload(ctx context.Context, model string, contextLen, slots int) error
	// Target is the engine's own OpenAI address and the model id it answers to,
	// dialled directly so the measurement is of this machine rather than
	// wherever a router would have sent the work.
	//
	// The id is returned because it is not always the catalog's: mlx-lm
	// dispatches on the request's "model" field and only recognises the name it
	// was started with, so sending the catalog key would have it try to fetch a
	// model it has never heard of instead of answering.
	Target(ctx context.Context, model string) (base, served string, err error)
	// Inflight is how many requests the engine is answering, so a sweep never
	// reloads an engine out from under live traffic.
	Inflight(ctx context.Context, model string) (int, error)
	// Current is the configuration found on arrival, restored when the sweep
	// ends. Both numbers, not just the slot count: restoring the slots at the
	// swept context would hand back a machine running a context it never had,
	// which is the same silent re-configuration this tool exists to find.
	Current(ctx context.Context, model string) (slots, contextLen int, err error)
}

// Run sweeps the slot counts. progress, when set, is called after each row so
// a caller can report as it goes rather than only at the end.
func Run(ctx context.Context, e Engine, cfg Config, progress func(Row)) (Report, error) {
	cfg.Fill()
	rep := Report{Model: cfg.Model, Context: cfg.Context, At: time.Now()}

	if err := idle(ctx, e, cfg.Model); err != nil {
		return rep, err
	}
	was, wasContext, _ := e.Current(ctx, cfg.Model)
	if wasContext <= 0 {
		wasContext = cfg.Context
	}

	for _, s := range sweep(cfg.Slots) {
		if ctx.Err() != nil {
			break
		}
		row := measure(ctx, e, cfg, s)
		rep.Rows = append(rep.Rows, row)
		if row.GotContext > 0 && row.GotContext != row.AskedContext {
			rep.Warnings = append(rep.Warnings, fmt.Sprintf(
				"at %d slots the engine ran a %d-token context, not the %d asked for — check the build and the GPU's free memory",
				s, row.GotContext, row.AskedContext))
		}
		if progress != nil {
			progress(row)
		}
		// A machine that ran short does not get asked again, even for a slot
		// count the operator listed: the next one allocates more, and the row
		// after a wedge is not a measurement anyone wants.
		if row.Starved {
			rep.StoppedBecause = "the machine ran short of memory; larger slot counts would need more"
			break
		}
		// Only when the sweep was not spelled out: an explicit list is the
		// operator's, and cutting it short would hide a row they asked for.
		if len(cfg.Slots) == 0 {
			if stop, why := stopSweep(rep.Rows); stop {
				rep.StoppedBecause = why
				break
			}
		}
	}
	rep.Recommended, rep.Why = recommend(rep.Rows)

	// Leave the node as it was found. A tool that reconfigures a machine as a
	// side effect of measuring it is one nobody runs twice.
	if was > 0 {
		if err := e.Reload(context.Background(), cfg.Model, wasContext, was); err == nil {
			rep.RestoredTo = was
		} else {
			rep.Warnings = append(rep.Warnings, "could not restore the previous slot count: "+err.Error())
		}
	}
	return rep, nil
}

// sweepMax bounds the doubling. Past this the slot count is not a tuning
// question: nothing on one machine serves 64 conversations of a large model
// well, and each step costs a reload.
const sweepMax = 32

// sweep yields the slot counts to try. A given list is used as given; an empty
// one doubles from 1 and is stopped by stopSweep when the machine says no.
func sweep(given []int) []int {
	if len(given) > 0 {
		return given
	}
	var out []int
	for n := 1; n <= sweepMax; n *= 2 {
		out = append(out, n)
	}
	return out
}

// stopSweep decides whether to keep doubling.
//
// Two reasons to stop, and they mean different things. A row that did not run
// is the machine's ceiling — more slots will not fit either, and trying costs
// a reload each. A row that ran but did not improve on the one before it is
// the point of diminishing returns, past which slots only spread the same
// tokens over more requests.
func stopSweep(rows []Row) (bool, string) {
	if len(rows) == 0 {
		return false, ""
	}
	last := rows[len(rows)-1]
	if !last.Fit {
		return true, fmt.Sprintf("%d slots would not load, so larger counts will not either", last.Slots)
	}
	if len(rows) < 2 {
		return false, ""
	}
	prev := rows[len(rows)-2]
	if prev.Fit && last.Aggregate <= prev.Aggregate*1.10 {
		return true, fmt.Sprintf("%d slots did not beat %d by more than a tenth, so doubling again would only add latency",
			last.Slots, prev.Slots)
	}
	return false, ""
}

// idle refuses unless the engine is quiet, sampled rather than glanced at.
//
// A single check is a race. Under real load there are gaps between requests —
// on a fleet running an eight-way benchmark the instantaneous count is zero
// often enough to catch — and a sweep that starts in one of those gaps reloads
// the engine out from under live traffic. That nearly happened the first time
// this ran. Several samples across a few seconds cost nothing next to a sweep
// that takes minutes.
// Idle waits until the model has no requests in flight, or says why it will
// not: reloading an engine under live traffic drops those requests.
func Idle(ctx context.Context, e Engine, model string) error { return idle(ctx, e, model) }

func idle(ctx context.Context, e Engine, model string) error {
	const samples, gap = 5, 1200 * time.Millisecond
	for i := 0; i < samples; i++ {
		if i > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(gap):
			}
		}
		n, err := e.Inflight(ctx, model)
		if err != nil {
			return err
		}
		if n > 0 {
			return fmt.Errorf(
				"%s is serving %d request(s); tuning reloads the engine, so wait until the node is idle",
				model, n)
		}
	}
	return nil
}

// recommend reads the sweep. More slots are worth having while total
// throughput still climbs; past that they only spread the same tokens over
// more requests, which every caller feels as a slower answer and every router
// reads as free capacity.
func recommend(rows []Row) (int, string) {
	var ok []Row
	for _, r := range rows {
		if r.Fit {
			ok = append(ok, r)
		}
	}
	if len(ok) == 0 {
		return 0, "nothing completed; the engine did not run at any slot count tried"
	}
	best := ok[0]
	for _, r := range ok[1:] {
		// A tenth more throughput is worth a slot; less than that is noise
		// between runs, and taking it trades real latency for nothing.
		if r.Aggregate > best.Aggregate*1.10 {
			best = r
		}
	}
	top := ok[0]
	for _, r := range ok[1:] {
		if r.Aggregate > top.Aggregate {
			top = r
		}
	}
	if top.Slots != best.Slots {
		return best.Slots, fmt.Sprintf(
			"%d slots reached %.0f tok/s against %.0f, which is not worth %.1fs more on the first token",
			top.Slots, top.Aggregate, best.Aggregate,
			float64(top.TTFTP50Ms-best.TTFTP50Ms)/1000)
	}
	// best is also the fastest row, so nothing was traded away. Which of two
	// different things happened depends on whether anything was tried past it:
	// helion reported "still climbing at 1 slots" while its own stop reason said
	// 2 slots had not beaten 1 — it had peaked at the first row and fallen, and
	// the sweep only looked like it ran out of room.
	if best.Slots == ok[len(ok)-1].Slots {
		return best.Slots, fmt.Sprintf(
			"throughput was still climbing at %s, and the sweep stopped before it flattened",
			slotsWord(best.Slots))
	}
	return best.Slots, fmt.Sprintf(
		"%s gave the most throughput; past that the same tokens are spread over more requests",
		slotsWord(best.Slots))
}

func slotsWord(n int) string {
	if n == 1 {
		return "1 slot"
	}
	return fmt.Sprintf("%d slots", n)
}

// measure loads at one slot count and pushes that many requests through at
// once. The prompts differ from each other on purpose: identical ones would
// all hit the prefix cache after the first, and what is measured here is what
// the engine can do concurrently, not how well it caches.
func measure(ctx context.Context, e Engine, cfg Config, slots int) Row {
	r := Row{Slots: slots, AskedContext: cfg.Context * slots}
	// Before anything is allocated. A reload on a machine with nothing to spare
	// is how a sweep takes its host off the network.
	if tight, why := memoryTooTight(); tight {
		r.Error, r.Starved = why, true
		return r
	}
	if err := e.Reload(ctx, cfg.Model, cfg.Context, slots); err != nil {
		r.Error = short(err.Error())
		return r
	}
	base, served, err := e.Target(ctx, cfg.Model)
	if err != nil {
		r.Error = short(err.Error())
		return r
	}
	r.GotContext = engineContext(ctx, base)

	type one struct {
		ttft   time.Duration
		tokens int
		dur    time.Duration
		err    string
	}
	// The requests run under their own context so the watch can abandon them.
	rowCtx, abandon := context.WithCancel(ctx)
	defer abandon()
	stopWatch := watchMemory(rowCtx, abandon)

	out := make([]one, slots)
	start := time.Now()
	var wg sync.WaitGroup
	for i := 0; i < slots; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			began := time.Now()
			n, ttft, err := streamOnce(rowCtx, base, served, prompt(i, cfg.Prompt), cfg.Output)
			out[i] = one{ttft: ttft, tokens: n, dur: time.Since(began)}
			if err != nil {
				out[i].err = short(err.Error())
			}
		}(i)
	}
	wg.Wait()
	if starved, why := stopWatch(); starved {
		// Whatever the requests reported, the reason they stopped is the
		// machine, and that is what the row has to say.
		r.Error, r.Starved = why, true
		r.WallMs = time.Since(start).Milliseconds()
		return r
	}
	wall := time.Since(start)
	r.WallMs = wall.Milliseconds()

	total, done := 0, 0
	var per, decode float64
	var ttfts []time.Duration
	for _, o := range out {
		if o.err != "" {
			if r.Error == "" {
				r.Error = o.err
			}
			continue
		}
		done++
		total += o.tokens
		ttfts = append(ttfts, o.ttft)
		if o.dur > 0 {
			per += float64(o.tokens) / o.dur.Seconds()
		}
		// Generation alone. The first token is the end of the prefill, so the
		// time after it is what decoding cost; one token in the window is no
		// measurement.
		if gen := o.dur - o.ttft; gen > 0 && o.tokens > 1 {
			decode += float64(o.tokens-1) / gen.Seconds()
		}
	}
	if done == 0 {
		return r
	}
	// Requests that succeeded and produced nothing are not a measurement of
	// zero. This row printed "0 tok/s together, 0 decode, first token 0.0s" and
	// then recommended the slot count that produced it — the engine had
	// streamed every token under a field name the parser did not read. A rate
	// of zero from a healthy request means this package failed to count, so it
	// says so instead of publishing the number.
	if total == 0 {
		if r.Error == "" {
			r.Error = fmt.Sprintf("%d of %d requests finished without a token this sweep could count",
				done, slots)
		}
		return r
	}
	r.Fit = true
	if wall > 0 {
		r.Aggregate = float64(total) / wall.Seconds()
	}
	r.PerRequest = per / float64(done)
	r.DecodeTokS = decode / float64(done)
	sort.Slice(ttfts, func(a, b int) bool { return ttfts[a] < ttfts[b] })
	r.TTFTP50Ms = ttfts[len(ttfts)/2].Milliseconds()
	r.TTFTMaxMs = ttfts[len(ttfts)-1].Milliseconds()
	return r
}

// engineContext asks the engine what context it is actually running.
func engineContext(ctx context.Context, base string) int {
	var props struct {
		NCtx     int `json:"n_ctx"`
		Settings struct {
			NCtx int `json:"n_ctx"`
		} `json:"default_generation_settings"`
	}
	rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(rctx, http.MethodGet, strings.TrimRight(base, "/")+"/props", nil)
	if err != nil {
		return 0
	}
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		return 0
	}
	defer resp.Body.Close()
	if json.NewDecoder(resp.Body).Decode(&props) != nil {
		return 0
	}
	if props.Settings.NCtx > 0 {
		return props.Settings.NCtx
	}
	return props.NCtx
}

// prompt builds roughly n tokens, different for each slot. Four characters per
// token is close enough for prose; what matters is that every slot gets a
// comparable amount of work and no two share a prefix.
// prompt builds one request's text. The words are pseudorandom so the prompts
// differ from each other, which is what makes the row a measure of concurrency
// rather than of the prefix cache.
//
// It has a side effect worth knowing: nonsense to be summarised is output a
// drafter cannot guess, so this workload is near worst case for speculative
// decoding. Measured 2026-09-25, MTP read as about 8% slower than no
// speculation on both a 5090 and a Mac, while the same 5090 on predictable
// output decodes 134 tok/s against 66 with drafting off. So the decode column
// is a floor for a drafting engine, and printTuneSummary says so.
func prompt(seed, tokens int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Document %d. ", seed*7919)
	words := []string{"scheduler", "prefill", "decode", "cache", "affinity", "slot",
		"queue", "tensor", "kernel", "batch", "context", "token", "engine", "mesh"}
	for i := 0; b.Len() < tokens*4; i++ {
		fmt.Fprintf(&b, "%s-%d ", words[(i+seed)%len(words)], (i*seed+i)%9973)
	}
	b.WriteString("\n\nSummarise the above in one sentence.")
	return b.String()
}

// streamOnce sends one request and returns tokens produced and the time to the
// first of them. Streamed so time-to-first-token is measured rather than
// inferred from the total.
func streamOnce(ctx context.Context, base, model, text string, maxTokens int) (int, time.Duration, error) {
	body, err := json.Marshal(map[string]any{
		"model": model, "max_tokens": maxTokens, "stream": true,
		"messages": []map[string]string{{"role": "user", "content": text}},
	})
	if err != nil {
		return 0, 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(base, "/")+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return 0, 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	began := time.Now()
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		return 0, 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return 0, 0, fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	n := 0
	var ttft time.Duration
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	for sc.Scan() {
		data, ok := strings.CutPrefix(strings.TrimSpace(sc.Text()), "data:")
		if !ok {
			continue
		}
		data = strings.TrimSpace(data)
		if data == "" || data == "[DONE]" {
			continue
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
					// A thinking model's tokens are real work and have to be
					// counted, but engines do not agree on the field: llama.cpp
					// sends reasoning_content, mlx-lm sends reasoning. Counting
					// only one of them measured a 27B MLX model at 0 tok/s —
					// every token it produced arrived under a name this parser
					// did not read.
					ReasoningContent string `json:"reasoning_content"`
					Reasoning        string `json:"reasoning"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if json.Unmarshal([]byte(data), &chunk) != nil || len(chunk.Choices) == 0 {
			continue
		}
		d := chunk.Choices[0].Delta
		if d.Content == "" && d.ReasoningContent == "" && d.Reasoning == "" {
			continue
		}
		if n == 0 {
			ttft = time.Since(began)
		}
		n++
	}
	return n, ttft, sc.Err()
}

func short(s string) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\n", " "))
	if len(s) > 96 {
		s = s[:96] + "…"
	}
	return s
}
