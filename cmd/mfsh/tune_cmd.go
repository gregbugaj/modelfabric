package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/gregbugaj/modelfabric/internal/tuner"
)

// `mfsh tune`: how many slots this machine should actually run.
//
// The sweep itself is internal/tuner, shared with the dashboard. What lives
// here is the command: parsing, an Engine that drives a node over its own
// management API, and printing.

func tuneCmd(args []string) error {
	fs := flag.NewFlagSet("tune", flag.ExitOnError)
	addr := fs.String("addr", defaultAddr, "address of the ModelFabric node")
	// Both default to nothing rather than to a number. A tuning tool that asks
	// which slot counts to try is asking for part of the answer it was run to
	// find, and a default context of "32768 because that is a round number"
	// tunes a configuration nobody is running.
	slotList := fs.String("slots", "", "slot counts to try (default: 1, doubling until the engine will not load)")
	ctxLen := fs.Int("context", 0, "per-request context (default: what the model is loaded with)")
	promptTok := fs.Int("prompt", 3000, "approximate prompt tokens per request")
	outTok := fs.Int("output", 300, "tokens to generate per request")
	// The comparison is the answer. One node's number says what that machine
	// does; the table says whether the fleet's uniform slot count is costing
	// anything, which is the question that gets asked.
	fleet := fs.Bool("fleet", false, "tune every node serving the model and print them side by side")
	yes := fs.Bool("y", false, "skip the confirmation before a fleet sweep")
	if err := fs.Parse(args); err != nil {
		return err
	}
	// Loading is not in the peer allowlist, and tuning is a sequence of loads:
	// it has to run on the machine being tuned. The dashboard reaches other
	// nodes through the owner-only node proxy, which is a different door.
	if err := mustBeLocalBecause(*addr, "tuning",
		"reloads that node's engine, which is not something another node may do", "tune"); err != nil {
		return err
	}
	if err := ensureNode(*addr); err != nil {
		return err
	}

	var slots []int
	if *slotList != "" {
		for _, s := range strings.Split(*slotList, ",") {
			n, err := strconv.Atoi(strings.TrimSpace(s))
			if err != nil || n < 1 {
				return fmt.Errorf("-slots wants a comma-separated list of positive numbers, got %q", *slotList)
			}
			slots = append(slots, n)
		}
		sort.Ints(slots)
	}

	model := ""
	if rest := fs.Args(); len(rest) > 0 {
		model = rest[0]
	}
	if model == "" {
		if model = modelHere(context.Background(), *addr); model == "" {
			return fmt.Errorf("no model is loaded here and none was named: `mfsh tune <model>`")
		}
	}
	// What the machine is running now is the best statement of intent there
	// is: it is what the operator chose, and it is what the benchmark or the
	// application is using.
	here := httpEngine{addr: *addr}.instanceOf(context.Background(), model)
	ctxNow := *ctxLen
	if ctxNow == 0 {
		ctxNow = here.Context
	}
	cfg := tuner.Config{Model: model, Context: ctxNow, Prompt: *promptTok, Output: *outTok, Slots: slots}
	cfg.Fill()
	if here.Context == 0 && *ctxLen == 0 {
		fmt.Fprintln(os.Stderr, dim(fmt.Sprintf(
			"  nothing loaded to read a context from; using %d — pass -context to choose", cfg.Context)))
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if *fleet {
		// Each node runs the sweep itself, through its own /api/v1/tune. The
		// alternative — this process driving three engines over the network —
		// would make every measurement include a tailnet round trip and leave a
		// half-swept node behind whenever the command was interrupted.
		nodes, err := fleetNodes(ctx, *addr, cfg.Model)
		if err != nil {
			return err
		}
		if !confirmFleetTune(nodes, *yes) {
			return nil
		}
		return tuneFleet(ctx, *addr, tuneRequestBody{
			Model: cfg.Model, ContextLength: *ctxLen,
			Prompt: *promptTok, Output: *outTok, Slots: slots,
		}, nodes)
	}

	fmt.Printf("\n  %s %s\n", bold("Tuning"), cfg.Model)
	sweep := "1, doubling until the engine will not load"
	if len(slots) > 0 {
		sweep = fmt.Sprintf("%v", cfg.Slots)
	}
	fmt.Printf("  %s\n\n", dim(fmt.Sprintf(
		"%d-token context per request, ~%d-token prompts, %d tokens out, slots %s",
		cfg.Context, cfg.Prompt, cfg.Output, sweep)))
	fmt.Println(dim("  Each row reloads the engine and pushes that many requests at once."))
	fmt.Println(dim("  Nothing else should be using this node while it runs."))
	fmt.Println()

	// Printed as each row lands: a sweep runs for minutes, and a silent
	// terminal is indistinguishable from a stuck one.
	rep, err := tuner.Run(ctx, httpEngine{addr: *addr}, cfg, printTuneRow)
	if err != nil {
		return err
	}
	printTuneSummary(rep, cfg)
	return nil
}

// httpEngine drives a node over its own management API. The server drives its
// supervisor directly; both run the same sweep.
type httpEngine struct{ addr string }

func (h httpEngine) Reload(ctx context.Context, model string, contextLen, slots int) error {
	// The instance, by id. Unload takes "instance_id"; an earlier version sent
	// {"all": true}, which the API ignored — so the model stayed up, every
	// load returned in 0s as already-loaded, and the sweep measured one
	// configuration three times while reporting three. The ops journal gave it
	// away: real loads take about three seconds.
	before := h.instanceOf(ctx, model)
	if before.found() {
		if err := call(ctx, h.addr, http.MethodPost, "/api/v1/models/unload",
			map[string]any{"instance_id": before.ID}, nil); err != nil {
			return fmt.Errorf("could not unload %s: %w", before.ID, err)
		}
	}
	// Settings go inline, not nested: supervisor.LoadRequest embeds
	// runtime.Settings, so a {"settings": {...}} object is silently ignored and
	// the load falls back to the model's saved defaults. That is what happened
	// here — every row loaded at the saved vision defaults (32768 x 4) while
	// reporting the context and slots asked for.
	//
	// vision and spec_mode are stated rather than left out for the same
	// reason: a saved default for either would otherwise decide them, and a
	// sweep that silently changes two things at once measures neither.
	//
	// They are carried over from what was running rather than asserted. These
	// used to read `true` and `"mtp"` — this fleet's settings — which meant
	// tuning a node serving the model text-only would quietly load the
	// projector and measure a configuration the operator does not run.
	body := map[string]any{
		"model":          model,
		"context_length": contextLen,
		"parallel":       slots,
	}
	if before.found() {
		body["vision"] = before.Vision
		body["spec_mode"] = before.SpecMode
		// Which weights and which engine. Without these a sweep on a machine
		// holding the same model as GGUF and as MLX would reload into whichever
		// runtime is the node's default, and report the numbers as the other
		// engine's.
		if before.Format != "" {
			body["format"] = before.Format
		}
		if before.Runtime != "" {
			body["runtime"] = before.Runtime
		}
	}
	if err := call(ctx, h.addr, http.MethodPost, "/api/v1/models/load", body, nil); err != nil {
		return err
	}
	// Then wait for it to be ready, and confirm it actually restarted. The
	// load API answers when the operation is accepted, not when the engine is
	// serving — checking immediately reported "the engine reports 0 slots" for
	// every row, which looks like a failed load rather than an early look.
	//
	// A measurement of an engine that was never reloaded is worse than no
	// measurement: it looks like data.
	deadline := time.Now().Add(90 * time.Second)
	for {
		after := h.instanceOf(ctx, model)
		restarted := after.found() && after.ID != before.ID
		switch {
		case restarted && after.Slots == slots:
			return nil
		case restarted && after.Slots != 0 && after.Slots != slots:
			return fmt.Errorf("asked for %d slot(s) and the engine reports %d", slots, after.Slots)
		}
		if time.Now().After(deadline) {
			if after.found() && after.ID == before.ID {
				return fmt.Errorf("the engine did not restart (still %s); this row would measure the previous settings", after.ID)
			}
			return fmt.Errorf("the engine did not come up with %d slot(s) within 90s", slots)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

// engineState is a struct rather than five unnamed returns because it was
// five unnamed returns: `after, _, _, got, _ := ...` bound the slot count to
// inflight, which is always zero on an idle engine, so every row of every
// sweep waited out its full timeout while the mesh reported the engine ready
// the whole time.
type engineState struct {
	ID       string
	Addr     string
	Port     int
	Inflight int
	Slots    int
	Context  int
	// Vision and SpecMode are what this instance is running, so a sweep can
	// reproduce them instead of imposing its own.
	Vision   bool
	SpecMode string
	// Format and Runtime are which weights and which engine. A Mac holds the
	// same model as GGUF and as MLX and can serve either, so a sweep that did
	// not carry these would measure llama.cpp while reporting a sweep of MLX —
	// the load would fall back to the node's default runtime on the first row.
	Format  string
	Runtime string
	// Served is the id this engine answers to when it is not the catalog key.
	Served string
}

func (e engineState) found() bool { return e.ID != "" }

func (h httpEngine) instanceOf(ctx context.Context, model string) engineState {
	var mesh struct {
		Self struct {
			Instances []struct {
				ID       string `json:"id"`
				Model    string `json:"model"`
				Served   string `json:"served_model"`
				Engine   string `json:"engine"`
				Address  string `json:"address"`
				Port     int    `json:"port"`
				Inflight int    `json:"inflight"`
				Slots    int    `json:"slots"`
			} `json:"instances"`
		} `json:"self"`
	}
	if call(ctx, h.addr, http.MethodGet, "/z/mesh", nil, &mesh) != nil {
		return engineState{}
	}
	for _, i := range mesh.Self.Instances {
		if model != "" && i.Model != model {
			continue
		}
		a := i.Address
		if a == "" {
			a = "127.0.0.1"
		}
		e := engineState{ID: i.ID, Addr: a, Port: i.Port, Inflight: i.Inflight, Slots: i.Slots,
			Served: i.Served, Format: formatFor(i.Engine)}
		e.Context, e.Vision, e.SpecMode, e.Runtime = h.loadedConfig(ctx, i.ID)
		return e
	}
	return engineState{}
}

// loadedConfig is what an instance was actually loaded with: the per-request
// context, and the two settings a sweep has to hold still while it varies
// slots. The mesh publishes slots and rates but none of these, so they come
// from the node's own model list.
func (h httpEngine) loadedConfig(ctx context.Context, instanceID string) (contextLen int, vision bool, specMode, rt string) {
	var out struct {
		Models []struct {
			Loaded []struct {
				ID     string `json:"id"`
				Config struct {
					ContextLength int    `json:"context_length"`
					Vision        bool   `json:"vision"`
					Speculative   bool   `json:"speculative"`
					SpecType      string `json:"spec_type"`
					Runtime       string `json:"runtime"`
				} `json:"config"`
			} `json:"loaded_instances"`
		} `json:"models"`
	}
	if call(ctx, h.addr, http.MethodGet, "/api/v1/models", nil, &out) != nil {
		return 0, false, "", ""
	}
	for _, m := range out.Models {
		for _, i := range m.Loaded {
			if i.ID != instanceID {
				continue
			}
			// The engine reports how it is drafting; a load takes the setting
			// that produces it.
			mode := "off"
			if i.Config.Speculative {
				mode = "mtp"
				if i.Config.SpecType == "draft-simple" {
					mode = "draft"
				}
			}
			// The entry that owns this instance is the variant it loaded, so
			// its format is the weights actually in memory.
			return i.Config.ContextLength, i.Config.Vision, mode, i.Config.Runtime
		}
	}
	return 0, false, "", ""
}

// formatFor is the weights an engine family serves.
//
// Taken from the family rather than from the model list, because an instance is
// listed under its model's primary entry whichever variant it loaded: reading
// the entry's format sent runtime=mlx with format=gguf, and the load refused it
// — correctly, and only because that refusal exists.
func formatFor(engine string) string {
	switch engine {
	case "mlx":
		return "mlx"
	case "llama.cpp":
		return "gguf"
	}
	return ""
}

func (h httpEngine) Target(ctx context.Context, model string) (string, string, error) {
	e := h.instanceOf(ctx, model)
	if !e.found() || e.Port == 0 {
		return "", "", fmt.Errorf("no ready engine for %s after loading", model)
	}
	served := model
	if e.Served != "" {
		served = e.Served
	}
	return fmt.Sprintf("http://%s:%d", e.Addr, e.Port), served, nil
}

func (h httpEngine) Inflight(ctx context.Context, model string) (int, error) {
	return h.instanceOf(ctx, model).Inflight, nil
}

func (h httpEngine) Current(ctx context.Context, model string) (int, int, error) {
	e := h.instanceOf(ctx, model)
	return e.Slots, e.Context, nil
}

// modelHere is what to tune when none was named.
func modelHere(ctx context.Context, addr string) string {
	var mesh struct {
		Self struct {
			Instances []struct {
				Model string `json:"model"`
			} `json:"instances"`
		} `json:"self"`
	}
	if call(ctx, addr, http.MethodGet, "/z/mesh", nil, &mesh) != nil {
		return ""
	}
	for _, i := range mesh.Self.Instances {
		return i.Model
	}
	return ""
}

func printTuneRow(r tuner.Row) { fmt.Println("  " + tuneRowLine(r)) }

// tuneRowLine is one measured row, slot count included, so a single-node sweep
// can print it as it stands and a fleet sweep can prefix the node's name.
func tuneRowLine(r tuner.Row) string {
	label := fmt.Sprintf("%-9s", plural(r.Slots, "slot"))
	if r.Starved {
		// Distinct from a load the engine refused: here ModelFabric refused, because
		// running the row would have taken the machine with it.
		return fmt.Sprintf("%s %s  %s", label, yellow("not attempted"), dim(r.Error))
	}
	if !r.Fit {
		return fmt.Sprintf("%s %s  %s", label, red("did not run"), dim(r.Error))
	}
	// Flagged rather than buried: a context that is not what was asked for has
	// already cost this project a benchmark.
	note := ""
	if r.GotContext > 0 && r.GotContext != r.AskedContext {
		note = "  " + yellow(fmt.Sprintf("engine reports n_ctx %d, asked for %d", r.GotContext, r.AskedContext))
	}
	// Decode is printed beside the aggregate because the two answer different
	// questions, and the aggregate alone reads as a slower machine than it is:
	// it carries the prefill, which on a Mac is most of the wall clock.
	return fmt.Sprintf("%s %s  %s  %s  %s%s", label,
		cyan(fmt.Sprintf("%6.0f tok/s together", r.Aggregate)),
		dim(fmt.Sprintf("%5.0f decode", r.DecodeTokS)),
		dim(fmt.Sprintf("first token %4.1fs", float64(r.TTFTP50Ms)/1000)),
		dim(fmt.Sprintf("%5.0f each end to end", r.PerRequest)), note)
}

func printTuneSummary(rep tuner.Report, cfg tuner.Config) {
	fmt.Println()
	if rep.Recommended == 0 {
		fmt.Println("  " + red(rep.Why))
		return
	}
	fmt.Printf("  %s %s\n", bold("Recommended:"),
		fmt.Sprintf("%s at %d-token context", plural(rep.Recommended, "slot"), cfg.Context))
	fmt.Printf("  %s\n", dim(rep.Why))
	if rep.StoppedBecause != "" {
		fmt.Printf("  %s\n", dim("stopped: "+rep.StoppedBecause))
	}
	for _, w := range rep.Warnings {
		fmt.Printf("  %s\n", yellow(w))
	}
	fmt.Printf("\n  %s\n", dim(fmt.Sprintf("apply with: mfsh load %s -context %d -parallel %d",
		cfg.Model, cfg.Context, rep.Recommended)))
	fmt.Printf("  %s\n", dim("Run this on each node; the answer is the machine's, not the fleet's."))
	// Said every time, because the decode column invites exactly the wrong
	// conclusion about drafting. The sweep's prompts are pseudorandom words to
	// be summarised, so the model's output is unpredictable and a drafter
	// guesses it badly: measured 2026-09-25 at one slot, MTP read as 8% slower
	// than no speculation on both a 5090 and a Mac, where the same 5090 on
	// predictable output decodes 134 tok/s against 66 with drafting off.
	fmt.Printf("  %s\n", dim("Slots and context only. Decode here understates speculative decoding, "+
		"which pays on predictable output and not on these prompts."))
	if rep.RestoredTo > 0 {
		fmt.Printf("  %s\n", dim(plural(rep.RestoredTo, "slot")+" restored, as found"))
	}
}
