package bench

import (
	"context"
	"fmt"
	"math"
	"slices"
	"sort"
	"sync"
	"time"
)

// Cluster benchmarks send requests through the inference front door and record
// which nodes served them. Engines retain their current configuration.

// ClusterConfig is what to run. Zero values are the standard cluster suite.
type ClusterConfig struct {
	Model   string `json:"model"`
	Prompts string `json:"prompts"`
	// PP are single requests at these prompt lengths, one at a time.
	PP []int `json:"pp"`
	TG int   `json:"tg"`
	// Concurrency are the levels of the load sweep, each at LoadPP tokens.
	Concurrency []int `json:"concurrency"`
	LoadPP      int   `json:"load_pp"`
	Reps        int   `json:"reps"`
}

var (
	StandardClusterPP          = []int{1024, 4096, 16384}
	StandardClusterConcurrency = []int{1, 2, 4, 8, 16}
)

func (c *ClusterConfig) Fill() {
	if c.Prompts == "" {
		c.Prompts = "prose"
	}
	if c.PP == nil {
		c.PP = append([]int(nil), StandardClusterPP...)
	}
	if c.TG <= 0 {
		c.TG = 128
	}
	if len(c.Concurrency) == 0 {
		c.Concurrency = append([]int(nil), StandardClusterConcurrency...)
	}
	if c.LoadPP <= 0 {
		c.LoadPP = 1024
	}
	if c.Reps <= 0 {
		c.Reps = 1
	}
	sort.Ints(c.PP)
	sort.Ints(c.Concurrency)
}

type ClusterSingle struct {
	Single
	Node string `json:"node"`
}

type Load struct {
	N         int     `json:"n"`
	TGTPS     float64 `json:"tg_tps"`  // generated tokens, all requests, per second of generation
	Speedup   float64 `json:"speedup"` // against one request at a time
	PPTPS     float64 `json:"pp_tps"`
	AvgTTFTMs float64 `json:"avg_ttft_ms"`
	P95TTFTMs float64 `json:"p95_ttft_ms"`
	E2ES      float64 `json:"e2e_s"`
	CachedPct float64 `json:"cached_pct"`
	// Spread is how many of the requests each node served: whether the
	// cluster shared the work, and how.
	Spread map[string]int `json:"spread"`
	Failed int            `json:"failed,omitempty"`
	Error  string         `json:"error,omitempty"`
}

type ClusterReport struct {
	Model   string          `json:"model"`
	At      time.Time       `json:"at"`
	Config  ClusterConfig   `json:"config"`
	Corpus  *Corpus         `json:"corpus"`
	Entry   string          `json:"entry"`   // the node whose front door took the requests
	Routing string          `json:"routing"` // "ModelFabric router", or "llm-d (<profile>)"
	Holders []ClusterNode   `json:"holders"` // the nodes holding the model when the run began
	Single  []ClusterSingle `json:"single"`
	Load    []Load          `json:"load"`
	Shared  []Load          `json:"load_shared_prompt"`
	Timings string          `json:"timings"`
	Command string          `json:"command"`
	Notes   []string        `json:"notes,omitempty"`
	Took    float64         `json:"took_s"`
	Phase   string          `json:"phase,omitempty"`
	Partial bool            `json:"partial,omitempty"`
}

type ClusterNode struct {
	Node     string `json:"node"`
	Platform string `json:"platform,omitempty"`
	Engines  int    `json:"engines"`
	Slots    int    `json:"slots,omitempty"`
}

// RunCluster sends the suite through the front door at base. key, when set,
// is the bearer token the front door asks for.
func RunCluster(ctx context.Context, base, key string, cfg ClusterConfig, rep ClusterReport, progress func(ClusterReport)) (ClusterReport, error) {
	cfg.Fill()
	start := time.Now()
	rep.Model, rep.Config, rep.At = cfg.Model, cfg, start
	corpus, err := LoadCorpus(cfg.Prompts)
	if err != nil {
		return rep, err
	}
	rep.Corpus = corpus
	emit := func(phase string) {
		rep.Phase, rep.Took = phase, time.Since(start).Seconds()
		if progress != nil {
			cp := rep
			cp.Single, cp.Load, cp.Shared = slices.Clone(rep.Single), slices.Clone(rep.Load), slices.Clone(rep.Shared)
			progress(cp)
		}
	}
	c := newClient(base, cfg.Model)
	c.key = key
	// Through the front door the engine's tokenizer is not reachable, so the
	// lengths are estimated from the text and the report states the prompt
	// length each request actually had, as the engine counted it.
	pb := &prompter{c: c, corpus: corpus}
	emit("checking the front door answers")
	if err := pb.calibrate(ctx); err != nil {
		return rep, err
	}
	warm(ctx, c, pb, cfg.TG)
	timed := true

	for _, pp := range cfg.PP {
		if ctx.Err() != nil {
			break
		}
		emit(fmt.Sprintf("pp%d/tg%d, one request", pp, cfg.TG))
		row := ClusterSingle{Single: Single{Test: fmt.Sprintf("pp%d/tg%d", pp, cfg.TG)}}
		var runs []sample
		for r := 0; r < cfg.Reps; r++ {
			prompt, _ := pb.build(ctx, pp, fmt.Sprintf("cluster single %d rep %d", pp, r))
			s := c.generate(ctx, prompt, cfg.TG)
			if s.err != nil {
				row.Error = s.err.Error()
				break
			}
			runs = append(runs, s)
		}
		if len(runs) > 0 {
			row.fill(runs)
			row.Node = runs[len(runs)-1].node
			timed = timed && timingSource(runs) == "engine"
		}
		rep.Single = append(rep.Single, row)
	}

	var baseline float64
	for _, mode := range []string{"different", "shared"} {
		for _, n := range cfg.Concurrency {
			if ctx.Err() != nil {
				break
			}
			emit(fmt.Sprintf("%d at once, %s prompts", n, mode))
			l := runLoad(ctx, c, pb, mode, n, cfg)
			if n == 1 && mode == "different" && l.Error == "" {
				baseline = l.TGTPS
			}
			if baseline > 0 && l.Error == "" {
				l.Speedup = math.Round(l.TGTPS/baseline*100) / 100
			}
			if mode == "different" {
				rep.Load = append(rep.Load, l)
			} else {
				rep.Shared = append(rep.Shared, l)
			}
		}
	}
	rep.Timings = "engine"
	if !timed {
		rep.Timings = "client"
	}
	rep.Partial = ctx.Err() != nil
	rep.Took, rep.Phase = time.Since(start).Seconds(), ""
	return rep, nil
}

// runLoad sends n requests at once through the front door. "shared" sends
// one prompt n times, read once first, as a shared system prompt is; the
// router's prefix affinity and every engine's cache get their chance.
func runLoad(ctx context.Context, c *client, pb *prompter, mode string, n int, cfg ClusterConfig) Load {
	l := Load{N: n, Spread: map[string]int{}}
	prompts := make([]string, n)
	if mode == "shared" {
		shared, err := pb.build(ctx, cfg.LoadPP, fmt.Sprintf("cluster shared %d", n))
		if err != nil {
			l.Error = err.Error()
			return l
		}
		c.generate(ctx, shared, 1)
		for i := range prompts {
			prompts[i] = shared
		}
	} else {
		for i := range prompts {
			p, err := pb.build(ctx, cfg.LoadPP, fmt.Sprintf("cluster load %d request %d", n, i))
			if err != nil {
				l.Error = err.Error()
				return l
			}
			prompts[i] = p
		}
	}
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

	var ok []sample
	for _, s := range out {
		if s.err != nil {
			l.Failed++
			l.Error = s.err.Error()
			continue
		}
		ok = append(ok, s)
		node := s.node
		if node == "" {
			node = "unknown"
		}
		l.Spread[node]++
	}
	if len(ok) == 0 {
		return l
	}
	if l.Failed == 0 {
		l.Error = ""
	}
	var start, firstFirst, lastFirst, lastEnd time.Time
	var gen, read, prompt, cached int
	ttfts := make([]float64, 0, len(ok))
	for i, s := range ok {
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
		ttfts = append(ttfts, s.ttftMs())
	}
	if t := lastEnd.Sub(firstFirst).Seconds(); t > 0 {
		l.TGTPS = round1(float64(gen) / t)
	}
	if t := lastFirst.Sub(start).Seconds(); t > 0 {
		l.PPTPS = round1(float64(read) / t)
	}
	if prompt > 0 {
		l.CachedPct = round1(100 * float64(cached) / float64(prompt))
	}
	sort.Float64s(ttfts)
	sum := 0.0
	for _, v := range ttfts {
		sum += v
	}
	l.AvgTTFTMs = round1(sum / float64(len(ttfts)))
	l.P95TTFTMs = round1(ttfts[min(len(ttfts)-1, int(math.Ceil(0.95*float64(len(ttfts))))-1)])
	l.E2ES = math.Round(lastEnd.Sub(start).Seconds()*1000) / 1000
	return l
}

func round1(v float64) float64 { return math.Round(v*10) / 10 }
