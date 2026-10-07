package mesh

import (
	"testing"
	"time"
)

// Router counters omit llm-d traffic because Envoy dials engines directly.
// Engine-observed counts must therefore contribute to in-flight reporting.
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
	// Busy slots omit queued requests. Preserve the local dispatch count
	// when it exceeds the engine's reported running count.
	e.SetObservedInflight(1)
	if got := e.Inflight(); got != 3 {
		t.Errorf("one running and two queued is three in flight: got %d", got)
	}
}

func TestSnapshotUsesTheEngineCount(t *testing.T) {
	e := NewEngine("i1", "http://127.0.0.1:1")
	e.MarkReady("m")
	e.SetObservedInflight(2)
	if got := e.snapshot().Inflight; got != 2 {
		t.Errorf("EngineState should carry the engine's own count, got %d", got)
	}
	e2 := NewEngine("i2", "http://127.0.0.1:2")
	e2.MarkReady("m")
	e2.inflight.Store(4)
	if got := e2.snapshot().Inflight; got != 4 {
		t.Errorf("unpolled, the router's count stands: got %d", got)
	}
}

func TestLoadAverage(t *testing.T) {
	e := NewEngine("i1", "http://127.0.0.1:1")
	if _, ok := e.LoadAvg(); ok {
		t.Error("an engine with no samples has no average")
	}
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

// Report preliminary rates before the 20,000-token threshold, but mark them
// untrusted until there is enough data for scheduling.
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

// Regression: finished peer requests must leave the load count immediately,
// or a follow-up sees its own previous request occupying the home slot.
func TestPeerLoadCountsARequestInAndOut(t *testing.T) {
	eng := func(inflight int64) []EngineState {
		return []EngineState{{Name: "gpu", Healthy: true, Models: []string{"qwen"}, EngineStats: EngineStats{Inflight: inflight}, Slots: 2}}
	}
	for _, tc := range []struct {
		name     string
		do       func(m *Mesh, poll func(int64))
		want     int64
		wantFull bool
	}{
		{"a finished request is gone before the next poll", func(m *Mesh, poll func(int64)) {
			poll(0)
			m.Candidates("qwen", false)[0].Acquire()()
		}, 0, false},
		{"a running request is counted before the peer reports it", func(m *Mesh, poll func(int64)) {
			poll(0)
			m.Candidates("qwen", false)[0].Acquire()
		}, 1, false},
		{"a running request the peer has reported is counted once", func(m *Mesh, poll func(int64)) {
			poll(0)
			m.Candidates("qwen", false)[0].Acquire()
			poll(1)
		}, 1, false},
		// The case from the run: the conversation's own request ended after the
		// peer last reported two in flight, and its neighbour is still running.
		{"home with a busy neighbour is not full when our own request ends", func(m *Mesh, poll func(int64)) {
			poll(0)
			c := m.Candidates("qwen", false)[0]
			done := c.Acquire()
			c.Acquire()
			poll(2)
			done()
		}, 1, false},
		{"requests others sent the peer are kept from its report", func(m *Mesh, poll func(int64)) {
			poll(0)
			done := m.Candidates("qwen", false)[0].Acquire()
			poll(2) // ours and one that is not
			done()
		}, 1, false},
		{"others' requests and ours together fill it", func(m *Mesh, poll func(int64)) {
			poll(1)
			m.Candidates("qwen", false)[0].Acquire()
		}, 2, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := testMesh(t)
			poll := func(n int64) {
				m.upsert("peer", "peer", "http://peer:1234", &NodeState{Node: "peer", Models: []string{"qwen"}, Inflight: n, Engines: eng(n)})
			}
			tc.do(m, poll)
			c := m.Candidates("qwen", false)[0]
			if c.Load() != tc.want || c.Inflight != tc.want {
				t.Errorf("load %d, in flight on its engines %d, want %d", c.Load(), c.Inflight, tc.want)
			}
			if c.Full() != tc.wantFull {
				t.Errorf("full = %v, want %v", c.Full(), tc.wantFull)
			}
		})
	}
}

// Regression: stale busy-slot polls must not retain completed local requests
// and delay their follow-up turns in the router queue.
func TestAFinishedRequestLeavesTheCountAtOnce(t *testing.T) {
	for _, tc := range []struct {
		name string
		do   func(e *Engine)
		want int64
	}{
		{"two running, both ours; one ends before the next poll", func(e *Engine) {
			e.inflight.Store(2)
			e.SetObservedInflight(2)
			e.inflight.Add(-1)
		}, 1},
		{"one of ours ends while the engine is being asked", func(e *Engine) {
			e.inflight.Store(2)
			before := e.inflight.Load()
			e.inflight.Add(-1) // ends between the count and the engine's answer
			e.setObserved(2, before)
		}, 1},
		{"one of ours starts while the engine is being asked", func(e *Engine) {
			e.inflight.Store(1)
			before := e.inflight.Load()
			e.inflight.Add(1)
			e.setObserved(2, before)
		}, 2},
		{"llm-d's requests are kept: nothing here dispatched them", func(e *Engine) {
			e.SetObservedInflight(2)
		}, 2},
		{"llm-d's two and one of ours", func(e *Engine) {
			e.SetObservedInflight(2)
			e.inflight.Add(1)
		}, 3},
		{"queued requests are ours and all counted", func(e *Engine) {
			e.inflight.Store(5) // one slot: one running, four waiting
			e.SetObservedInflight(1)
		}, 5},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := NewEngine("i1", "http://127.0.0.1:1")
			tc.do(e)
			if got := e.Inflight(); got != tc.want {
				t.Errorf("in flight %d, want %d", got, tc.want)
			}
		})
	}
}

// Engine reloads invalidate cached-prefix placement; stale entries previously
// routed repeated benchmark conversations using caches that no longer existed.
func TestAnEngineThatStopsIsReported(t *testing.T) {
	inst := func(ids ...string) []InstanceState {
		var out []InstanceState
		for _, id := range ids {
			out = append(out, InstanceState{ID: id, Model: "qwen"})
		}
		return out
	}
	for _, c := range []struct {
		name          string
		before, after []InstanceState
		want          []string
	}{
		{"a peer reporting the same instance again: nothing", inst("a"), inst("a"), nil},
		{"a peer whose instance was reloaded under a new id", inst("a"), inst("b"), []string{"peer"}},
		{"a peer that unloaded its model", inst("a"), nil, []string{"peer"}},
		{"a peer that loaded a second instance beside the first: nothing", inst("a"), inst("a", "b"), nil},
		{"a peer that dropped one of two", inst("a", "b"), inst("b"), []string{"peer"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			m := testMesh(t)
			var got []string
			m.SetEngineGoneHook(func(name string) { got = append(got, name) })
			for _, list := range [][]InstanceState{c.before, c.after} {
				m.upsert("peer", "peer", "http://peer:1234", &NodeState{Node: "peer", Models: []string{"qwen"}, Instances: list})
			}
			if len(got) != len(c.want) || (len(got) > 0 && got[0] != c.want[0]) {
				t.Errorf("reported %v, want %v", got, c.want)
			}
		})
	}
	t.Run("a local engine that is unregistered", func(t *testing.T) {
		m := testMesh(t)
		var got []string
		m.SetEngineGoneHook(func(name string) { got = append(got, name) })
		m.RegisterEngine(NewEngine("i1", "http://127.0.0.1:1"))
		m.UnregisterEngine("i1")
		m.UnregisterEngine("i1") // already gone: said once
		if len(got) != 1 || got[0] != "i1" {
			t.Errorf("reported %v, want [i1]", got)
		}
	})
}
