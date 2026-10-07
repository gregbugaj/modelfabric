package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gregbugaj/modelfabric/internal/tuner"
)

// `mfsh tune -fleet` compares slot counts across nodes serving the model.
// Each node runs POST /api/v1/tune through the node proxy. Recommendations
// are printed as commands and are not applied automatically.

// tuneFleetPoll sets the progress polling interval; each reload-and-generation row takes tens of seconds.
const tuneFleetPoll = 3 * time.Second

type fleetResult struct {
	Node   string
	Report *tuner.Report
	Err    error
}

func tuneFleet(ctx context.Context, addr string, req tuneRequestBody, nodes []string) error {
	fmt.Printf("\n  %s %s\n", bold("Tuning the fleet:"), strings.Join(nodes, ", "))
	fmt.Printf("  %s\n", dim(fmt.Sprintf(
		"%s, ~%d-token prompts, %d tokens out", contextPhrase(req.ContextLength), tunePrompt(req.Prompt), tuneOutput(req.Output))))
	fmt.Println()
	fmt.Println(dim("  Every node reloads its engine once per slot count, so each is"))
	fmt.Println(dim("  unavailable while its own sweep runs. Nothing is applied: each node is"))
	fmt.Println(dim("  put back on the count it was found with, and the table says what it"))
	fmt.Println(dim("  would recommend."))
	fmt.Println()

	// Sweep independent nodes concurrently; each node rejects overlapping local sweeps.
	var (
		mu      sync.Mutex // one writer to the terminal
		wg      sync.WaitGroup
		results = make([]fleetResult, len(nodes))
	)
	for i, node := range nodes {
		wg.Add(1)
		go func(i int, node string) {
			defer wg.Done()
			rep, err := tuneOneNode(ctx, addr, node, req, func(row tuner.Row) {
				mu.Lock()
				defer mu.Unlock()
				fmt.Printf("  %-12s %s\n", cyan(node), tuneRowLine(row))
			})
			results[i] = fleetResult{Node: node, Report: rep, Err: err}
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				fmt.Printf("  %-12s %s\n", cyan(node), red(err.Error()))
			}
		}(i, node)
	}
	wg.Wait()

	printFleetTable(results)
	return nil
}

// tuneOneNode starts and follows a node-owned sweep, which survives CLI disconnects and failed polls.
func tuneOneNode(ctx context.Context, addr, node string, req tuneRequestBody, onRow func(tuner.Row)) (*tuner.Report, error) {
	base := "/api/v1/nodes/" + node + "/tune"
	var started tuneStatusBody
	if err := call(ctx, addr, http.MethodPost, base, req, &started); err != nil {
		return nil, fmt.Errorf("could not start a sweep on %s: %w", node, err)
	}

	seen := 0
	for {
		select {
		case <-ctx.Done():
			// Interrupted: stop the sweep rather than leaving a node mid-row.
			// tuner.Run restores what it found, so this puts the node back.
			stop, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			_ = call(stop, addr, http.MethodDelete, base, nil, nil)
			cancel()
			return nil, ctx.Err()
		case <-time.After(tuneFleetPoll):
		}
		var st tuneStatusBody
		if err := call(ctx, addr, http.MethodGet, base, nil, &st); err != nil {
			// A poll that fails is not a sweep that failed. Keep following.
			continue
		}
		for _, row := range st.Rows[min(seen, len(st.Rows)):] {
			if onRow != nil {
				onRow(row)
			}
		}
		seen = len(st.Rows)
		if st.Running {
			continue
		}
		if st.Error != "" {
			return st.Report, fmt.Errorf("%s", st.Error)
		}
		if st.Report == nil {
			return nil, fmt.Errorf("%s stopped without a report", node)
		}
		return st.Report, nil
	}
}

func printFleetTable(results []fleetResult) {
	// Include every tried slot count; nodes reach memory limits at different counts.
	var counts []int
	seen := map[int]bool{}
	model, contextLen := "", 0
	// Track each context length because throughput at different contexts is not comparable.
	contexts := map[int][]string{}
	for _, r := range results {
		if r.Report == nil {
			continue
		}
		model, contextLen = r.Report.Model, r.Report.Context
		contexts[r.Report.Context] = append(contexts[r.Report.Context], r.Node)
		for _, row := range r.Report.Rows {
			if !seen[row.Slots] {
				seen[row.Slots] = true
				counts = append(counts, row.Slots)
			}
		}
	}
	sort.Ints(counts)
	if len(counts) == 0 {
		fmt.Println()
		fmt.Println("  " + red("no node completed a sweep; nothing to recommend"))
		return
	}

	fmt.Println()
	fmt.Printf("  %s %s\n", bold(model), dim("· "+contextPhrase(contextLen)))
	if len(contexts) > 1 {
		var parts []string
		swept := make([]int, 0, len(contexts))
		for c := range contexts {
			swept = append(swept, c)
		}
		sort.Ints(swept)
		for _, c := range swept {
			parts = append(parts, fmt.Sprintf("%d on %s", c, strings.Join(contexts[c], ", ")))
		}
		fmt.Printf("  %s\n", yellow("the nodes were swept at different contexts ("+strings.Join(parts, "; ")+
			"), so these columns do not compare — re-run with -context to fix one"))
	}
	fmt.Println()

	header := fmt.Sprintf("  %-12s", "node")
	for _, c := range counts {
		header += fmt.Sprintf("%8s", plural(c, "slot"))
	}
	header += "   " + "recommended"
	fmt.Println(dim(header))

	for _, r := range results {
		line := fmt.Sprintf("  %-12s", r.Node)
		byCount := map[int]tuner.Row{}
		if r.Report != nil {
			for _, row := range r.Report.Rows {
				byCount[row.Slots] = row
			}
		}
		for _, c := range counts {
			line += fmt.Sprintf("%8s", cell(byCount, c))
		}
		switch {
		case r.Report == nil:
			line += "   " + red("did not run")
		case r.Report.Recommended == 0:
			line += "   " + red("no answer")
		default:
			line += "   " + bold(plural(r.Report.Recommended, "slot"))
		}
		fmt.Println(line)
	}
	fmt.Printf("  %s\n", dim(strings.Repeat(" ", 12)+"total tok/s with that many requests in flight"))

	fmt.Println()
	for _, r := range results {
		if r.Report == nil {
			continue
		}
		fmt.Printf("  %s %s\n", cyan(r.Node+":"), dim(r.Report.Why))
		if r.Report.StoppedBecause != "" {
			fmt.Printf("  %s %s\n", strings.Repeat(" ", len(r.Node)+1), dim("stopped: "+r.Report.StoppedBecause))
		}
		for _, w := range r.Report.Warnings {
			fmt.Printf("  %s %s\n", strings.Repeat(" ", len(r.Node)+1), yellow(w))
		}
	}

	var restored, notRestored []string
	for _, r := range results {
		if r.Report == nil {
			continue
		}
		if r.Report.RestoredTo > 0 {
			restored = append(restored, fmt.Sprintf("%s (%d)", r.Node, r.Report.RestoredTo))
		} else {
			notRestored = append(notRestored, r.Node)
		}
	}
	fmt.Println()
	if len(restored) > 0 {
		fmt.Printf("  %s\n", dim("Nothing was changed — back as found: "+strings.Join(restored, ", ")))
	}
	if len(notRestored) > 0 {
		fmt.Printf("  %s\n", yellow("Check these nodes; the sweep could not put them back: "+
			strings.Join(notRestored, ", ")))
	}

	// Print apply commands without running them: reloading drops engine-resident conversations.
	fmt.Println()
	fmt.Println(bold("  To apply:"))
	for _, r := range results {
		if r.Report == nil || r.Report.Recommended == 0 {
			continue
		}
		cmd := fmt.Sprintf("mfsh load %s -context %d -parallel %d", model, contextLen, r.Report.Recommended)
		fmt.Printf("    %-12s %s\n", r.Node, cmd)
	}
	fmt.Printf("  %s\n", dim("Each runs on its own node — loading is not something one node does to another."))
}

// cell is one measurement, or why there is none. A blank column and a column
// that failed mean different things: "not tried" versus "this machine cannot".
func cell(rows map[int]tuner.Row, slots int) string {
	row, ok := rows[slots]
	switch {
	case !ok:
		return "—"
	case row.Fit:
		return fmt.Sprintf("%.0f", row.Aggregate)
	case row.Starved:
		// The machine had no room to try, which is not the same as the engine
		// refusing to load.
		return "no room"
	case strings.Contains(strings.ToLower(row.Error), "out of memory"),
		strings.Contains(strings.ToLower(row.Error), "oom"):
		return "OOM"
	default:
		return "failed"
	}
}

func contextPhrase(n int) string {
	if n <= 0 {
		return "the loaded context"
	}
	return fmt.Sprintf("%d-token context", n)
}

func tunePrompt(n int) int {
	if n <= 0 {
		return 3000
	}
	return n
}

func tuneOutput(n int) int {
	if n <= 0 {
		return 300
	}
	return n
}

// tuneRequestBody and tuneStatusBody mirror internal/server's tune API. They
// are restated rather than imported because cmd/mfsh talks to a node over HTTP,
// including nodes running a different build of ModelFabric than this binary.
type tuneRequestBody struct {
	Model         string `json:"model,omitempty"`
	ContextLength int    `json:"context_length,omitempty"`
	Prompt        int    `json:"prompt,omitempty"`
	Output        int    `json:"output,omitempty"`
	Slots         []int  `json:"slots,omitempty"`
}

type tuneStatusBody struct {
	Node    string        `json:"node"`
	Running bool          `json:"running"`
	Rows    []tuner.Row   `json:"rows"`
	Report  *tuner.Report `json:"report"`
	Error   string        `json:"error"`
}

func fleetNodes(ctx context.Context, addr, model string) ([]string, error) {
	var mesh struct {
		Self  meshNodeView   `json:"self"`
		Peers []meshNodeView `json:"peers"`
	}
	if err := call(ctx, addr, http.MethodGet, "/z/mesh", nil, &mesh); err != nil {
		return nil, err
	}
	var nodes []string
	for _, n := range append([]meshNodeView{mesh.Self}, mesh.Peers...) {
		if n.Node == "" {
			continue
		}
		for _, i := range n.Instances {
			if model == "" || i.Model == model {
				nodes = append(nodes, n.Node)
				break
			}
		}
	}
	if len(nodes) == 0 {
		return nil, fmt.Errorf("no node in the mesh is serving %s", model)
	}
	return nodes, nil
}

func confirmFleetTune(nodes []string, yes bool) bool {
	if yes {
		return true
	}
	fmt.Printf("\n  This reloads the engine on %s several times each.\n", plural(len(nodes), "node"))
	fmt.Printf("  %s\n", dim("Requests routed to a node mid-sweep will fail: it is restarting."))
	fmt.Printf("\n  Tune %s? %s ", strings.Join(nodes, ", "), dim("[y/N]"))
	var answer string
	_, _ = fmt.Fscanln(os.Stdin, &answer)
	ok := strings.EqualFold(strings.TrimSpace(answer), "y") || strings.EqualFold(strings.TrimSpace(answer), "yes")
	if !ok {
		fmt.Println("  nothing was changed")
	}
	fmt.Println()
	return ok
}
