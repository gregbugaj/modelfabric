package bench

import (
	"fmt"
	"sort"
	"strings"
)

func (r Report) Text() string {
	var b strings.Builder
	line := strings.Repeat("=", 78)
	fmt.Fprintf(&b, "ModelFabric benchmark: %s on %s\n%s\n", r.Model, r.Node, line)
	if r.Machine.GPU != "" {
		fmt.Fprintf(&b, "GPU      %s, %d MiB", r.Machine.GPU, r.Machine.VRAMMB)
		if r.Machine.Driver != "" {
			fmt.Fprintf(&b, " (driver %s)", r.Machine.Driver)
		}
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "System   %s %s, ModelFabric %s\n", r.Machine.OS, r.Machine.Platform, r.Machine.Version)
	fmt.Fprintf(&b, "Engine   %s\n", r.Engine.Runtime)
	fmt.Fprintf(&b, "Model    %s %s (%d MiB)\n", r.Engine.File, r.Engine.Quant, r.Engine.SizeMB)
	if r.Corpus != nil {
		fmt.Fprintf(&b, "Prompts  %s-%s (sha256 %s): %q\n", r.Corpus.Name, r.Corpus.Version, r.Corpus.SHA256[:16], r.Corpus.Ask)
	}
	fmt.Fprintf(&b, "Timings  from the %s; %d run(s) per test, median\n", or(r.Engine.Timings, "engine"), r.Config.Reps)
	fmt.Fprintf(&b, "Run      %s, took %.0fs\n", r.At.Format("2006-01-02 15:04 MST"), r.Took)

	if len(r.Single) > 0 {
		fmt.Fprintf(&b, "\nSingle request\n%s\n", strings.Repeat("-", 78))
		fmt.Fprintf(&b, "%-15s %9s %9s %10s %9s %8s %11s %10s\n", "Test", "TTFT(ms)", "TPOT(ms)", "pp TPS", "tg TPS", "E2E(s)", "Throughput", "Peak mem")
		for _, s := range r.Single {
			if s.Error != "" {
				fmt.Fprintf(&b, "%-15s %s\n", s.Test, s.Error)
				continue
			}
			fmt.Fprintf(&b, "%-15s %9.1f %9.2f %10.1f %9.1f %8.3f %11.1f %10s\n",
				s.Test, s.TTFTMs, s.TPOTMs, s.PPTPS, s.TGTPS, s.E2ES, s.Throughput, mem(s.PeakMemMB))
		}
	}
	batch := func(title string, rows []Batch) {
		if len(rows) == 0 {
			return
		}
		fmt.Fprintf(&b, "\n%s (pp%d / tg%d)\n%s\n", title, r.Config.BatchPP, r.Config.TG, strings.Repeat("-", 78))
		fmt.Fprintf(&b, "%-6s %10s %8s %10s %11s %8s %10s %8s\n", "Batch", "tg TPS", "Speedup", "pp TPS", "pp TPS/req", "Cached", "Avg TTFT", "E2E(s)")
		for _, x := range rows {
			if x.Error != "" {
				fmt.Fprintf(&b, "%-6s %s\n", fmt.Sprintf("%dx", x.N), x.Error)
				continue
			}
			fmt.Fprintf(&b, "%-6s %10.1f %7.2fx %10.1f %11.1f %7.0f%% %10.1f %8.3f\n",
				fmt.Sprintf("%dx", x.N), x.TGTPS, x.Speedup, x.PPTPS, x.PPPerRq, x.CachedPct, x.AvgTTFTMs, x.E2ES)
		}
	}
	batch("Continuous batching, same prompt", r.Same)
	batch("Continuous batching, different prompts", r.Diff)

	for _, n := range r.Notes {
		fmt.Fprintf(&b, "\nNote: %s\n", n)
	}
	if r.Partial {
		b.WriteString("\nStopped before the end: these are partial results.\n")
	}
	fmt.Fprintf(&b, "\nRepeat with:\n  %s\n", r.Command)
	return b.String()
}

func mem(mb int) string {
	if mb <= 0 {
		return "-"
	}
	return fmt.Sprintf("%.2f GB", float64(mb)/1024)
}

func or(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// Compare puts several nodes' reports side by side: generation speed at each
// prompt length and batch, and prompt reading at each length, which together
// say where a model runs best. The CLI's -fleet and the dashboard's Cluster
// benchmark print the same text.
func Compare(reps []Report) string {
	if len(reps) == 0 {
		return ""
	}
	var b strings.Builder
	cell := func(v float64, ok bool) string {
		if !ok {
			return "-"
		}
		return fmt.Sprintf("%.1f", v)
	}
	head := func(title string) {
		fmt.Fprintf(&b, "\n%s\n%s\n%-17s", title, strings.Repeat("-", 78), "Test")
		for _, r := range reps {
			fmt.Fprintf(&b, " %16s", r.Node)
		}
		b.WriteString("\n")
	}
	head(fmt.Sprintf("Across the cluster: %s, tg tok/s (generation)", reps[0].Model))
	for i, s := range reps[0].Single {
		fmt.Fprintf(&b, "%-17s", s.Test)
		for _, r := range reps {
			ok := i < len(r.Single) && r.Single[i].Error == ""
			v := 0.0
			if ok {
				v = r.Single[i].TGTPS
			}
			fmt.Fprintf(&b, " %16s", cell(v, ok))
		}
		b.WriteString("\n")
	}
	for _, mode := range []struct {
		label string
		rows  func(Report) []Batch
	}{{"different", func(r Report) []Batch { return r.Diff }}, {"same prompt", func(r Report) []Batch { return r.Same }}} {
		for i, x := range mode.rows(reps[0]) {
			fmt.Fprintf(&b, "%-17s", fmt.Sprintf("%dx %s", x.N, mode.label))
			for _, r := range reps {
				rows := mode.rows(r)
				ok := i < len(rows) && rows[i].Error == ""
				v := 0.0
				if ok {
					v = rows[i].TGTPS
				}
				fmt.Fprintf(&b, " %16s", cell(v, ok))
			}
			b.WriteString("\n")
		}
	}
	head("Prompt reading, pp tok/s")
	for i, s := range reps[0].Single {
		fmt.Fprintf(&b, "%-17s", s.Test)
		for _, r := range reps {
			ok := i < len(r.Single) && r.Single[i].Error == ""
			v := 0.0
			if ok {
				v = r.Single[i].PPTPS
			}
			fmt.Fprintf(&b, " %16s", cell(v, ok))
		}
		b.WriteString("\n")
	}
	return b.String()
}

func (r ClusterReport) Text() string {
	var b strings.Builder
	line := strings.Repeat("=", 78)
	fmt.Fprintf(&b, "ModelFabric cluster benchmark: %s, through %s's front door\n%s\n", r.Model, r.Entry, line)
	fmt.Fprintf(&b, "Routing  %s\n", r.Routing)
	for _, h := range r.Holders {
		// 0 is "not reported" (a peer's external engine), not none.
		eng, slots := "engines not reported", ""
		if h.Engines > 0 {
			eng = fmt.Sprintf("%d engine(s)", h.Engines)
		}
		if h.Slots > 0 {
			slots = fmt.Sprintf(", %d slot(s)", h.Slots)
		}
		fmt.Fprintf(&b, "Node     %s %s, %s%s\n", h.Node, h.Platform, eng, slots)
	}
	if r.Corpus != nil {
		fmt.Fprintf(&b, "Prompts  %s-%s (sha256 %s)\n", r.Corpus.Name, r.Corpus.Version, r.Corpus.SHA256[:16])
	}
	fmt.Fprintf(&b, "Timings  from the %s; %d run(s) per single test, median\n", or(r.Timings, "engine"), r.Config.Reps)
	fmt.Fprintf(&b, "Run      %s, took %.0fs\n", r.At.Format("2006-01-02 15:04 MST"), r.Took)
	if len(r.Single) > 0 {
		fmt.Fprintf(&b, "\nOne request at a time\n%s\n", strings.Repeat("-", 78))
		fmt.Fprintf(&b, "%-15s %9s %10s %9s %8s  %s\n", "Test", "TTFT(ms)", "pp TPS", "tg TPS", "E2E(s)", "Served by")
		for _, s := range r.Single {
			if s.Error != "" {
				fmt.Fprintf(&b, "%-15s %s\n", s.Test, s.Error)
				continue
			}
			fmt.Fprintf(&b, "%-15s %9.1f %10.1f %9.1f %8.3f  %s\n", s.Test, s.TTFTMs, s.PPTPS, s.TGTPS, s.E2ES, s.Node)
		}
	}
	load := func(title string, rows []Load) {
		if len(rows) == 0 {
			return
		}
		fmt.Fprintf(&b, "\n%s (pp%d / tg%d)\n%s\n", title, r.Config.LoadPP, r.Config.TG, strings.Repeat("-", 78))
		fmt.Fprintf(&b, "%-7s %9s %8s %10s %10s %10s %7s  %s\n", "At once", "tg TPS", "Speedup", "Avg TTFT", "p95 TTFT", "E2E(s)", "Cached", "Spread")
		for _, l := range rows {
			if l.Error != "" && len(l.Spread) == 0 {
				fmt.Fprintf(&b, "%-7d %s\n", l.N, l.Error)
				continue
			}
			fmt.Fprintf(&b, "%-7d %9.1f %7.2fx %10.1f %10.1f %10.3f %6.0f%%  %s\n",
				l.N, l.TGTPS, l.Speedup, l.AvgTTFTMs, l.P95TTFTMs, l.E2ES, l.CachedPct, spread(l.Spread))
			if l.Failed > 0 {
				fmt.Fprintf(&b, "        %d failed: %s\n", l.Failed, l.Error)
			}
		}
	}
	load("The cluster under load, each request its own prompt", r.Load)
	load("The cluster under load, one prompt shared", r.Shared)
	for _, n := range r.Notes {
		fmt.Fprintf(&b, "\nNote: %s\n", n)
	}
	if r.Partial {
		b.WriteString("\nStopped before the end: these are partial results.\n")
	}
	if r.Command != "" {
		fmt.Fprintf(&b, "\nRepeat with:\n  %s\n", r.Command)
	}
	return b.String()
}

// spread is "minion 6 · helion 2", busiest first.
func spread(m map[string]int) string {
	type kv struct {
		k string
		v int
	}
	var s []kv
	for k, v := range m {
		s = append(s, kv{k, v})
	}
	sort.Slice(s, func(i, j int) bool { return s[i].v > s[j].v || (s[i].v == s[j].v && s[i].k < s[j].k) })
	parts := make([]string, len(s))
	for i, x := range s {
		parts[i] = fmt.Sprintf("%s %d", x.k, x.v)
	}
	return strings.Join(parts, " · ")
}
