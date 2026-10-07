package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/gregbugaj/modelfabric/internal/bench"
)

// `mfsh bench` runs the standard benchmark through the measured node's
// /api/v1/bench endpoint, excluding CLI network latency. The node owns the run
// and restores engine settings if the CLI disconnects. Peers use the owner-only proxy.
//
// -cluster measures requests routed through this node across the mesh without
// reloading engines (bench.RunCluster).

func benchCmd(args []string) error {
	fs := flag.NewFlagSet("bench", flag.ExitOnError)
	addr := fs.String("addr", defaultAddr, "address of the local ModelFabric node")
	node := fs.String("node", "", "the node to benchmark (default: this one)")
	fleet := fs.Bool("fleet", false, "benchmark every node serving the model, one after another, and compare them")
	cluster := fs.Bool("cluster", false, "benchmark the whole cluster through this node's front door, at rising concurrency")
	concList := fs.String("concurrency", "", "with -cluster: requests at once (default: 1,2,4,8,16)")
	loadPP := fs.Int("load-pp", 1024, "with -cluster: prompt length for the load tests")
	model := fs.String("model", "", "the model (default: the one loaded on the node)")
	prompts := fs.String("prompts", "prose", "prompt set: prose (docs, summarised) or code (Go, continued)")
	ppList := fs.String("pp", "", "prompt lengths in tokens (default: 1024,4096,8192,16384,32768; with -cluster 1024,4096,16384)")
	tg := fs.Int("tg", 128, "tokens generated per request, exactly")
	batchList := fs.String("batch", "", "concurrency levels (default: 2,4,8; \"none\" to skip)")
	batchPP := fs.Int("batch-pp", 1024, "prompt length for the batching tests")
	reps := fs.Int("reps", 1, "runs per test; the median is reported")
	quick := fs.Bool("quick", false, "a short run: 1024 and 4096 tokens, batches of 2 and 4")
	out := fs.String("out", "", "where to save the report (default: ~/.local/share/modelfabric/bench)")
	yes := fs.Bool("y", false, "do not ask before a fleet run")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *model == "" && fs.NArg() > 0 {
		*model = fs.Arg(0)
	}
	cfg := bench.Config{Model: *model, Prompts: *prompts, TG: *tg, BatchPP: *batchPP, Reps: *reps}
	var err error
	if cfg.PP, err = intList(*ppList, "-pp"); err != nil {
		return err
	}
	switch *batchList {
	case "none":
		cfg.Batch = []int{}
	default:
		if cfg.Batch, err = intList(*batchList, "-batch"); err != nil {
			return err
		}
	}
	if *quick {
		cfg.PP, cfg.Batch = []int{1024, 4096}, []int{2, 4}
	}
	if *cluster {
		if *fleet || *node != "" {
			return fmt.Errorf("-cluster measures the cluster as one system, so it takes no -node or -fleet")
		}
		cc := bench.ClusterConfig{Model: cfg.Model, Prompts: cfg.Prompts, PP: cfg.PP, TG: cfg.TG, LoadPP: *loadPP, Reps: cfg.Reps}
		if cc.Concurrency, err = intList(*concList, "-concurrency"); err != nil {
			return err
		}
		if *quick {
			cc.PP, cc.Concurrency = []int{1024}, []int{1, 2, 4}
		}
		return benchCluster(*addr, cc, *out)
	}
	if err := mustBeLocalBecause(*addr, "benchmarking",
		"reloads that node's engine, which is not something another node may do", "bench"); err != nil {
		return err
	}
	if err := ensureNode(*addr); err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if cfg.Model == "" {
		if cfg.Model = modelHere(ctx, *addr); cfg.Model == "" && !*fleet && *node == "" {
			return fmt.Errorf("no model is loaded here and none was named: `mfsh bench -model <model>`")
		}
	}

	nodes := []string{*node}
	if *fleet {
		if cfg.Model == "" {
			return fmt.Errorf("a fleet benchmark needs the model named: `mfsh bench -fleet -model <model>`")
		}
		if nodes, err = fleetNodes(ctx, *addr, cfg.Model); err != nil {
			return err
		}
		if !*yes {
			fmt.Printf("\n  This reloads %s's engine on %s, one after another.\n", cfg.Model, strings.Join(nodes, ", "))
			fmt.Printf("  %s\n", dim("Requests routed to a node while it runs will fail or wait."))
			fmt.Printf("\n  Benchmark? %s ", dim("[y/N]"))
			var a string
			_, _ = fmt.Fscanln(os.Stdin, &a)
			if a = strings.ToLower(strings.TrimSpace(a)); a != "y" && a != "yes" {
				fmt.Println("  nothing was changed")
				return nil
			}
		}
	}

	var reports []bench.Report
	for _, n := range nodes {
		rep, err := benchOne(ctx, *addr, n, cfg)
		if err != nil {
			return fmt.Errorf("%s: %w", or(n, "this node"), err)
		}
		fmt.Println()
		fmt.Print(rep.Text())
		if path, err := saveBench(rep, *out); err == nil {
			fmt.Printf("\n%s %s\n", dim("saved"), path)
		} else {
			fmt.Fprintln(os.Stderr, "could not save the report:", err)
		}
		reports = append(reports, rep)
		if ctx.Err() != nil {
			break
		}
	}
	if len(reports) > 1 {
		fmt.Print(bench.Compare(reports))
	}
	return nil
}

// benchOne starts the run on a node, prints its progress, and returns the
// report. Interrupted, it stops the node's run, which puts the engine back.
func benchOne(ctx context.Context, addr, node string, cfg bench.Config) (bench.Report, error) {
	path := "/api/v1/bench"
	if node != "" {
		path = "/api/v1/nodes/" + node + "/bench"
	}
	var st struct {
		Running bool          `json:"running"`
		Report  *bench.Report `json:"report"`
		Error   string        `json:"error"`
	}
	if err := call(ctx, addr, http.MethodPost, path, cfg, &st); err != nil {
		return bench.Report{}, err
	}
	fmt.Printf("\n  %s %s on %s\n", bold("Benchmarking"), or(cfg.Model, "the loaded model"), or(node, "this node"))
	fmt.Println(dim("  The engine is reloaded for each phase and put back afterwards."))
	last := ""
	var lost time.Time // when polling first failed; zero while it works
	for {
		select {
		case <-ctx.Done():
			sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			_ = call(sctx, addr, http.MethodDelete, path, nil, nil)
			cancel()
			fmt.Println(dim("\n  stopped; the node is putting its engine back"))
			return bench.Report{}, ctx.Err()
		case <-time.After(1500 * time.Millisecond):
		}
		if err := call(ctx, addr, http.MethodGet, path, nil, &st); err != nil {
			if ctx.Err() != nil {
				continue
			}
			// Retry failed polls: the node owns the benchmark and continues through CLI or peer disconnects.
			if lost.IsZero() {
				lost = time.Now()
				fmt.Printf("  %s\n", dim("lost contact ("+err.Error()+"); the run continues on the node, retrying"))
			}
			if time.Since(lost) > time.Minute {
				return bench.Report{}, fmt.Errorf("lost contact for a minute: %w; the run may still finish there — `mfsh bench` can't reattach yet, but GET /api/v1/nodes/<node>/bench has the report", err)
			}
			continue
		}
		lost = time.Time{}
		if st.Report != nil && st.Report.Phase != "" && st.Report.Phase != last {
			last = st.Report.Phase
			fmt.Printf("  %s %s\n", dim(fmt.Sprintf("%5.0fs", st.Report.Took)), last)
		}
		if !st.Running {
			if st.Error != "" {
				return bench.Report{}, fmt.Errorf("%s", st.Error)
			}
			if st.Report == nil {
				return bench.Report{}, fmt.Errorf("the node finished without a report")
			}
			return *st.Report, nil
		}
	}
}

// benchCluster starts the cluster run on this node and follows it. The run is
// the node's; interrupted, this stops it.
func benchCluster(addr string, cfg bench.ClusterConfig, out string) error {
	if err := ensureNode(addr); err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if cfg.Model == "" {
		if cfg.Model = modelHere(ctx, addr); cfg.Model == "" {
			return fmt.Errorf("name the model: `mfsh bench -cluster -model <model>`")
		}
	}
	const path = "/api/v1/bench/cluster"
	var st struct {
		Running bool                 `json:"running"`
		Report  *bench.ClusterReport `json:"report"`
		Error   string               `json:"error"`
	}
	if err := call(ctx, addr, http.MethodPost, path, cfg, &st); err != nil {
		return err
	}
	if r := st.Report; r != nil {
		fmt.Printf("\n  %s %s through %s's front door, routed by %s\n", bold("Benchmarking the cluster:"), cfg.Model, r.Entry, r.Routing)
		for _, h := range r.Holders {
			fmt.Printf("  %s\n", dim("held by "+h.Node))
		}
	}
	last := ""
	for {
		select {
		case <-ctx.Done():
			sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			_ = call(sctx, addr, http.MethodDelete, path, nil, nil)
			cancel()
			fmt.Println(dim("\n  stopped"))
			return ctx.Err()
		case <-time.After(1500 * time.Millisecond):
		}
		if err := call(ctx, addr, http.MethodGet, path, nil, &st); err != nil {
			if ctx.Err() != nil {
				continue
			}
			return fmt.Errorf("lost contact with this node: %w; the run goes on there, and GET %s has the report", err, path)
		}
		if st.Report != nil && st.Report.Phase != "" && st.Report.Phase != last {
			last = st.Report.Phase
			fmt.Printf("  %s %s\n", dim(fmt.Sprintf("%5.0fs", st.Report.Took)), last)
		}
		if st.Running {
			continue
		}
		if st.Report != nil {
			fmt.Println()
			fmt.Print(st.Report.Text())
			name := fmt.Sprintf("%s-cluster-%s", st.Report.At.Format("20060102-150405"), st.Report.Model)
			if p, err := saveJSON(st.Report, name, out); err == nil {
				fmt.Printf("\n%s %s\n", dim("saved"), p)
			} else {
				fmt.Fprintln(os.Stderr, "could not save the report:", err)
			}
		}
		if st.Error != "" {
			return fmt.Errorf("%s", st.Error)
		}
		return nil
	}
}

func saveBench(rep bench.Report, dir string) (string, error) {
	return saveJSON(rep, fmt.Sprintf("%s-%s-%s", rep.At.Format("20060102-150405"), rep.Node, rep.Model), dir)
}

func saveJSON(v any, name, dir string) (string, error) {
	if dir == "" {
		dir = benchDir()
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	safe := regexp.MustCompile(`[^A-Za-z0-9._-]+`).ReplaceAllString(name, "_")
	path := filepath.Join(dir, safe+".json")
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return "", err
	}
	return path, os.WriteFile(path, append(b, '\n'), 0o644)
}

func intList(s, flagName string) ([]int, error) {
	if s == "" {
		return nil, nil
	}
	var out []int
	for _, f := range strings.Split(s, ",") {
		n, err := strconv.Atoi(strings.TrimSpace(f))
		if err != nil || n < 1 {
			return nil, fmt.Errorf("%s wants a comma-separated list of positive numbers, got %q", flagName, s)
		}
		out = append(out, n)
	}
	return out, nil
}

func or(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
