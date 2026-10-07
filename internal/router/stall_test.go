package router

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gregbugaj/modelfabric/internal/mesh"
)

// Regression: client backpressure can stall forwarding indefinitely.
// The inactivity timeout must cancel the request and release its engine slot.
func TestStalledRequestIsAbandonedAndTheSlotReleased(t *testing.T) {
	silent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n")
		w.(http.Flusher).Flush()
		<-req.Context().Done() // released when the router gives up
	}))
	defer silent.Close()

	r := routerWith(t, silent)
	r.Stall = 150 * time.Millisecond

	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- post(r) }()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the router is still holding a request that stopped making progress")
	}

	// The engine must be free afterwards. A counter that is not decremented is
	// how a node comes to advertise itself as permanently busy.
	for _, c := range r.m.Candidates("m", false) {
		if c.Inflight != 0 {
			t.Errorf("%s still reports %d in flight after the request was abandoned",
				c.Name, c.Inflight)
		}
	}
}

// Silence is normal while a prompt is prefilled -- minutes of it, on the Apple
// Silicon node in this fleet. The bound is on progress, so a stream that keeps
// producing must survive far past the window, however slowly.
func TestSlowButProgressingStreamIsNotAbandoned(t *testing.T) {
	const chunks = 12
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		for i := 0; i < chunks; i++ {
			fmt.Fprintf(w, "data: {\"i\":%d}\n\n", i)
			w.(http.Flusher).Flush()
			time.Sleep(40 * time.Millisecond) // each gap is most of the window
		}
	}))
	defer slow.Close()

	r := routerWith(t, slow)
	r.Stall = 100 * time.Millisecond // total run is ~5x this

	rec := post(r)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, want 200", rec.Code)
	}
	if got := strings.Count(rec.Body.String(), "data:"); got != chunks {
		t.Errorf("got %d chunks, want %d: a slow stream was cut short, which is worse "+
			"than the deadlock this guards against", got, chunks)
	}
}

// Negative disables it, for anyone who would rather hold a slot than lose a
// request. Zero is the default, not "off" -- a zero-value Router must be bounded.
func TestStallWindow(t *testing.T) {
	var r Router
	if got := r.stall(); got != DefaultStall {
		t.Errorf("a zero-value router has a %v window, want the default %v: "+
			"unbounded is how this went unnoticed for ninety minutes", got, DefaultStall)
	}
	r.Stall = -1
	if got := r.stall(); got != 0 {
		t.Errorf("negative should disable the bound, got %v", got)
	}
	r.Stall = time.Minute
	if got := r.stall(); got != time.Minute {
		t.Errorf("explicit window not honoured: %v", got)
	}
}

// Non-streaming responses may delay headers for the entire generation.
// Inference must not inherit the probe client's response-header timeout.
// The test reduces that timeout so a 200 ms response exceeds it.
func TestSlowHeadersAreNotAStall(t *testing.T) {
	old := mesh.ProbeHeaderTimeout
	mesh.ProbeHeaderTimeout = 50 * time.Millisecond
	defer func() { mesh.ProbeHeaderTimeout = old }()

	var calls atomic.Int32
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		time.Sleep(200 * time.Millisecond) // "generating", with nothing sent yet
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer slow.Close()
	// A second engine, so a wrongful timeout shows up as the retry it caused.
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		time.Sleep(200 * time.Millisecond)
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer other.Close()

	rec := post(routerWith(t, slow, other))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "ok") {
		t.Fatalf("a slow answer was treated as a dead engine: %d %q", rec.Code, rec.Body.String())
	}
	// One engine did the work once. Before the fix both were made to start it
	// and both were abandoned.
	if n := calls.Load(); n != 1 {
		t.Errorf("the request reached %d engines, want 1", n)
	}
}
