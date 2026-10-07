package router

import (
	"bufio"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gregbugaj/modelfabric/internal/mesh"
)

func placementBody(text string) []byte {
	return []byte(`{"model":"m","stream":true,"messages":[{"role":"user","content":"` + text + `"}]}`)
}

func placementFleet() []mesh.Candidate {
	return []mesh.Candidate{{Name: "a", Local: true, Slots: 1, PrefillTokS: 1000}, {Name: "b", Local: true, Slots: 1, PrefillTokS: 1000}}
}

// The old 64 KiB chain made every later turn appear fully cached, even
// when a tool appended hundreds of kilobytes of new text.
func TestPlacementChargesLongPromptExtensions(t *testing.T) {
	for _, tc := range []struct {
		name string
		size int
	}{
		{"extension beyond old 64 KiB cap is charged", 80 << 10},
		{"extension beyond disk cache cap is charged", 5 << 20},
	} {
		t.Run(tc.name, func(t *testing.T) {
			size := tc.size
			p := NewPlacement(nil)
			first := p.Order(placementFleet(), "m", placementBody(strings.Repeat("a", size)), "")
			a1 := p.Placed(first, "a")
			a1.Finished(int64(size/4), 1000)
			longer := p.Order(placementFleet(), "m", placementBody(strings.Repeat("a", size)+strings.Repeat("b", 240<<10)), "")
			if longer.cost("a") < 50000 {
				t.Errorf("new 240 KiB suffix costs %d tokens, want at least 50000", longer.cost("a"))
			}
		})
	}
}

func TestPlacementDistinguishesLongPromptBranches(t *testing.T) {
	p := NewPlacement(nil)
	shared := strings.Repeat("a", 80<<10)
	first := p.Order(placementFleet(), "m", placementBody(shared+strings.Repeat("b", 80<<10)), "")
	a1 := p.Placed(first, "a")
	a1.Finished(40000, 1000)
	branch := p.Order(placementFleet(), "m", placementBody(shared+strings.Repeat("c", 80<<10)), "")
	if branch.conv != nil || branch.shares["a"] >= stickyShare {
		t.Fatalf("different suffix inherited the first conversation: %s, resident %v", branch.Explain(), branch.conv)
	}
	if branch.Candidates[0].Name != "b" {
		t.Fatal("a different conversation must use the engine with room")
	}
}

// An overlapping extension moves the resident's leaf before the original
// request finishes. Completion must still release the original reservation.
func TestPlacementReleasesOverlappingContinuations(t *testing.T) {
	for _, reverse := range []bool{false, true} {
		name := "original finishes first"
		if reverse {
			name = "extension finishes first"
		}
		t.Run(name, func(t *testing.T) {
			now := time.Unix(1000, 0)
			p := NewPlacement(func() time.Time { return now })
			first := p.Order(placementFleet(), "m", placementBody(strings.Repeat("a", 4096)), "")
			a1 := p.Placed(first, "a")
			next := p.Order(placementFleet(), "m", placementBody(strings.Repeat("a", 8192)), "")
			a2 := p.Placed(next, "a")
			if reverse {
				a1, a2 = a2, a1
			}
			a1.Finished(0, 0)
			a2.Finished(0, 0)
			if p.homes[next.blocks[len(next.blocks)-1]] == nil {
				t.Error("older completion replaced the extended conversation's identity")
			}
			for _, resident := range p.homes {
				if resident.inflight != 0 {
					t.Errorf("finished conversation still has %d requests", resident.inflight)
				}
			}
			now = now.Add(time.Hour)
			// Give a the cold-LRU tie break so this checks room, not the
			// independent preference for b as an engine never used yet.
			p.coldOrder = []string{"a", "b"}
			cold := p.Order(placementFleet(), "m", placementBody(strings.Repeat("z", 4096)), "")
			if cold.Candidates[0].Name != "a" {
				t.Error("expired conversation still reserves a's slot")
			}
		})
	}
}

func TestAttemptReleasesOnlyItsOwnLoad(t *testing.T) {
	p := NewPlacement(nil)
	first := p.Placed(p.Order(placementFleet(), "m", placementBody(strings.Repeat("a", 8192)), ""), "a")
	second := p.Placed(p.Order(placementFleet(), "m", placementBody(strings.Repeat("b", 8192)), ""), "a")
	want := second.reading
	first.Prefilled()
	first.Prefilled()
	first.Finished(2000, 20)
	first.Failed()
	if n := p.Reading()["a"]; n != want {
		t.Fatalf("other request has %d tokens charged, want %d", n, want)
	}
	second.Failed()
	if n := p.Reading()["a"]; n != 0 {
		t.Fatalf("all requests ended but load=%d", n)
	}
}

func TestFailedAttemptPreservesOtherCacheEvidence(t *testing.T) {
	for _, tc := range []struct {
		name                  string
		confirmed, concurrent bool
	}{
		{"rejecting the only attempt leaves no cache", false, false},
		{"prior successful cache survives rejection", true, false},
		{"another pending request survives rejection", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := NewPlacement(nil)
			body := placementBody(strings.Repeat("a", 8192))
			place := func() *Attempt { return p.Placed(p.Order(placementFleet(), "m", body, ""), "a") }
			if tc.confirmed {
				place().Finished(2000, 20)
			}
			failed := place()
			var other *Attempt
			if tc.concurrent {
				other = place()
			}
			failed.Failed()
			got := p.Order(placementFleet(), "m", body, "")
			if (got.shares["a"] > 0) != (tc.confirmed || tc.concurrent) {
				t.Errorf("unexpected cache evidence: %s", got.Explain())
			}
			if !tc.confirmed && !tc.concurrent && len(p.homes) != 0 {
				t.Error("failed conversation still reserves room")
			}
			other.Failed()
			if !tc.confirmed && len(p.homes) != 0 {
				t.Error("failed overlapping attempts still reserve room")
			}
		})
	}
}

func TestForgottenAttemptCannotRestoreReloadedCache(t *testing.T) {
	for _, streamed := range []bool{false, true} {
		name := "reload during prefill"
		if streamed {
			name = "reload during decoding"
		}
		t.Run(name, func(t *testing.T) {
			p := NewPlacement(nil)
			body := placementBody(strings.Repeat("a", 8192))
			old := p.Placed(p.Order(placementFleet(), "m", body, ""), "a")
			if streamed {
				old.Prefilled()
			}
			p.Forget("a")
			fresh := p.Placed(p.Order(placementFleet(), "m", placementBody(strings.Repeat("b", 8192)), ""), "a")
			want := fresh.reading
			old.Prefilled()
			old.Finished(2000, 20)
			if n := p.Reading()["a"]; n != want {
				t.Errorf("old completion changed new engine's load: %d, want %d", n, want)
			}
			if got := p.Order(placementFleet(), "m", body, ""); got.shares["a"] != 0 {
				t.Errorf("old completion restored cache: %s", got.Explain())
			}
			fresh.Failed()
		})
	}
}

func TestFailedMigrationCannotRestoreForgottenHome(t *testing.T) {
	p := NewPlacement(nil)
	body := placementBody(strings.Repeat("a", 8192))
	first := p.Placed(p.Order(placementFleet(), "m", body, ""), "a")
	first.Finished(2000, 20)
	moved := p.Placed(p.Order(placementFleet(), "m", body, ""), "b")
	p.Forget("a")
	moved.Failed()
	if len(p.homes) != 0 {
		t.Fatalf("failed migration restored a forgotten home: %v", p.homes)
	}
}

func TestRouterRollsBackRejectedAffinity(t *testing.T) {
	for _, homeSlot := range []bool{false, true} {
		name := "scheduler"
		if homeSlot {
			name = "home slot"
		}
		t.Run(name, func(t *testing.T) {
			var rejected atomic.Int32
			bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				rejected.Add(1)
				http.Error(w, "busy", http.StatusServiceUnavailable)
			}))
			defer bad.Close()
			good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, `{"ok":true}`) }))
			defer good.Close()
			r := routerWith(t, bad, good)
			r.EnablePrefixAffinity()
			r.aff.HomeSlot = homeSlot
			body := placementBody(strings.Repeat("a", 8192))
			for range 2 {
				w := httptest.NewRecorder()
				r.Forward(w, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(string(body))), "/v1/chat/completions")
				if w.Code != http.StatusOK {
					t.Fatalf("request failed: %d", w.Code)
				}
			}
			if rejected.Load() != 1 {
				t.Errorf("rejected engine called %d times, want once", rejected.Load())
			}
			if held := r.aff.aff.heldBy(prefixBlocks("m", body)); held["a"] != 0 {
				t.Errorf("rejected engine still holds prefix: %v", held)
			}
		})
	}
}

func TestRouterReleasesPrefillWhileStreamRemainsOpen(t *testing.T) {
	release := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"first\"}}]}\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-req.Context().Done():
		}
	}))
	defer up.Close()
	r := routerWith(t, up)
	r.EnablePrefixAffinity()
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) { r.Forward(w, req, "/v1/chat/completions") }))
	defer front.Close()
	defer close(release)
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Post(front.URL, "application/json", strings.NewReader(string(placementBody(strings.Repeat("a", 128<<10)))))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	line, err := bufio.NewReader(resp.Body).ReadString('\n')
	if err != nil || !strings.Contains(line, "first") {
		t.Fatalf("first generated chunk: %q, %v", line, err)
	}
	if n := r.aff.Reading()["a"]; n != 0 {
		t.Errorf("prefill finished but %d tokens remain charged", n)
	}
	if n := r.m.Candidates("m", false)[0].Load(); n != 1 {
		t.Errorf("stream still open but request load is %d, want 1", n)
	}
}
