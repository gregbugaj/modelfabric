package mesh

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/gregbugaj/modelfabric/internal/config"
)

func testMesh(t *testing.T) *Mesh {
	t.Helper()
	cfg := config.Default()
	cfg.LocalBias = 0.5
	cfg.Engines = []config.Engine{
		{Name: "gpu0", BaseURL: "http://127.0.0.1:18080"},
		{Name: "gpu1", BaseURL: "http://127.0.0.1:18081"},
	}
	return New(cfg, "self")
}

func setEngine(e *Engine, models ...string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.healthy, e.models = true, models
}

func addPeer(m *Mesh, node string, inflight int64, models ...string) *Peer {
	p := &Peer{Node: node, Addr: node, BaseURL: "http://" + node + ":1234"}
	p.models, p.reported, p.alive, p.lastSeen = models, inflight, true, time.Now()
	m.peers[node] = p
	return p
}

func names(cs []Candidate) []string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = c.Name
	}
	return out
}

func TestCandidatesPrefersLocalOnTies(t *testing.T) {
	m := testMesh(t)
	setEngine(m.engines[0], "qwen")
	addPeer(m, "predator", 0, "qwen")

	got := names(m.Candidates("qwen", false))
	if len(got) != 2 || got[0] != "gpu0" {
		t.Fatalf("expected local engine first, got %v", got)
	}
}

// llm-d bypasses the router counter. Using only the polled count instead
// would lose requests dispatched since the last poll, while adding the two
// counts would count router requests twice once the engine sees them.
func TestCandidatesAccountForObservedAndRoutedLoad(t *testing.T) {
	for _, tc := range []struct {
		name     string
		observed int
		routed   int64
		want     int64
	}{
		{"llm-d occupies the engine", 2, 0, 2},
		{"router dispatch follows idle poll", 0, 2, 2},
		{"overlapping observations are not added", 2, 2, 2},
		{"unpolled engine uses router count", -1, 2, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := testMesh(t)
			e := m.engines[0]
			setEngine(e, "qwen")
			e.SetSlots(2)
			// The router's count first, then the engine's reading: the reading
			// is compared with what the router had dispatched when it was
			// taken. In the other order every case here would be a request
			// dispatched after the poll, which is the second case alone.
			if tc.name != "router dispatch follows idle poll" {
				e.inflight.Store(tc.routed)
			}
			if tc.observed >= 0 {
				e.SetObservedInflight(tc.observed)
			}
			e.inflight.Store(tc.routed)
			cs := m.Candidates("qwen", true)
			if len(cs) != 1 {
				t.Fatalf("got %d candidates", len(cs))
			}
			if cs[0].Inflight != tc.want || !cs[0].Full() {
				t.Fatalf("inflight=%d full=%v, want %d and full", cs[0].Inflight, cs[0].Full(), tc.want)
			}
		})
	}
}

func TestCandidatesPrefersIdlePeerOverBusyLocal(t *testing.T) {
	m := testMesh(t)
	setEngine(m.engines[0], "qwen")
	m.engines[0].inflight.Store(4)
	addPeer(m, "predator", 0, "qwen")

	// LocalBias is worth half a request, so a local engine four requests deep
	// must lose to an idle peer.
	got := names(m.Candidates("qwen", false))
	if got[0] != "predator" {
		t.Fatalf("expected idle peer first, got %v", got)
	}
}

func TestCandidatesOrdersByLoad(t *testing.T) {
	m := testMesh(t)
	setEngine(m.engines[0], "qwen")
	setEngine(m.engines[1], "qwen")
	m.engines[0].inflight.Store(7)
	m.engines[1].inflight.Store(2)

	got := names(m.Candidates("qwen", false))
	if got[0] != "gpu1" {
		t.Fatalf("expected least-loaded engine first, got %v", got)
	}
}

func TestPeerPendingCountsBetweenPolls(t *testing.T) {
	m := testMesh(t)
	p := addPeer(m, "predator", 1, "qwen")
	if got := p.load(); got != 1 {
		t.Fatalf("load = %d, want 1", got)
	}
	// Requests dispatched before the next poll must still raise the peer's
	// apparent load, or a burst would all land on one node.
	cs := m.Candidates("qwen", false)
	release := cs[0].Acquire()
	defer release()
	if got := p.load(); got != 2 {
		t.Fatalf("load after dispatch = %d, want 2", got)
	}
}

func TestLocalOnlyExcludesPeers(t *testing.T) {
	m := testMesh(t)
	addPeer(m, "predator", 0, "qwen")

	// A forwarded request must never be forwarded again: with no local engine
	// serving the model there is nowhere legitimate to send it.
	if got := m.Candidates("qwen", true); len(got) != 0 {
		t.Fatalf("expected no candidates for forwarded request, got %v", names(got))
	}
	if got := m.Candidates("qwen", false); len(got) != 1 {
		t.Fatalf("expected the peer to be routable normally, got %v", names(got))
	}
}

func TestDeadPeerIsNotRoutable(t *testing.T) {
	m := testMesh(t)
	p := addPeer(m, "predator", 0, "qwen")
	p.alive = false
	if got := m.Candidates("qwen", false); len(got) != 0 {
		t.Fatalf("expected dead peer to be excluded, got %v", names(got))
	}
}

func TestUnhealthyEngineIsNotRoutable(t *testing.T) {
	m := testMesh(t)
	setEngine(m.engines[0], "qwen")
	m.engines[0].mu.Lock()
	m.engines[0].healthy = false
	m.engines[0].mu.Unlock()
	if got := m.Candidates("qwen", false); len(got) != 0 {
		t.Fatalf("expected unhealthy engine to be excluded, got %v", names(got))
	}
}

func TestModelsUnion(t *testing.T) {
	m := testMesh(t)
	setEngine(m.engines[0], "qwen", "shared")
	addPeer(m, "predator", 0, "shared", "llama")

	got := m.Models()
	want := []string{"llama", "qwen", "shared"}
	if len(got) != len(want) {
		t.Fatalf("Models() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Models() = %v, want %v", got, want)
		}
	}
}

func TestV4SkipsIPv6(t *testing.T) {
	n := &tsNode{TailscaleIPs: []string{"fd7a:115c:a1e0::e339:e108", "100.107.225.6"}}
	if got := n.v4(); got != "100.107.225.6" {
		t.Fatalf("v4() = %q, want the IPv4 address", got)
	}
	n6 := &tsNode{TailscaleIPs: []string{"fd7a:115c:a1e0::e339:e108"}}
	if got := n6.v4(); got != "" {
		t.Fatalf("v4() = %q, want empty for an IPv6-only peer", got)
	}
}

func TestExpireMarksStalePeersDead(t *testing.T) {
	m := testMesh(t)
	p := addPeer(m, "predator", 0, "qwen")
	p.lastSeen = time.Now().Add(-time.Hour)
	m.expire()
	p.mu.RLock()
	alive := p.alive
	p.mu.RUnlock()
	if alive {
		t.Fatal("expected a peer unseen for an hour to be marked dead")
	}
}

// LM Link's rule: "When the same model is available on multiple devices in the
// link, LM Link uses the preferred device to load and use the model."
func TestPreferredNodeWinsRegardlessOfLoad(t *testing.T) {
	m := testMesh(t)
	setEngine(m.engines[0], "qwen")
	addPeer(m, "predator", 0, "qwen")
	addPeer(m, "sites-01", 0, "qwen")

	m.SetPreferred("sites-01")
	got := names(m.Candidates("qwen", false))
	if got[0] != "sites-01" {
		t.Fatalf("preferred node not first: %v", got)
	}

	// A preference is strict precedence, not a score bonus: loading it up must
	// not dislodge it.
	m.peers["sites-01"].reported = 99
	got = names(m.Candidates("qwen", false))
	if got[0] != "sites-01" {
		t.Fatalf("a busy preferred node should still be first, got %v", got)
	}
}

// The fallback LM Studio leaves unspecified: a preference must never make a
// request impossible.
func TestPreferredNodeFallsBackWhenUnusable(t *testing.T) {
	m := testMesh(t)
	setEngine(m.engines[0], "qwen")
	addPeer(m, "predator", 0, "qwen")

	m.SetPreferred("sites-01")
	addPeer(m, "sites-01", 0, "some-other-model")
	got := names(m.Candidates("qwen", false))
	if len(got) != 2 {
		t.Fatalf("expected the other holders to remain routable, got %v", got)
	}

	m.peers["predator"].alive = true
	m.SetPreferred("predator")
	m.peers["predator"].alive = false
	got = names(m.Candidates("qwen", false))
	if len(got) == 0 {
		t.Fatal("a dead preferred node must not make the model unroutable")
	}
	for _, n := range got {
		if n == "predator" {
			t.Fatal("a dead node must not be a candidate")
		}
	}
}

// A request sent to an engine with no free slot queues, then evicts another
// conversation's cache; any engine with room must come first, even a busier
// one with more slots.
func TestCandidatesPutEnginesWithRoomFirst(t *testing.T) {
	m := testMesh(t)
	setEngine(m.engines[0], "qwen")
	setEngine(m.engines[1], "qwen")
	m.engines[0].SetSlots(2)
	m.engines[0].inflight.Store(2) // full
	m.engines[1].SetSlots(4)
	m.engines[1].inflight.Store(3) // busier, but has a slot
	cs := m.Candidates("qwen", false)
	if cs[0].Name != "gpu1" || !cs[1].Full() {
		t.Fatalf("the engine with a free slot should lead, got %v (full: %v)", names(cs), cs[1].Full())
	}
}

func TestUnknownSlotsAreNeverFull(t *testing.T) {
	m := testMesh(t)
	setEngine(m.engines[0], "qwen")
	m.engines[0].inflight.Store(50)
	if m.Candidates("qwen", false)[0].Full() {
		t.Fatal("an engine that never said its capacity must not be treated as full")
	}
}

func TestEqualLoadPrefersFasterPrefill(t *testing.T) {
	m := testMesh(t)
	setEngine(m.engines[0], "qwen")
	setEngine(m.engines[1], "qwen")
	// Trusted, because routing uses only a settled rate: a figure from a few
	// short prompts would place real traffic on the strength of nothing.
	m.engines[0].SetRates(EngineRates{PrefillTokS: 900, Trusted: true})
	m.engines[1].SetRates(EngineRates{PrefillTokS: 2400, Trusted: true})
	if got := names(m.Candidates("qwen", false)); got[0] != "gpu1" {
		t.Fatalf("expected the faster engine first, got %v", got)
	}
}

// A peer is one candidate for its whole node: its capacity for a model is its
// engines serving that model, plus what we have sent it since the last poll.
func TestPeerCapacityCoversItsEnginesForTheModel(t *testing.T) {
	m := testMesh(t)
	p := addPeer(m, "minion", 1, "qwen", "other")
	p.engines = []EngineState{
		{Name: "a", Healthy: true, Models: []string{"qwen"}, Slots: 2, EngineStats: EngineStats{Inflight: 1, PrefillTokS: 850}},
		{Name: "b", Healthy: true, Models: []string{"other"}, Slots: 4, EngineStats: EngineStats{Inflight: 4, PrefillTokS: 3000}},
		{Name: "c", Healthy: false, Models: []string{"qwen"}, Slots: 2, EngineStats: EngineStats{Inflight: 0}},
	}
	c := m.Candidates("qwen", false)
	var peer Candidate
	for _, x := range c {
		if x.Name == "minion" {
			peer = x
		}
	}
	if peer.Slots != 2 || peer.Inflight != 1 || peer.PrefillTokS != 850 {
		t.Fatalf("peer capacity = %d in flight of %d slots at %.0f tok/s; want 1 of 2 at 850", peer.Inflight, peer.Slots, peer.PrefillTokS)
	}
	release := peer.Acquire()
	defer release()
	if in, _, _ := p.capacity("qwen"); in != 2 {
		t.Fatalf("a dispatch since the last poll must count, got %d in flight", in)
	}
}

func TestPrefillRateNeedsHistory(t *testing.T) {
	body := "llamacpp:prompt_tokens_total 500\nllamacpp:prompt_seconds_total 1\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, body) }))
	defer srv.Close()
	m := testMesh(t)
	if _, ok := m.fetchRates(context.Background(), srv.URL); ok {
		t.Fatal("500 tokens is too little to measure from")
	}
	body = "# HELP x\nllamacpp:prompt_tokens_total 90000\nllamacpp:prompt_seconds_total 60\n"
	if rates, ok := m.fetchRates(context.Background(), srv.URL); !ok || rates.PrefillTokS != 1500 {
		t.Fatalf("rate = %v, %v; want 1500", rates.PrefillTokS, ok)
	}
}

// Candidates, State and the engine poll all walked m.engines without the lock
// that RegisterEngine/UnregisterEngine hold, so loading or unloading a model
// while requests were being routed raced on the slice's backing array. Run
// with -race, this fails on the unsnapshotted version.
func TestEngineSliceIsNotRacedByLoadAndRoute(t *testing.T) {
	m := New(config.Default(), "self")
	done := make(chan struct{})
	var wg sync.WaitGroup
	churn := func(id string) {
		defer wg.Done()
		for {
			select {
			case <-done:
				return
			default:
			}
			e := NewEngine(id, "http://127.0.0.1:1")
			e.MarkReady("m")
			m.RegisterEngine(e)
			m.UnregisterEngine(id)
		}
	}
	read := func() {
		defer wg.Done()
		for {
			select {
			case <-done:
				return
			default:
			}
			_ = m.Candidates("m", true)
			_ = m.State()
			_ = m.Engines()
		}
	}
	for i, id := range []string{"a", "b", "c"} {
		wg.Add(1)
		go churn(id)
		_ = i
	}
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go read()
	}
	time.Sleep(150 * time.Millisecond)
	close(done)
	wg.Wait()
}
