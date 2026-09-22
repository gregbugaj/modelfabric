package server

import (
	"bufio"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gregbugaj/modelfabric/internal/config"
	"github.com/gregbugaj/modelfabric/internal/mesh"
	"github.com/gregbugaj/modelfabric/internal/router"
)

// fixed answers the same body every time; counter answers a new one each call.
func fixed(body string, code int) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(code)
		_, _ = io.WriteString(w, body+"\n")
	}
}

func counter(n *atomic.Int64) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, strings.Repeat("x", int(n.Add(1)))+"\n")
	}
}

// next waits for the subscriber's next non-empty batch, or fails. A wake can
// find nothing: a sample may signal after the reader already took what that
// sample queued, and the handler simply writes nothing for it.
func next(t *testing.T, f *stateFeed, sub *feedSub) map[string]string {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case <-sub.wake:
		case <-deadline:
			t.Fatal("no event within 2s")
		}
		out := map[string]string{}
		for _, e := range sub.take(f) {
			out[e.name] = string(e.data)
		}
		if len(out) > 0 {
			return out
		}
	}
}

// quiet asserts nothing arrives across several samples.
func quiet(t *testing.T, sub *feedSub, f *stateFeed) {
	t.Helper()
	deadline := time.After(10 * f.every)
	for {
		select {
		case <-sub.wake:
			if got := sub.take(f); len(got) > 0 {
				t.Fatalf("unexpected event: %v", got)
			}
		case <-deadline:
			return
		}
	}
}

func TestFeedSendsOnlyWhatChanged(t *testing.T) {
	var ticks atomic.Int64
	f := newStateFeed([]feedSource{
		{"steady", fixed(`{"a":1}`, 200), "/steady"},
		{"moving", counter(&ticks), "/moving"},
		{"absent", fixed(`{"error":"no"}`, 404), "/absent"},
	}, 5*time.Millisecond)

	tests := []struct {
		name  string
		only  []string
		first map[string]string // "*" = any non-empty value
		// second is the next batch, or nil for "nothing more arrives".
		second map[string]string
	}{
		{
			name:   "first batch carries every resource, a non-200 as null, then only the one that moved",
			first:  map[string]string{"steady": `{"a":1}`, "moving": "*", "absent": "null"},
			second: map[string]string{"moving": "*"},
		},
		{
			name:  "only narrows the stream to the named resources",
			only:  []string{"steady"},
			first: map[string]string{"steady": `{"a":1}`},
		},
	}
	match := func(t *testing.T, which string, got, want map[string]string) {
		t.Helper()
		if len(got) != len(want) {
			t.Fatalf("%s batch = %v, want %v", which, got, want)
		}
		for k, v := range want {
			if g, ok := got[k]; !ok || (v == "*" && g == "") || (v != "*" && g != v) {
				t.Fatalf("%s batch = %v, want %v", which, got, want)
			}
		}
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sub, cancel := f.subscribe(tc.only)
			defer cancel()
			match(t, "first", next(t, f, sub), tc.first)
			if tc.second == nil {
				quiet(t, sub, f)
				return
			}
			match(t, "second", next(t, f, sub), tc.second)
		})
	}
}

// A tab that joins late gets the current state at once rather than after the
// next change, which for a steady resource might never come.
func TestFeedLateSubscriberGetsCurrentState(t *testing.T) {
	f := newStateFeed([]feedSource{{"steady", fixed(`{"a":1}`, 200), "/steady"}}, 5*time.Millisecond)
	a, cancelA := f.subscribe(nil)
	defer cancelA()
	next(t, f, a)
	b, cancelB := f.subscribe(nil)
	defer cancelB()
	if got := next(t, f, b); got["steady"] != `{"a":1}` {
		t.Fatalf("late subscriber got %v", got)
	}
}

// With no dashboard open the sampler must not keep rendering handlers.
func TestFeedStopsWithItsLastSubscriber(t *testing.T) {
	var calls atomic.Int64
	f := newStateFeed([]feedSource{{"moving", counter(&calls), "/moving"}}, 5*time.Millisecond)
	sub, cancel := f.subscribe(nil)
	next(t, f, sub)
	cancel()
	time.Sleep(20 * f.every)
	before := calls.Load()
	time.Sleep(20 * f.every)
	if after := calls.Load(); after != before {
		t.Fatalf("sampler still running with no subscribers: %d -> %d calls", before, after)
	}
	f.mu.Lock()
	running := f.running
	f.mu.Unlock()
	if running {
		t.Fatal("feed reports running with no subscribers")
	}
}

func eventsServer(t *testing.T) *httptest.Server {
	t.Helper()
	m := mesh.New(config.Default(), "minion")
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	ts := httptest.NewServer(New(m, router.New(m, log), nil, log, nil).Handler())
	t.Cleanup(ts.Close)
	return ts
}

func TestEventsEndpoint(t *testing.T) {
	ts := eventsServer(t)
	tests := []struct {
		name      string
		query     string
		wantCode  int
		wantEvent string // the first event name, when the stream opens
	}{
		{name: "unknown resource is refused by name", query: "?only=mesh,bogus", wantCode: http.StatusBadRequest},
		{name: "narrowed stream opens with that resource", query: "?only=mesh", wantCode: http.StatusOK, wantEvent: "mesh"},
		{name: "whole stream opens with the first resource", wantCode: http.StatusOK, wantEvent: "mesh"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := http.Get(ts.URL + "/api/v1/events" + tc.query)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != tc.wantCode {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tc.wantCode)
			}
			if tc.wantEvent == "" {
				return
			}
			line, err := bufio.NewReader(resp.Body).ReadString('\n')
			if err != nil {
				t.Fatal(err)
			}
			if got := strings.TrimSpace(line); got != "event: "+tc.wantEvent {
				t.Fatalf("first line = %q, want event: %s", got, tc.wantEvent)
			}
		})
	}
}
