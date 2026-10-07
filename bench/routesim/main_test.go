package main

import (
	"container/heap"
	"testing"
)

func testEngine(slots, pool, ramTokens int) *engine {
	return &engine{name: "e", slots: make([]slot, slots), pool: pool, prefill: 1000, decode: 100,
		ramTokens: ramTokens, pshare: 1, dshare: 1, dstarve: 100}
}

func served(e *engine, conv string, prompt, out int, now float64) int {
	r := &request{conv: conv, prompt: prompt, out: out}
	e.serve(r, now)
	e.running = nil
	e.release(conv, now)
	return r.cachedGot
}

func TestEngineModel(t *testing.T) {
	t.Run("a returning conversation finds its slot", func(t *testing.T) {
		e := testEngine(2, 100000, 0)
		served(e, "a", 5000, 100, 0)
		if got := served(e, "a", 5200, 100, 1); got != 5100 {
			t.Errorf("cached %d, want the 5100 tokens of its last turn", got)
		}
	})
	t.Run("evicted with no RAM cache, it reads everything again", func(t *testing.T) {
		e := testEngine(1, 100000, 0)
		served(e, "a", 5000, 100, 0)
		served(e, "b", 4000, 100, 1) // takes a's only slot
		if got := served(e, "a", 5200, 100, 2); got != sharedPrefix {
			t.Errorf("cached %d, want only the shared prefix (%d)", got, sharedPrefix)
		}
	})
	t.Run("evicted into a RAM cache that holds it, it is restored", func(t *testing.T) {
		e := testEngine(1, 100000, 20000)
		served(e, "a", 5000, 100, 0)
		served(e, "b", 4000, 100, 1)
		if got := served(e, "a", 5200, 100, 2); got != 5100 {
			t.Errorf("cached %d, want 5100 restored from RAM", got)
		}
	})
	// minion on 2026-10-05: a 127k-token conversation against a 1981 MiB cache.
	t.Run("too large for the RAM cache, it is lost", func(t *testing.T) {
		e := testEngine(1, 400000, 24000)
		served(e, "a", 127000, 100, 0)
		served(e, "b", 4000, 100, 1)
		if got := served(e, "a", 127200, 100, 2); got != sharedPrefix {
			t.Errorf("cached %d, want only the shared prefix", got)
		}
	})
	t.Run("a pool two conversations overflow keeps only one", func(t *testing.T) {
		e := testEngine(2, 262144, 0)
		served(e, "a", 133000, 100, 0)
		served(e, "b", 117000, 100, 1) // 250k held; a's next turn does not fit beside it
		if got := served(e, "a", 134000, 16000, 2); got != 133100 {
			t.Fatalf("a still has its own slot: cached %d", got)
		}
		if got := served(e, "b", 118000, 100, 3); got != sharedPrefix {
			t.Errorf("b was evicted to make room for a: cached %d, want %d", got, sharedPrefix)
		}
	})
}

func testSim(poll float64, engines ...*engine) *sim {
	s := &sim{engines: engines, policy: "least-busy-first", poll: poll, home: map[string]string{}, lastSeen: map[string]float64{}}
	if poll > 0 {
		s.at(0, s.pollAll)
	}
	return s
}

func (s *sim) drain(until float64) {
	for s.q.Len() > 0 && s.q[0].at <= until {
		ev := heap.Pop(&s.q).(event)
		s.now = ev.at
		ev.fn(ev.at)
	}
	s.now = until
}

// Concurrent prefill reduces decode throughput to roughly a tenth of its
// standalone rate; fixed rates and equal sharing underpredicted runtime.
func TestRequestsOnOneEngineSlowEachOther(t *testing.T) {
	// 1000 tok/s reading, 100 writing; two readers share 1.4x one; a writer
	// beside a writer keeps 0.6; a writer beside a reader gets 10 tok/s.
	for _, c := range []struct {
		name string
		// each request: tokens to read (beyond the shared prefix), tokens to write
		reqs [][2]int
		want []float64 // when each finishes, seconds
	}{
		{"alone, it reads then writes at the engine's rates", [][2]int{{10000, 100}}, []float64{11}},
		{"two writers each keep 0.6 of the rate", [][2]int{{0, 600}, {0, 600}}, []float64{10, 10}},
		// A writes 100 tokens in the 10s B spends reading, where alone it would take 1s.
		{"a writer crawls until the reader beside it is done", [][2]int{{0, 100}, {10000, 100}}, []float64{10, 11}},
		// 0.7 of the rate each: 7000 tokens in 10s, then the writing as two writers.
		{"two readers share 1.4 times one", [][2]int{{7000, 60}, {7000, 60}}, []float64{11, 11}},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := &engine{name: "e", slots: make([]slot, 4), pool: 1 << 30, prefill: 1000, decode: 100, pshare: 1.4, dshare: 0.6, dstarve: 10}
			s := testSim(0, e)
			got := make([]float64, len(c.reqs))
			for i, r := range c.reqs {
				s.start(e, &request{conv: string(rune('a' + i)), prompt: r[0] + sharedPrefix, out: r[1], done: func(now float64) { got[i] = now }})
			}
			s.drain(1e9)
			for i := range got {
				if d := got[i] - c.want[i]; d > 0.01 || d < -0.01 {
					t.Errorf("request %d finished at %.3fs, want %.3fs", i, got[i], c.want[i])
				}
			}
		})
	}
}

// Reproduce stale peer load: dispatched requests are added immediately, but
// completed requests remain counted until the next poll. A quick follow-up
// can therefore leave its home engine despite an available slot.
func TestTheRoutersViewIsAsOldAsThePoll(t *testing.T) {
	for _, c := range []struct {
		name string
		poll float64
		at   float64 // when the router looks; the request ran from 0.5s to 1.5s
		want int
	}{
		{"exact view: a finished request is gone at once", 0, 1.7, 0},
		{"polled view: still counted after it finished", 2, 1.7, 1},
		{"polled view: gone after the next poll", 2, 2.1, 0},
		{"polled view: counted while it runs", 2, 1.0, 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := &engine{name: "e", slots: make([]slot, 2), pool: 1 << 30, prefill: 1000, decode: 100, pshare: 1, dshare: 1, dstarve: 100}
			s := testSim(c.poll, e)
			s.at(0.5, func(float64) {
				e.pending++ // as sendAt does on dispatch
				s.start(e, &request{conv: "a", prompt: sharedPrefix, out: 100, arrived: 0.5, done: func(float64) {}})
			})
			s.drain(c.at)
			if got := s.load(e); got != c.want {
				t.Errorf("router believes %d in flight at %.1fs, want %d", got, c.at, c.want)
			}
		})
	}
}
