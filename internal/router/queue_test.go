package router

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gregbugaj/modelfabric/internal/mesh"
)

func queueFleet(x, m, h int64) []mesh.Candidate {
	return []mesh.Candidate{
		{Name: "xpredator", Local: true, Inflight: x, Slots: 2, PrefillTokS: 1761},
		{Name: "minion", Local: true, Inflight: m, Slots: 4, PrefillTokS: 840},
		{Name: "helion", Local: true, Inflight: h, Slots: 1, PrefillTokS: 264},
	}
}

func testQueue() (*Queue, *time.Time) {
	now := time.Unix(1000, 0)
	q := NewQueue(2*time.Second, 60*time.Second)
	q.now = func() time.Time { return now }
	return q, &now
}

// Recently freed slots retain conversation affinity during Grace. Requests
// without a suitable destination wait in the router instead of an engine queue.
func TestQueueDecides(t *testing.T) {
	for _, c := range []struct {
		name  string
		cands []mesh.Candidate
		home  string
		// freed is engines that had a slot vacated this many seconds ago; with
		// a negative number, every free slot there was.
		freed  map[string]float64
		waited time.Duration
		ahead  bool
		want   string // the engine it goes to, "" to keep waiting
	}{
		{"home has a slot: goes at once", queueFleet(1, 4, 1), "xpredator", nil, 0, false, "xpredator"},
		{"home has a slot, even one vacated a moment ago: it is its own",
			queueFleet(1, 4, 1), "xpredator", map[string]float64{"xpredator": 0.2}, 0, false, "xpredator"},
		{"home has a slot: goes at once though others have waited longer",
			queueFleet(1, 4, 1), "xpredator", nil, 0, true, "xpredator"},
		// Find the free home slot even when speed ranking places another engine
		// first; otherwise requests wait unnecessarily and displace other caches.
		{"home has a slot but is not first in the order: home, at once",
			queueFleet(1, 4, 0), "helion", map[string]float64{"xpredator": 0.2, "helion": 0.2}, 0, false, "helion"},
		{"home has a slot, and the much faster engine has one that has stayed free: the faster",
			queueFleet(1, 4, 0), "helion", map[string]float64{"helion": 0.2}, 0, false, "xpredator"},
		{"home is shared and a faster engine stands idle: leaves for it",
			queueFleet(0, 3, 0), "minion", map[string]float64{"minion": 0.2}, 0, false, "xpredator"},
		{"home is shared, the faster engine has one neighbour and a slot nobody owns: leaves",
			queueFleet(1, 3, 0), "minion", map[string]float64{"minion": 0.2}, 0, false, "xpredator"},
		{"the faster engine's free slots were vacated a moment ago: they are someone's, stays",
			queueFleet(0, 1, 0), "minion", map[string]float64{"minion": 0.2, "xpredator": -0.3}, 0, false, "minion"},
		{"it would have as many neighbours there as at home: stays",
			queueFleet(1, 1, 0), "minion", map[string]float64{"minion": 0.2}, 0, false, "minion"},
		{"alone at home: stays, the faster engine is not three times faster",
			queueFleet(0, 0, 0), "minion", map[string]float64{"minion": 0.2}, 0, false, "minion"},
		{"never leaves a shared home for a slower engine",
			queueFleet(2, 3, 0), "minion", map[string]float64{"minion": 0.2}, 0, false, "minion"},
		{"everything full: waits", queueFleet(2, 4, 1), "", nil, 0, false, ""},
		// The open-slot rule gave this slot away and its owner, back 0.2s
		// later, took the next conversation's.
		{"a slot vacated 0.2s ago is not offered", queueFleet(1, 4, 1), "minion",
			map[string]float64{"xpredator": 0.2}, 0, false, ""},
		{"a slot that has stayed free past the grace is taken", queueFleet(1, 4, 1), "minion",
			map[string]float64{"xpredator": 2.5}, 0, false, "xpredator"},
		{"a slot never used is free to take", queueFleet(2, 3, 1), "", nil, 0, false, "minion"},
		{"two slots open, one just vacated: the other is taken", queueFleet(2, 2, 1), "xpredator",
			map[string]float64{"minion": 0.2}, 0, false, "minion"},
		{"a settled slot, but another request has waited longer", queueFleet(2, 3, 1), "", nil, 0, true, ""},
		{"waited the longest allowed: goes where placement says", queueFleet(2, 4, 1), "", nil, 61 * time.Second, false, "helion"},
	} {
		t.Run(c.name, func(t *testing.T) {
			q, now := testQueue()
			for name, ago := range c.freed {
				n := int64(1)
				if ago < 0 {
					ago = -ago
					for _, cand := range c.cands {
						if cand.Name == name {
							n = cand.Slots - cand.Inflight
						}
					}
				}
				for range n {
					q.freed[name] = append(q.freed[name], now.Add(-time.Duration(ago*float64(time.Second))))
				}
			}
			w := &waiter{since: now.Add(-c.waited)}
			if c.ahead {
				q.waiting = []*waiter{{since: now.Add(-time.Minute)}, w}
			}
			ordered := applyAffinity(c.cands, c.home, "")
			out, ok := q.decide(ordered, c.home, w)
			got := ""
			if ok {
				got = out[0].Name
			}
			if got != c.want {
				t.Errorf("goes to %q, want %q", got, c.want)
			}
		})
	}
}

// Regression: reading mesh load before locking can miss a request that was
// admitted, dispatched, and removed from pending, admitting the same slot twice.
func TestQueueDoesNotGiveOneSlotTwice(t *testing.T) {
	q, _ := testQueue()
	before := queueFleet(2, 3, 1) // one slot open, on minion
	first := &waiter{since: q.now()}
	out, ok := q.decide(before, "", first)
	if !ok || out[0].Name != "minion" {
		t.Fatalf("first request: got %v %v, want minion", out, ok)
	}
	g := q.grant(out[0].Name, first, nil)
	if out, ok := q.decide(before, "", &waiter{since: q.now()}); ok {
		t.Errorf("second request was given %s before the first was counted", out[0].Name)
	}
	q.Sent(g)
	if n := q.pending["minion"]; n != 0 {
		t.Errorf("%d still pending on minion after its request was sent", n)
	}
	if _, ok := q.decide(queueFleet(2, 4, 1), "", &waiter{since: q.now()}); ok {
		t.Error("the slot is taken now, and counted: nothing should be offered")
	}
	q.Done(g, 0, 0)
	q.Done(q.grant("minion", &waiter{since: q.now()}, nil), 0, 0)
	if n := q.pending["minion"]; n != 0 {
		t.Errorf("pending on minion is %d after everything ended, want 0", n)
	}
}

// Regression: free slots do not guarantee room in the shared KV pool.
// Oversized residents can repeatedly evict each other while a slot stays free.
func TestQueuePlacesBySizeNotOnlySlots(t *testing.T) {
	pools := func(cs []mesh.Candidate) []mesh.Candidate {
		for i := range cs {
			cs[i].KVPool = map[string]int64{"xpredator": 131072, "minion": 262144, "helion": 65536}[cs[i].Name]
		}
		return cs
	}
	live := func(q *Queue, id byte, engine string, tokens int64) *resident {
		r := &resident{leaf: [32]byte{id}, engine: engine, tokens: tokens, inflight: true}
		q.residents[r.leaf] = r
		return r
	}
	for _, c := range []struct {
		name   string
		cands  []mesh.Candidate
		others []int64
		mine   int64 // this conversation's size; its home is minion
		onFast int64
		want   string
	}{
		// The 5090 is given one request running in these, so that nothing but
		// size decides: a home with two neighbours beside an idle faster
		// engine is left anyway (see leave).
		{"it fits beside the others: home", queueFleet(1, 1, 0), []int64{85000, 60000}, 97000, 0, "minion"},
		{"the run's case: it does not fit beside the other two, and the 5090 has room",
			queueFleet(1, 1, 0), []int64{85000, 92000}, 97000, 0, "xpredator"},
		{"nor does it fit on the 5090 beside what lives there: waits",
			queueFleet(1, 2, 1), []int64{85000, 92000}, 97000, 60000, ""},
		{"small conversations are not sent away from a home with room",
			queueFleet(1, 1, 0), []int64{85000, 92000}, 20000, 0, "minion"},
	} {
		t.Run(c.name, func(t *testing.T) {
			q, _ := testQueue()
			for i, n := range c.others {
				live(q, byte(i+1), "minion", n)
			}
			if c.onFast > 0 {
				live(q, 9, "xpredator", c.onFast)
			}
			me := &resident{leaf: [32]byte{100}, engine: "minion", tokens: c.mine}
			q.residents[me.leaf] = me
			w := &waiter{since: q.now(), res: me, need: c.mine + headroom}
			out, ok := q.decide(applyAffinity(pools(c.cands), "minion", ""), "minion", w)
			got := ""
			if ok {
				got = out[0].Name
			}
			if got != c.want {
				t.Errorf("goes to %q, want %q", got, c.want)
			}
		})
	}
	t.Run("larger than any engine's pool: goes home at once, there is nothing to wait for", func(t *testing.T) {
		q, _ := testQueue()
		me := &resident{leaf: [32]byte{100}, engine: "minion", tokens: 300000}
		q.residents[me.leaf] = me
		w := &waiter{since: q.now(), res: me, need: 300000 + headroom}
		out, ok := q.decide(applyAffinity(pools(queueFleet(1, 1, 0)), "minion", ""), "minion", w)
		if !ok || out[0].Name != "minion" {
			t.Errorf("got %v ok=%v, want minion at once", order(out), ok)
		}
	})
	t.Run("fits nowhere now, waited the longest allowed: where the others lose least", func(t *testing.T) {
		q, now := testQueue()
		// The additional 97k tokens exceed these pools by 20k and 34k respectively.
		live(q, 1, "minion", 85000)
		live(q, 2, "minion", 92000)
		live(q, 9, "xpredator", 60000)
		me := &resident{leaf: [32]byte{100}, engine: "minion", tokens: 97000}
		q.residents[me.leaf] = me
		w := &waiter{since: now.Add(-q.MaxWait - time.Second), res: me, need: 97000 + headroom}
		out, ok := q.decide(applyAffinity(pools(queueFleet(1, 2, 1)), "minion", ""), "minion", w)
		if !ok || out[0].Name != "minion" {
			t.Errorf("got %v ok=%v, want minion: 20k over there against 34k on xpredator", order(out), ok)
		}
		q.residents[[32]byte{9}].tokens = 30000
		out, ok = q.decide(applyAffinity(pools(queueFleet(1, 2, 1)), "minion", ""), "minion", w)
		if !ok || out[0].Name != "xpredator" {
			t.Errorf("got %v ok=%v, want xpredator: 4k over there against 20k at home", order(out), ok)
		}
	})
	t.Run("a conversation that ended makes room", func(t *testing.T) {
		q, now := testQueue()
		a := live(q, 1, "minion", 85000)
		live(q, 2, "minion", 92000)
		me := &resident{leaf: [32]byte{100}, engine: "minion", tokens: 97000}
		w := &waiter{since: q.now(), res: me, need: 97000 + headroom}
		minion := pools(queueFleet(2, 1, 1))[1]
		if q.fits(minion, w) {
			t.Fatal("three conversations of 85, 92 and 97 thousand tokens do not fit in 262,144")
		}
		a.inflight, a.lastDone = false, *now
		*now = now.Add(q.Grace + time.Second)
		if !q.fits(minion, w) {
			t.Error("the first conversation ended a grace ago: its tokens are no longer in the way")
		}
	})
}

func TestAdmitWaitsAndWakes(t *testing.T) {
	q := NewQueue(10*time.Millisecond, time.Minute)
	q.tick = 5 * time.Millisecond
	var open atomic.Bool
	done := make(chan string, 1)
	go func() {
		out, _, _ := q.Admit(context.Background(), Ask{}, func() ([]mesh.Candidate, string) {
			if !open.Load() {
				return queueFleet(2, 4, 1), ""
			}
			return queueFleet(2, 3, 1), ""
		})
		done <- out[0].Name
	}()
	deadline := time.Now().Add(2 * time.Second)
	for q.Waiting() != 1 {
		if time.Now().After(deadline) {
			t.Fatal("request was not held")
		}
		time.Sleep(time.Millisecond)
	}
	open.Store(true)
	q.Done(&Grant{name: "minion"}, 0, 0)
	select {
	case got := <-done:
		if got != "minion" {
			t.Errorf("went to %s, want minion", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("request was never let through")
	}
	if n := q.Waiting(); n != 0 {
		t.Errorf("%d still listed as waiting", n)
	}

	ctx, cancel := context.WithCancel(context.Background())
	gone := make(chan struct{})
	go func() {
		q.Admit(ctx, Ask{}, func() ([]mesh.Candidate, string) { return queueFleet(2, 4, 1), "" })
		close(gone)
	}()
	cancel()
	select {
	case <-gone:
	case <-time.After(2 * time.Second):
		t.Fatal("a request whose caller left stayed in the queue")
	}
	if n := q.Waiting(); n != 0 {
		t.Errorf("%d still listed as waiting after its caller left", n)
	}
}

// The queue in the request path, on a real router with a one-slot engine: a
// second conversation arriving while the slot is busy is held here, not sent
// to wait inside the engine, and is let through once the slot has stayed free.
func TestForwardHoldsARequestUntilASlotSettles(t *testing.T) {
	var inEngine atomic.Int32
	var most atomic.Int32
	hold := make(chan struct{})
	eng := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := inEngine.Add(1)
		for {
			old := most.Load()
			if n <= old || most.CompareAndSwap(old, n) {
				break
			}
		}
		<-hold
		inEngine.Add(-1)
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer eng.Close()

	r := routerWith(t, eng)
	for _, e := range r.m.Engines() {
		e.SetSlots(1)
	}
	r.EnablePrefixAffinity()
	r.EnableQueue(20*time.Millisecond, time.Minute)
	r.queue.tick = 5 * time.Millisecond

	send := func(text string) chan int {
		done := make(chan int, 1)
		go func() {
			rec := httptest.NewRecorder()
			body := `{"model":"m","messages":[{"role":"user","content":"` + text + strings.Repeat(" x", 2000) + `"}]}`
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
			r.Forward(rec, req, "/v1/chat/completions")
			done <- rec.Code
		}()
		return done
	}
	waitFor := func(what string, ok func() bool) {
		t.Helper()
		for end := time.Now().Add(5 * time.Second); !ok(); {
			if time.Now().After(end) {
				t.Fatalf("timed out waiting for %s", what)
			}
			time.Sleep(time.Millisecond)
		}
	}

	first := send("conversation one")
	waitFor("the first request to reach the engine", func() bool { return inEngine.Load() == 1 })
	second := send("conversation two")
	waitFor("the second request to be held at the router", func() bool { return r.Queued() == 1 })
	if n := inEngine.Load(); n != 1 {
		t.Fatalf("%d requests inside a one-slot engine: the second was sent to queue there", n)
	}

	hold <- struct{}{} // the first finishes
	if code := <-first; code != http.StatusOK {
		t.Fatalf("first request: status %d", code)
	}
	waitFor("the second request to be let through", func() bool { return inEngine.Load() == 1 && r.Queued() == 0 })
	hold <- struct{}{}
	if code := <-second; code != http.StatusOK {
		t.Fatalf("second request: status %d", code)
	}
	if most.Load() != 1 {
		t.Errorf("the engine saw %d requests at once, want never more than its 1 slot", most.Load())
	}
}

// The size the queue places by comes from the engine's own count, read from
// the end of the response as it passes: a body may be megabytes or a stream,
// and is not the router's to hold.
func TestReadUsage(t *testing.T) {
	for _, c := range []struct {
		name, tail string
		want       usage
	}{
		{"a chat completion", `..."finish_reason":"stop"}],"usage":{"completion_tokens":157,"prompt_tokens":1688,"total_tokens":1845,"prompt_tokens_details":{"cached_tokens":0}},"timings":{"cache_n":0,"prompt_n":1688}}`, usage{prompt: 1688, completion: 157}},
		{"the last chunk of a stream", "data: {\"choices\":[],\"usage\":{\"prompt_tokens\": 97006, \"completion_tokens\": 188}}\n\ndata: [DONE]\n\n", usage{prompt: 97006, completion: 188}},
		{"an answer that talks about usage is not usage", `{"choices":[{"message":{"content":"set \"prompt_tokens\": 5 in the config"}}],"usage":{"prompt_tokens":12,"completion_tokens":9}}`, usage{prompt: 12, completion: 9}},
		{"what the engine says about its cache and speed", `"usage":{"prompt_tokens":2100,"completion_tokens":48,"prompt_tokens_details":{"cached_tokens":2000}},"timings":{"cache_n":2000,"prompt_n":100,"prompt_ms":120.5,"predicted_per_second":43.25,"draft_n":40,"draft_n_accepted":30}}`,
			usage{prompt: 2100, completion: 48, cached: 2000, promptMillis: 120.5, tokensPerSec: 43.25, drafted: 40, draftAccepted: 30}},
		// Most agents stream without asking for a usage block. llama.cpp's last
		// chunk still carries its timings, and a streamed request used to end
		// with no size at all (seen live 2026-10-07: "finished: 200 in 500ms"
		// and nothing about tokens).
		{"a stream with no usage block, sized from the engine's timings", "data: {\"choices\":[{\"finish_reason\":\"stop\",\"delta\":{}}],\"timings\":{\"cache_n\":1978,\"prompt_n\":4,\"prompt_ms\":152.1,\"predicted_n\":40,\"predicted_per_second\":114.1}}\n\ndata: [DONE]\n\n",
			usage{prompt: 1982, completion: 40, cached: 1978, promptMillis: 152.1, tokensPerSec: 114.1}},
		{"no usage in the response", `{"error":{"message":"context size has been exceeded"}}`, usage{}},
		{"cut off before the number", `"usage":{"prompt_tokens":`, usage{}},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := readUsage([]byte(c.tail)); got != c.want {
				t.Errorf("got %+v, want %+v", got, c.want)
			}
		})
	}
	var tail []byte
	for range 100 {
		tail = keepTail(tail, make([]byte, 1000))
	}
	tail = keepTail(tail, []byte(`"usage":{"prompt_tokens":7,"completion_tokens":3}}`))
	if len(tail) > usageTail {
		t.Errorf("kept %d bytes, want at most %d", len(tail), usageTail)
	}
	if got := readUsage(tail); got != (usage{prompt: 7, completion: 3}) {
		t.Errorf("after a long response: got %+v", got)
	}
}
