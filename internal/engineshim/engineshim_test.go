package engineshim

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// The shape llama.cpp actually returns, captured from a running engine.
const realSlots = `[
 {"id":0,"n_ctx":32768,"speculative":true,"is_processing":false},
 {"id":1,"n_ctx":32768,"speculative":true,"is_processing":true,"id_task":60466,
  "n_prompt_tokens":420,"n_prompt_tokens_processed":19,"n_prompt_tokens_cache":42}
]`

func TestKVUsageCountsResidentTokens(t *testing.T) {
	var slots []Slot
	if err := json.Unmarshal([]byte(realSlots), &slots); err != nil {
		t.Fatalf("the captured /slots shape no longer parses: %v", err)
	}
	got, ok := KVUsage(slots)
	if !ok {
		t.Fatal("no usage from a valid /slots response")
	}
	if want := 420.0 / 65536.0; got != want {
		t.Errorf("KVUsage = %v, want %v (one 420-token prompt in a two-slot pool)", got, want)
	}
}

// Measured on a real engine: while the model generates, is_processing goes
// false and the slot keeps its prompt — and it keeps it after the request ends
// too, as a cache for the next one. Gating on is_processing reported an idle
// engine throughout a long generation.
func TestKVUsageCountsSlotsThatAreNotProcessing(t *testing.T) {
	generating := []Slot{
		{ID: 0, NCtx: 16384},
		{ID: 1, NCtx: 16384, IsProcessing: false, NPromptTokens: 917},
	}
	got, ok := KVUsage(generating)
	if !ok || got == 0 {
		t.Fatalf("KVUsage = (%v, %v); a slot holding 917 tokens is not an empty cache", got, ok)
	}
	if want := 917.0 / 32768.0; got != want {
		t.Errorf("KVUsage = %v, want %v", got, want)
	}
}

func TestKVUsageEdges(t *testing.T) {
	for _, c := range []struct {
		name  string
		slots []Slot
		want  float64
		ok    bool
	}{
		{"cold engine", []Slot{{NCtx: 1024}, {NCtx: 1024}}, 0, true},
		{"both slots hold a prompt", []Slot{{NCtx: 1000, IsProcessing: true, NPromptTokens: 500},
			{NCtx: 1000, NPromptTokens: 250}}, 0.375, true},
		// A prompt longer than its slot (context shifting) must not report
		// more than a full cache.
		{"over capacity", []Slot{{NCtx: 100, IsProcessing: true, NPromptTokens: 400}}, 1, true},
		// Nothing to divide by: report no value rather than a zero a
		// scheduler would read as "idle".
		{"no slots", nil, 0, false},
		{"no context reported", []Slot{{NCtx: 0, IsProcessing: true, NPromptTokens: 10}}, 0, false},
	} {
		got, ok := KVUsage(c.slots)
		if got != c.want || ok != c.ok {
			t.Errorf("%s: KVUsage = (%v, %v), want (%v, %v)", c.name, got, ok, c.want, c.ok)
		}
	}
}

func TestAugmentAppendsAGaugeAndKeepsTheEnginesOwn(t *testing.T) {
	metrics := []byte("llamacpp:requests_processing 1\nllamacpp:requests_deferred 0\n")
	out := string(Augment(metrics, 0.25, true))
	if !strings.Contains(out, "llamacpp:requests_processing 1") {
		t.Error("the engine's own metrics were lost")
	}
	if !strings.Contains(out, "# TYPE "+KVUsageMetric+" gauge") || !strings.Contains(out, KVUsageMetric+" 0.250000") {
		t.Errorf("gauge missing or malformed:\n%s", out)
	}
	// No value: publish nothing rather than a zero.
	if got := string(Augment(metrics, 0, false)); got != string(metrics) {
		t.Errorf("unknown usage still wrote a gauge:\n%s", got)
	}
	// An engine that publishes its own is left alone.
	own := []byte(KVUsageMetric + " 0.9\n")
	if got := string(Augment(own, 0.1, true)); got != string(own) {
		t.Errorf("overwrote the engine's own gauge:\n%s", got)
	}
}

// Everything but /metrics reaches the engine unchanged, and /metrics comes
// back with the gauge added.
func TestShimProxiesAndAugments(t *testing.T) {
	var gotPath, gotBody string
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		switch r.URL.Path {
		case "/metrics":
			_, _ = w.Write([]byte("llamacpp:requests_processing 1\n"))
		case "/slots":
			_, _ = w.Write([]byte(realSlots))
		default:
			b, _ := io.ReadAll(r.Body)
			gotBody = string(b)
			_, _ = w.Write([]byte(`{"ok":true}`))
		}
	}))
	defer engine.Close()

	s, err := New(engine.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	port, err := s.Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	base := "http://127.0.0.1:" + strconv.Itoa(port)

	resp, err := http.Get(base + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), "llamacpp:requests_processing 1") || !strings.Contains(string(body), KVUsageMetric) {
		t.Errorf("/metrics did not pass through and augment:\n%s", body)
	}

	resp, err = http.Post(base+"/v1/chat/completions", "application/json", strings.NewReader(`{"model":"m"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if gotPath != "/v1/chat/completions" || gotBody != `{"model":"m"}` {
		t.Errorf("inference was not proxied verbatim: path=%q body=%q", gotPath, gotBody)
	}
}

// An engine that cannot be reached must look broken to the scraper, not idle.
func TestShimReportsUnreachableEngine(t *testing.T) {
	s, err := New("http://127.0.0.1:1", nil)
	if err != nil {
		t.Fatal(err)
	}
	port, err := s.Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	resp, err := http.Get("http://127.0.0.1:" + strconv.Itoa(port) + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %s, want 502", resp.Status)
	}
}

// New accepted anything url.Parse accepted, so a bad engine address failed
// later as a mystery scrape error instead of at configuration time.
func TestNewRejectsAnAddressThatIsNotAnOrigin(t *testing.T) {
	for _, bad := range []string{"127.0.0.1:18000", "", "unix:///tmp/sock", "http://"} {
		if _, err := New(bad, nil); err == nil {
			t.Errorf("New(%q) was accepted", bad)
		}
	}
	if _, err := New("http://127.0.0.1:18000", nil); err != nil {
		t.Errorf("a normal engine address was rejected: %v", err)
	}
}

// The duplicate check was a substring search: it matched the metric's own HELP
// line (so a HELP without a sample suppressed the gauge) and missed a labelled
// or tab-separated sample (so the gauge was emitted twice).
func TestAugmentRecognisesAnExistingSample(t *testing.T) {
	gauge := KVUsageMetric
	already := []string{
		gauge + " 0.5",
		gauge + "\t0.5",
		gauge + `{slot="0"} 0.5`,
	}
	for _, m := range already {
		if got := Augment([]byte(m), 0.25, true); !bytes.Equal(got, []byte(m)) {
			t.Errorf("augmented an exposition that already had %q", m)
		}
	}
	// A HELP line alone is not a sample; the gauge is still owed.
	helpOnly := "# HELP " + gauge + " something\n# TYPE " + gauge + " gauge\n"
	if got := Augment([]byte(helpOnly), 0.25, true); !bytes.Contains(got, []byte(gauge+" 0.250000")) {
		t.Errorf("a HELP line without a sample suppressed the gauge: %s", got)
	}
	// An unrelated metric with the same prefix does not count.
	other := gauge + "_total 3\n"
	if got := Augment([]byte(other), 0.25, true); !bytes.Contains(got, []byte(gauge+" 0.250000")) {
		t.Errorf("a differently-named metric suppressed the gauge: %s", got)
	}
}

// Listening twice left the first server running and unreachable by Close.
func TestListenTwiceIsRefused(t *testing.T) {
	sh, err := New("http://127.0.0.1:1", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sh.Listen("127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}
	defer sh.Close()
	if _, err := sh.Listen("127.0.0.1:0"); err == nil {
		t.Fatal("a second Listen was allowed, leaking the first listener")
	}
}

// Requests in flight, counted from the same /slots read that gives KV usage.
// Measured on llama.cpp 2.40.0: is_processing stays true through a 700-token
// generation and matches llamacpp:requests_processing.
func TestBusySlots(t *testing.T) {
	for _, tc := range []struct {
		name  string
		slots []Slot
		want  int
		ok    bool
	}{
		{"none reported", nil, 0, false},
		{"all idle", []Slot{{ID: 0}, {ID: 1}}, 0, true},
		{"one busy", []Slot{{ID: 0, IsProcessing: true}, {ID: 1}}, 1, true},
		{"all busy", []Slot{{ID: 0, IsProcessing: true}, {ID: 1, IsProcessing: true}}, 2, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := BusySlots(tc.slots)
			if got != tc.want || ok != tc.ok {
				t.Errorf("got (%d, %v), want (%d, %v)", got, ok, tc.want, tc.ok)
			}
		})
	}
}

type fakePlacer struct {
	mu      sync.Mutex
	chains  int
	slot    int
	results []int
}

func (p *fakePlacer) Place(_ context.Context, engine string, chain [][32]byte) (int, func(int)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if engine != "inst-1" || len(chain) == 0 {
		return -1, func(int) {}
	}
	p.chains++
	return p.slot, func(status int) {
		p.mu.Lock()
		defer p.mu.Unlock()
		p.results = append(p.results, status)
	}
}

// The disk cache places requests in the shim, the one hop ModelFabric's
// router, a peer's forwarded request and llm-d's Envoy all pass. In the
// router, requests llm-d scheduled never reached it.
func TestShimPlacesRequestsForTheCache(t *testing.T) {
	long := strings.Repeat("the same long prompt ", 200)
	tests := []struct {
		name     string
		body     string
		cache    bool
		wantSlot string // the id_slot the engine must see; "" for none
		wantDone []int
	}{
		{name: "a generating request is pinned to the cache's slot, and its status reported",
			body: `{"model":"m","messages":[{"role":"user","content":"` + long + `"}]}`, cache: true,
			wantSlot: "2", wantDone: []int{200}},
		{name: "a request that pins its own slot is left to it",
			body: `{"model":"m","id_slot":0,"messages":[{"role":"user","content":"` + long + `"}]}`, cache: true,
			wantSlot: "0"},
		{name: "without the cache nothing is pinned",
			body: `{"model":"m","messages":[{"role":"user","content":"` + long + `"}]}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var seen string
			engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				b, _ := io.ReadAll(r.Body)
				var probe struct {
					IDSlot *int `json:"id_slot"`
				}
				json.Unmarshal(b, &probe)
				if probe.IDSlot != nil {
					seen = strconv.Itoa(*probe.IDSlot)
				}
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, "data: {}\n\n")
				w.(http.Flusher).Flush()
				fmt.Fprint(w, "data: [DONE]\n\n")
			}))
			t.Cleanup(engine.Close)
			sh, err := New(engine.URL, nil)
			if err != nil {
				t.Fatal(err)
			}
			port, err := sh.Listen("127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { sh.Close() })
			p := &fakePlacer{slot: 2}
			if tc.cache {
				sh.SetCache("inst-1", p)
			}
			resp, err := http.Post(fmt.Sprintf("http://127.0.0.1:%d/v1/chat/completions", port), "application/json", strings.NewReader(tc.body))
			if err != nil {
				t.Fatal(err)
			}
			out, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if !strings.Contains(string(out), "[DONE]") {
				t.Fatalf("the stream did not come through: %q", out)
			}
			if seen != tc.wantSlot {
				t.Fatalf("engine saw id_slot %q, want %q", seen, tc.wantSlot)
			}
			p.mu.Lock()
			defer p.mu.Unlock()
			if fmt.Sprint(p.results) != fmt.Sprint(tc.wantDone) && !(len(p.results) == 0 && len(tc.wantDone) == 0) {
				t.Fatalf("statuses reported %v, want %v", p.results, tc.wantDone)
			}
		})
	}
}
