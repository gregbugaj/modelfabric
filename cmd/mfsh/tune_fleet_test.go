package main

import (
	"io"
	"os"
	"strings"
	"testing"

	"github.com/gregbugaj/modelfabric/internal/tuner"
)

func capture(t *testing.T, f func()) string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	done := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()
	f()
	w.Close()
	os.Stdout = old
	return <-done
}

// Nodes may stop at different slot counts and recommend different values.
func measuredFleet() []fleetResult {
	row := func(slots int, agg float64) tuner.Row {
		return tuner.Row{Slots: slots, Fit: true, Aggregate: agg, AskedContext: 65536, GotContext: 65536}
	}
	oom := func(slots int) tuner.Row {
		return tuner.Row{Slots: slots, Error: "the GPU ran out of memory while allocating the KV cache"}
	}
	return []fleetResult{
		{Node: "xpredator", Report: &tuner.Report{Node: "xpredator", Model: "qwen/qwen3.8-27b", Context: 65536,
			Rows:        []tuner.Row{row(1, 60), row(2, 77), oom(4)},
			Recommended: 2, Why: "throughput was still climbing at 2 slots, and the sweep stopped before it flattened",
			StoppedBecause: "4 slots would not load, so larger counts will not either", RestoredTo: 2}},
		{Node: "minion", Report: &tuner.Report{Node: "minion", Model: "qwen/qwen3.8-27b", Context: 65536,
			Rows:        []tuner.Row{row(1, 32), row(2, 42), row(4, 53), oom(8)},
			Recommended: 4, Why: "throughput was still climbing at 4 slots, and the sweep stopped before it flattened",
			RestoredTo: 2}},
		{Node: "helion", Report: &tuner.Report{Node: "helion", Model: "qwen/qwen3.8-27b", Context: 65536,
			Rows:        []tuner.Row{row(1, 10), row(2, 9)},
			Recommended: 1, Why: "1 slot gave the most throughput; past that the same tokens are spread over more requests",
			StoppedBecause: "2 slots did not beat 1 by more than a tenth, so doubling again would only add latency",
			RestoredTo:     2}},
	}
}

func TestFleetTableShowsEveryNodeAndItsAnswer(t *testing.T) {
	out := capture(t, func() { printFleetTable(measuredFleet()) })

	for _, want := range []string{"xpredator", "minion", "helion", "8 slots", "OOM"} {
		if !strings.Contains(out, want) {
			t.Errorf("the table is missing %q:\n%s", want, out)
		}
	}
	helion := line(t, out, "helion")
	if strings.Contains(helion, "53") || strings.Contains(helion, "OOM") {
		t.Errorf("helion's row carries another node's measurements: %q", helion)
	}
	if strings.Count(helion, "—") != 2 {
		t.Errorf("helion tried 1 and 2 of four columns, so two should be blank: %q", helion)
	}
	for node, want := range map[string]string{"xpredator": "2 slots", "minion": "4 slots", "helion": "1 slot"} {
		if got := line(t, out, node); !strings.HasSuffix(strings.TrimSpace(got), want) {
			t.Errorf("%s should recommend %s: %q", node, want, got)
		}
	}
}

func TestFleetTableAppliesNothingAndPrintsTheCommands(t *testing.T) {
	out := capture(t, func() { printFleetTable(measuredFleet()) })
	if !strings.Contains(out, "Nothing was changed") {
		t.Errorf("the table does not say the fleet was left alone:\n%s", out)
	}
	for _, want := range []string{
		"mfsh load qwen/qwen3.8-27b -context 65536 -parallel 2",
		"mfsh load qwen/qwen3.8-27b -context 65536 -parallel 4",
		"mfsh load qwen/qwen3.8-27b -context 65536 -parallel 1",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing the command to apply a recommendation: %q\n%s", want, out)
		}
	}
}

func TestFleetTableNamesNodesItCouldNotRestore(t *testing.T) {
	res := measuredFleet()
	res[1].Report.RestoredTo = 0
	out := capture(t, func() { printFleetTable(res) })
	if !strings.Contains(out, "could not put them back: minion") {
		t.Errorf("a node left on the swept slot count is not named:\n%s", out)
	}
}

// Two nodes swept at different contexts do not compare, and a table that
// printed one context in the header would read as though they did.
func TestFleetTableRefusesToPretendDifferentContextsCompare(t *testing.T) {
	res := measuredFleet()
	res[2].Report.Context = 32768
	out := capture(t, func() { printFleetTable(res) })
	if !strings.Contains(out, "different contexts") {
		t.Errorf("mixed contexts are presented as comparable:\n%s", out)
	}
}

// A node that never answered still gets a row. Dropping it would make a failed
// sweep look like a fleet that is one node smaller.
func TestFleetTableKeepsANodeThatFailed(t *testing.T) {
	res := append(measuredFleet(), fleetResult{Node: "sites-01", Err: io.EOF})
	out := capture(t, func() { printFleetTable(res) })
	row := line(t, out, "sites-01")
	if !strings.Contains(row, "did not run") {
		t.Errorf("a node that failed should say so: %q", row)
	}
}

func line(t *testing.T, out, prefix string) string {
	t.Helper()
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), prefix) {
			return l
		}
	}
	t.Fatalf("no line for %q in:\n%s", prefix, out)
	return ""
}
