package mesh

import (
	"testing"
	"time"
)

// The dashboard showed 0 in flight on an engine whose KV cache was visibly
// climbing, through a whole 8000-word request. ModelFabric's counter only sees what
// its own router dispatched, and under llm-d nothing does: Envoy dials the
// engine directly. So the engine's own count wins where there is one.
func TestEngineReportedInflightWins(t *testing.T) {
	e := NewEngine("i1", "http://127.0.0.1:1")
	if got := e.Inflight(); got != 0 {
		t.Fatalf("a fresh engine is idle, got %d", got)
	}
	e.SetObservedInflight(2)
	if got := e.Inflight(); got != 2 {
		t.Errorf("the engine said 2, got %d", got)
	}
	// A measured zero is a real answer, not "never measured": an engine that
	// has gone idle must read 0 rather than falling back to a router counter
	// that llm-d never incremented.
	e.SetObservedInflight(0)
	if got := e.Inflight(); got != 0 {
		t.Errorf("the engine said 0, got %d", got)
	}
}

// An engine ModelFabric cannot ask keeps the behaviour it always had: mlx-lm serves
// no /slots, and its requests all come through ModelFabric's router anyway.
func TestRouterCountStandsWithoutTheEngine(t *testing.T) {
	e := NewEngine("i1", "http://127.0.0.1:1")
	e.inflight.Store(3)
	if got := e.Inflight(); got != 3 {
		t.Errorf("unpolled, the router's own count stands: got %d", got)
	}
	e.SetObservedInflight(1)
	if got := e.Inflight(); got != 1 {
		t.Errorf("once the engine answers it wins: got %d", got)
	}
}

// The node's own engines report through EngineState, peers through
// InstanceState. Both must ask the same way, or the Serving page shows a
// peer's true load beside a flat 0 for the machine you are sitting at — which
// is exactly what it did.
func TestSnapshotUsesTheEngineCount(t *testing.T) {
	e := NewEngine("i1", "http://127.0.0.1:1")
	e.MarkReady("m")
	e.SetObservedInflight(2)
	if got := e.snapshot().Inflight; got != 2 {
		t.Errorf("EngineState should carry the engine's own count, got %d", got)
	}
	// And without one, the router's count still shows.
	e2 := NewEngine("i2", "http://127.0.0.1:2")
	e2.MarkReady("m")
	e2.inflight.Store(4)
	if got := e2.snapshot().Inflight; got != 4 {
		t.Errorf("unpolled, the router's count stands: got %d", got)
	}
}

// The instant says what an engine is doing; the average says how work was
// shared. On a mixed fleet only the second answers the question worth asking:
// an even share of requests to an engine a ninth as fast is not an even share
// of work. The dashboard could only average while the page was open, so the
// node keeps it too.
func TestLoadAverage(t *testing.T) {
	e := NewEngine("i1", "http://127.0.0.1:1")
	if _, ok := e.LoadAvg(); ok {
		t.Error("an engine with no samples has no average")
	}
	// Two readings of a two-minute window is not an average.
	e.SetObservedInflight(2)
	e.SetObservedInflight(2)
	if _, ok := e.LoadAvg(); ok {
		t.Error("two samples is still not an average")
	}
	e.SetObservedInflight(2)
	v, ok := e.LoadAvg()
	if !ok || v != 2 {
		t.Errorf("three samples of 2 average 2: got %v, ok=%v", v, ok)
	}
	e.SetObservedInflight(0)
	if v, _ := e.LoadAvg(); v != 1.5 {
		t.Errorf("2,2,2,0 averages 1.5: got %v", v)
	}
	// And it is published, so a peer reads it without asking twice.
	if got := e.snapshot().LoadAvg; got != 1.5 {
		t.Errorf("EngineState should carry it: got %v", got)
	}
}

// Samples older than the window are dropped, or a burst an hour ago still
// counts against an engine that has been idle since.
func TestLoadAverageForgetsOldSamples(t *testing.T) {
	e := NewEngine("i1", "http://127.0.0.1:1")
	old := time.Now().Add(-2 * LoadWindow)
	e.load = []loadSample{{at: old, n: 99}, {at: old, n: 99}}
	e.SetObservedInflight(1)
	e.SetObservedInflight(1)
	e.SetObservedInflight(1)
	v, ok := e.LoadAvg()
	if !ok || v != 1 {
		t.Errorf("the old burst should be gone: got %v, ok=%v", v, ok)
	}
}

// One rate was doing two jobs. Routing needs a settled figure — llm-d
// schedules by it — but waiting for 20,000 prompt tokens meant a visibly busy
// engine showed a dash for minutes. So it is reported early and marked, and
// only trusted late.
func TestPrefillRateIsReportedEarlyAndTrustedLate(t *testing.T) {
	e := NewEngine("i1", "http://127.0.0.1:1")
	e.SetRates(EngineRates{PrefillTokS: 262, Trusted: false})
	if got := e.PrefillRate(); got != 262 {
		t.Errorf("a rough rate is still shown: got %v", got)
	}
	if got := e.TrustedPrefillRate(); got != 0 {
		t.Errorf("but nothing routes by it: got %v", got)
	}
	if e.PrefillTrusted() {
		t.Error("and it says so")
	}
	e.SetRates(EngineRates{PrefillTokS: 262, Trusted: true})
	if got := e.TrustedPrefillRate(); got != 262 {
		t.Errorf("once settled, routing uses it: got %v", got)
	}
	if e.snapshot().PrefillTrusted != true {
		t.Error("and peers are told")
	}
}
