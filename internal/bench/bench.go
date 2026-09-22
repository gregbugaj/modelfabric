// Package bench measures how fast a model runs on one machine, in a fixed
// shape that can be repeated: prompts of 1K to 64K tokens with a fixed 128
// out, then 1, 2, 4 and 8 requests at once, sharing a prompt or not.
//
// It is not the tuner. The tuner asks how many slots a machine should run and
// sweeps that; this asks how fast the model is here, and keeps everything that
// changes the answer still and written down: the exact text sent (corpus.go),
// the exact number of tokens generated, the engine's build and settings, the
// model file, the GPU and its driver. A result that cannot be reproduced
// cannot be compared, and comparing is the point.
//
// Like the tuner it runs on the node it measures, against that node's own
// engine: through the router it would measure wherever the router chose. It
// reloads the engine for each phase (one slot with a long context for the
// prompt sweep, eight short slots for batching) and puts back what it found.
package bench

import (
	"context"
	"fmt"
	"math"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/gregbugaj/modelfabric/internal/tuner"
)

// Config is what to run. Zero values are the standard suite, the one every
// published result uses unless it says otherwise.
type Config struct {
	Model   string `json:"model"`
	Prompts string `json:"prompts"` // prose | code
	// PP are the prompt lengths of the single-request tests, in tokens.
	PP []int `json:"pp"`
	// TG is how many tokens every request generates, exactly.
	TG int `json:"tg"`
	// Batch are the concurrency levels, each run at BatchPP tokens.
	Batch   []int `json:"batch"`
	BatchPP int   `json:"batch_pp"`
	// Reps runs each test this many times and reports the median.
	Reps int `json:"reps"`
}

var (
	StandardPP    = []int{1024, 4096, 8192, 16384, 32768}
	StandardBatch = []int{2, 4, 8}
)

// Fill supplies the standard suite where the caller chose nothing.
func (c *Config) Fill() {
	if c.Prompts == "" {
		c.Prompts = "prose"
	}
	if len(c.PP) == 0 {
		c.PP = append([]int(nil), StandardPP...)
	}
	if c.TG <= 0 {
		c.TG = 128
	}
	if c.Batch == nil {
		c.Batch = append([]int(nil), StandardBatch...)
	}
	if c.BatchPP <= 0 {
		c.BatchPP = 1024
	}
	if c.Reps <= 0 {
		c.Reps = 1
	}
	sort.Ints(c.PP)
	sort.Ints(c.Batch)
}

// Single is one prompt length, one request at a time.
type Single struct {
	Test         string  `json:"test"`          // pp4096/tg128
	PromptTokens int     `json:"prompt_tokens"` // as the engine counted them
	TTFTMs       float64 `json:"ttft_ms"`
	TPOTMs       float64 `json:"tpot_ms"`
	PPTPS        float64 `json:"pp_tps"`
	TGTPS        float64 `json:"tg_tps"`
	E2ES         float64 `json:"e2e_s"`
	Throughput   float64 `json:"throughput_tps"` // (prompt + generated) / end to end
	PeakMemMB    int     `json:"peak_mem_mb,omitempty"`
	Error        string  `json:"error,omitempty"`
}

// Batch is n requests at once.
type Batch struct {
	N       int     `json:"n"`
	TGTPS   float64 `json:"tg_tps"`  // generated tokens across all, per second of generation
	Speedup float64 `json:"speedup"` // TGTPS against one request alone
	PPTPS   float64 `json:"pp_tps"`  // prompt tokens read across all, per second of prefill
	PPPerRq float64 `json:"pp_tps_per_request"`
	// CachedPct is the share of prompt tokens served from the cache: what
	// "same prompt" is meant to show, measured rather than assumed.
	CachedPct float64 `json:"cached_pct"`
	AvgTTFTMs float64 `json:"avg_ttft_ms"`
	E2ES      float64 `json:"e2e_s"` // the last one to finish
	PeakMemMB int     `json:"peak_mem_mb,omitempty"`
	Error     string  `json:"error,omitempty"`
}

// Report is a run, with everything needed to run it again.
type Report struct {
	Node    string     `json:"node"`
	Model   string     `json:"model"`
	At      time.Time  `json:"at"`
	Config  Config     `json:"config"`
	Corpus  *Corpus    `json:"corpus"`
	Machine Machine    `json:"machine"`
	Engine  EngineInfo `json:"engine"`
	Single  []Single   `json:"single"`
	Same    []Batch    `json:"batch_same_prompt"`
	Diff    []Batch    `json:"batch_different_prompts"`
	Command string     `json:"command"`
	Notes   []string   `json:"notes,omitempty"`
	Took    float64    `json:"took_s"`
	Phase   string     `json:"phase,omitempty"` // while running: what it is doing
	Partial bool       `json:"partial,omitempty"`
}

// Machine is the hardware and system the numbers belong to.
type Machine struct {
	OS       string `json:"os,omitempty"`
	GPU      string `json:"gpu,omitempty"`
	Driver   string `json:"driver,omitempty"`
	VRAMMB   int    `json:"vram_mb,omitempty"`
	Platform string `json:"platform,omitempty"`
	Version  string `json:"modelfabric,omitempty"`
}

// EngineInfo is what ran the model: the build and every setting it loaded
// with, per phase, and the model file.
type EngineInfo struct {
	Runtime string         `json:"runtime,omitempty"`
	File    string         `json:"file,omitempty"`
	Quant   string         `json:"quant,omitempty"`
	SizeMB  int            `json:"size_mb,omitempty"`
	Single  map[string]any `json:"single_load,omitempty"` // the load the prompt sweep ran on
	Batched map[string]any `json:"batch_load,omitempty"`  // and the batching tests
	Timings string         `json:"timings"`               // "engine" or "client"
}

// Engine is the node's model, for the length of a run.
type Engine interface {
	tuner.Engine
	// Loaded describes what is serving the model now: its process (for
	// memory), its build and every setting it loaded with.
	Loaded(ctx context.Context, model string) (pid int, runtime string, settings map[string]any, err error)
}

// Memory samples a process's GPU memory in MiB. Nil, or an error, means it
// cannot be known here, and the report leaves the column empty.
type Memory func(pid int) (int, error)

// Run measures cfg on e. progress, when set, gets the report as it fills.
func Run(ctx context.Context, e Engine, mem Memory, cfg Config, progress func(Report)) (Report, error) {
	cfg.Fill()
	start := time.Now()
	rep := Report{Model: cfg.Model, At: start, Config: cfg}
	corpus, err := LoadCorpus(cfg.Prompts)
	if err != nil {
		return rep, err
	}
	rep.Corpus = corpus
	emit := func(phase string) {
		rep.Phase = phase
		rep.Took = time.Since(start).Seconds()
		if progress != nil {
			// Copies of the rows: the receiver reads the report on another
			// goroutine while this one goes on appending to it.
			cp := rep
			cp.Single, cp.Same, cp.Diff = slices.Clone(rep.Single), slices.Clone(rep.Same), slices.Clone(rep.Diff)
			cp.Notes = slices.Clone(rep.Notes)
			progress(cp)
		}
	}

	if err := tuner.Idle(ctx, e, cfg.Model); err != nil {
		return rep, err
	}
	origSlots, origCtx, err := e.Current(ctx, cfg.Model)
	if err != nil {
		return rep, err
	}
	// Put back what was found, whatever happens below: a cancelled run must
	// not leave the node on a benchmark's settings.
	defer func() {
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Minute)
		defer cancel()
		if err := e.Reload(rctx, cfg.Model, origCtx, origSlots); err != nil {
			rep.Notes = append(rep.Notes, "could not restore the engine to "+fmt.Sprint(origSlots)+" slots at "+fmt.Sprint(origCtx)+" context: "+err.Error())
		}
	}()

	// Phase 1: one slot with room for the longest prompt.
	longest := cfg.PP[len(cfg.PP)-1]
	singleCtx := roundUp(longest+cfg.TG+512, 1024)
	emit(fmt.Sprintf("loading with one slot of %d tokens", singleCtx))
	if err := e.Reload(ctx, cfg.Model, singleCtx, 1); err != nil {
		return rep, fmt.Errorf("load for the prompt sweep (1 slot × %d context): %w", singleCtx, err)
	}
	c, pid, err := connect(ctx, e, cfg.Model, &rep.Engine.Runtime, &rep.Engine.Single)
	if err != nil {
		return rep, err
	}
	pb := &prompter{c: c, corpus: corpus}
	if err := pb.calibrate(ctx); err != nil {
		return rep, err
	}
	warm(ctx, c, pb, cfg.TG)
	var baseline float64 // tg tok/s of one request at BatchPP, for the speedups
	for _, pp := range cfg.PP {
		if ctx.Err() != nil {
			break
		}
		emit(fmt.Sprintf("pp%d/tg%d", pp, cfg.TG))
		row := Single{Test: fmt.Sprintf("pp%d/tg%d", pp, cfg.TG)}
		var runs []sample
		peak := watchMemory(mem, pid)
		for r := 0; r < cfg.Reps; r++ {
			prompt, err := pb.build(ctx, pp, fmt.Sprintf("single %d rep %d", pp, r))
			if err != nil {
				row.Error = err.Error()
				break
			}
			s := c.generate(ctx, prompt, cfg.TG)
			if s.err != nil {
				row.Error = s.err.Error()
				break
			}
			runs = append(runs, s)
		}
		row.PeakMemMB = peak()
		if len(runs) > 0 {
			row.fill(runs)
			rep.Engine.Timings = timingSource(runs)
		}
		rep.Single = append(rep.Single, row)
		if pp == cfg.BatchPP && row.Error == "" {
			baseline = row.TGTPS
		}
		emit(row.Test)
	}

	// Phase 2: as many slots as the largest batch, each short.
	if len(cfg.Batch) > 0 && ctx.Err() == nil {
		maxN := cfg.Batch[len(cfg.Batch)-1]
		batchCtx := roundUp(cfg.BatchPP+cfg.TG+512, 1024)
		emit(fmt.Sprintf("loading with %d slots of %d tokens", maxN, batchCtx))
		if err := e.Reload(ctx, cfg.Model, batchCtx, maxN); err != nil {
			rep.Notes = append(rep.Notes, fmt.Sprintf("batching skipped: the engine would not load %d slots × %d context: %v", maxN, batchCtx, err))
		} else if c, pid, err = connect(ctx, e, cfg.Model, &rep.Engine.Runtime, &rep.Engine.Batched); err != nil {
			rep.Notes = append(rep.Notes, "batching skipped: "+err.Error())
		} else {
			pb.c = c
			warm(ctx, c, pb, cfg.TG)
			// The 1x baseline on this load, so speedups compare like with like.
			one := c.generate(ctx, mustBuild(ctx, pb, cfg.BatchPP, "batch baseline"), cfg.TG)
			if one.err == nil {
				baseline = one.tgTPS()
			}
			for _, mode := range []string{"same", "different"} {
				for _, n := range cfg.Batch {
					if ctx.Err() != nil {
						break
					}
					emit(fmt.Sprintf("%dx %s prompt", n, mode))
					b := runBatch(ctx, c, pb, mem, pid, mode, n, cfg)
					if baseline > 0 {
						b.Speedup = b.TGTPS / baseline
					}
					if mode == "same" {
						rep.Same = append(rep.Same, b)
					} else {
						rep.Diff = append(rep.Diff, b)
					}
					emit(fmt.Sprintf("%dx %s prompt", n, mode))
				}
			}
		}
	}
	rep.Partial = ctx.Err() != nil
	rep.Took = time.Since(start).Seconds()
	rep.Phase = ""
	return rep, nil
}

func (r *Single) fill(runs []sample) {
	r.PromptTokens = runs[0].promptTokens
	r.TTFTMs = median(runs, sample.ttftMs)
	r.TPOTMs = median(runs, sample.tpotMs)
	r.PPTPS = median(runs, sample.ppTPS)
	r.TGTPS = median(runs, sample.tgTPS)
	r.E2ES = median(runs, sample.e2e)
	r.Throughput = median(runs, func(s sample) float64 {
		if e := s.e2e(); e > 0 {
			return float64(s.promptTokens+s.outTokens) / e
		}
		return 0
	})
}

// runBatch sends n requests at once. "same" warms one prompt and sends it n
// times: a shared system prompt, whose prefix the engine can reuse. "different"
// gives each its own opening, so nothing is shared.
func runBatch(ctx context.Context, c *client, pb *prompter, mem Memory, pid int, mode string, n int, cfg Config) Batch {
	b := Batch{N: n}
	prompts := make([]string, n)
	shared := ""
	if mode == "same" {
		var err error
		if shared, err = pb.build(ctx, cfg.BatchPP, fmt.Sprintf("shared %d", n)); err != nil {
			b.Error = err.Error()
			return b
		}
		// Read once first, as a shared prompt would have been by an earlier
		// request: the batch then measures reusing it, not the first read.
		if s := c.generate(ctx, shared, 1); s.err != nil {
			b.Error = s.err.Error()
			return b
		}
	}
	for i := range prompts {
		if mode == "same" {
			prompts[i] = shared
			continue
		}
		p, err := pb.build(ctx, cfg.BatchPP, fmt.Sprintf("batch %d request %d", n, i))
		if err != nil {
			b.Error = err.Error()
			return b
		}
		prompts[i] = p
	}
	peak := watchMemory(mem, pid)
	out := make([]sample, n)
	var wg sync.WaitGroup
	for i := range prompts {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			out[i] = c.generate(ctx, prompts[i], cfg.TG)
		}(i)
	}
	wg.Wait()
	b.PeakMemMB = peak()
	var start, firstFirst, lastFirst, lastEnd time.Time
	var gen, read, prompt, cached int
	var ttft float64
	for i, s := range out {
		if s.err != nil {
			b.Error = s.err.Error()
			return b
		}
		if i == 0 || s.start.Before(start) {
			start = s.start
		}
		if i == 0 || s.first.Before(firstFirst) {
			firstFirst = s.first
		}
		if s.first.After(lastFirst) {
			lastFirst = s.first
		}
		if s.end.After(lastEnd) {
			lastEnd = s.end
		}
		gen += s.outTokens
		read += s.promptTokens - s.cachedTokens
		prompt += s.promptTokens
		cached += s.cachedTokens
		ttft += s.ttftMs()
	}
	// Generation runs from the first token anywhere to the last token
	// anywhere; prefill from the start to the last request's first token.
	if t := lastEnd.Sub(firstFirst).Seconds(); t > 0 {
		b.TGTPS = float64(gen) / t
	}
	if t := lastFirst.Sub(start).Seconds(); t > 0 {
		b.PPTPS = float64(read) / t
		b.PPPerRq = b.PPTPS / float64(n)
	}
	if prompt > 0 {
		b.CachedPct = 100 * float64(cached) / float64(prompt)
	}
	b.AvgTTFTMs = ttft / float64(n)
	b.E2ES = lastEnd.Sub(start).Seconds()
	return b
}

func connect(ctx context.Context, e Engine, model string, runtime *string, settings *map[string]any) (*client, int, error) {
	base, served, err := e.Target(ctx, model)
	if err != nil {
		return nil, 0, err
	}
	pid, rt, s, err := e.Loaded(ctx, model)
	if err == nil {
		*runtime, *settings = rt, s
	}
	return newClient(base, served), pid, nil
}

// warm sends one short request the results do not count: the first request
// after a load pays for setting up the GPU's work, which is not the model's
// speed.
func warm(ctx context.Context, c *client, pb *prompter, tg int) {
	if p, err := pb.build(ctx, 256, "warm-up"); err == nil {
		c.generate(ctx, p, min(tg, 16))
	}
}

func mustBuild(ctx context.Context, pb *prompter, n int, tag string) string {
	p, _ := pb.build(ctx, n, tag)
	return p
}

func timingSource(runs []sample) string {
	for _, s := range runs {
		if !s.engineTimed {
			return "client"
		}
	}
	return "engine"
}

// watchMemory samples pid's GPU memory until the returned function is called,
// which returns the peak. Zero when it cannot be known.
func watchMemory(mem Memory, pid int) func() int {
	if mem == nil || pid <= 0 {
		return func() int { return 0 }
	}
	stop := make(chan struct{})
	done := make(chan int)
	go func() {
		peak := 0
		t := time.NewTicker(250 * time.Millisecond)
		defer t.Stop()
		for {
			if mb, err := mem(pid); err == nil && mb > peak {
				peak = mb
			}
			select {
			case <-stop:
				done <- peak
				return
			case <-t.C:
			}
		}
	}()
	return func() int {
		close(stop)
		return <-done
	}
}

func median(runs []sample, f func(sample) float64) float64 {
	v := make([]float64, 0, len(runs))
	for _, s := range runs {
		v = append(v, f(s))
	}
	sort.Float64s(v)
	m := v[len(v)/2]
	if len(v)%2 == 0 {
		m = (v[len(v)/2-1] + v[len(v)/2]) / 2
	}
	return math.Round(m*100) / 100
}

func roundUp(n, to int) int { return (n + to - 1) / to * to }
