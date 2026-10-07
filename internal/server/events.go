package server

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"time"
)

// The dashboard feed samples GET handlers in process and publishes changed
// resources over SSE. Subscribers share one sampler, active only while needed.
// Event data matches the corresponding GET body; non-200 responses become null.
// SSE supports this one-way feed without a WebSocket dependency.

// feedEvery is how often the feed samples while someone is subscribed. It is
// also the most an action waits before the dashboard sees its effect.
const feedEvery = time.Second

type feedSource struct {
	name string
	h    http.HandlerFunc
	path string
}

func (s *Server) feedSources() []feedSource {
	return []feedSource{
		{"mesh", s.handleMesh, "/z/mesh"},
		{"models", s.handleListPrepared, "/api/v1/models"},
		{"operations", s.handleOperations, "/api/v1/operations"},
		{"runtimes", s.handleRuntimes, "/api/v1/runtimes"},
		{"llmd", s.handleLLMDStatus, "/api/v1/llmd"},
		{"front", s.handleFront, "/api/v1/front"},
	}
}

type stateFeed struct {
	sources []feedSource
	every   time.Duration

	mu      sync.Mutex
	subs    map[*feedSub]struct{}
	last    map[string][]byte // what was last sent, by source name
	running bool
}

// feedSub holds the newest unsent answer per resource rather than a queue: a
// slow reader skips states it never saw instead of falling behind, and never
// misses the one it ends on.
type feedSub struct {
	only    map[string]bool // nil = every resource
	pending map[string][]byte
	wake    chan struct{}
}

func newStateFeed(sources []feedSource, every time.Duration) *stateFeed {
	return &stateFeed{sources: sources, every: every, subs: map[*feedSub]struct{}{}, last: map[string][]byte{}}
}

func (f *stateFeed) known(name string) bool {
	return slices.ContainsFunc(f.sources, func(s feedSource) bool { return s.name == name })
}

// subscribe registers a reader for the named resources (all of them when only
// is empty). What the feed already holds is queued at once, so a new tab does
// not wait a sample for its first paint.
func (f *stateFeed) subscribe(only []string) (*feedSub, func()) {
	sub := &feedSub{pending: map[string][]byte{}, wake: make(chan struct{}, 1)}
	if len(only) > 0 {
		sub.only = map[string]bool{}
		for _, n := range only {
			sub.only[n] = true
		}
	}
	f.mu.Lock()
	f.subs[sub] = struct{}{}
	for name, b := range f.last {
		if sub.wants(name) {
			sub.pending[name] = b
		}
	}
	if len(sub.pending) > 0 {
		sub.wake <- struct{}{}
	}
	if !f.running {
		f.running = true
		go f.run()
	}
	f.mu.Unlock()
	return sub, func() {
		f.mu.Lock()
		delete(f.subs, sub)
		f.mu.Unlock()
	}
}

func (s *feedSub) wants(name string) bool { return s.only == nil || s.only[name] }

// take returns what is waiting, in the feed's source order so a reader sees
// the same order every time.
func (s *feedSub) take(f *stateFeed) []feedEvent {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []feedEvent
	for _, src := range f.sources {
		if b, ok := s.pending[src.name]; ok {
			out = append(out, feedEvent{src.name, b})
			delete(s.pending, src.name)
		}
	}
	return out
}

type feedEvent struct {
	name string
	data []byte
}

func (f *stateFeed) run() {
	t := time.NewTicker(f.every)
	defer t.Stop()
	for {
		if !f.sample() {
			return
		}
		<-t.C
	}
}

// sample renders every resource some subscriber wants and hands out the ones
// that changed. It reports false, and stops the feed, once nobody is left.
func (f *stateFeed) sample() bool {
	f.mu.Lock()
	if len(f.subs) == 0 {
		f.running = false
		// Forgotten, so the next first subscriber is not handed a state
		// from whenever the last one left.
		clear(f.last)
		f.mu.Unlock()
		return false
	}
	want := map[string]bool{}
	for sub := range f.subs {
		for _, src := range f.sources {
			if sub.wants(src.name) {
				want[src.name] = true
			}
		}
	}
	f.mu.Unlock()

	// Rendered outside the lock: a handler can be slow, and subscribing must
	// not wait on it.
	fresh := map[string][]byte{}
	for _, src := range f.sources {
		if want[src.name] {
			fresh[src.name] = renderSource(src)
		}
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	for name, b := range fresh {
		if prev, ok := f.last[name]; ok && bytes.Equal(prev, b) {
			continue
		}
		f.last[name] = b
		for sub := range f.subs {
			if !sub.wants(name) {
				continue
			}
			sub.pending[name] = b
			select {
			case sub.wake <- struct{}{}:
			default: // already woken; it will take this too
			}
		}
	}
	return true
}

func renderSource(src feedSource) []byte {
	rec := httptest.NewRecorder()
	src.h(rec, httptest.NewRequest(http.MethodGet, src.path, nil))
	if rec.Code != http.StatusOK {
		return []byte("null")
	}
	// Compact JSON has no raw newlines, which is what lets one `data:` line
	// carry it; the encoder's trailing one is all there is to trim.
	return bytes.TrimSpace(rec.Body.Bytes())
}

// handleEvents serves the feed as server-sent events: `event: <resource>`
// with that resource's GET body as data. ?only=models,operations narrows it,
// which is what a dashboard asks of a peer.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	var only []string
	if v := r.URL.Query().Get("only"); v != "" {
		for _, n := range strings.Split(v, ",") {
			if !s.feed.known(n) {
				writeError(w, http.StatusBadRequest, fmt.Sprintf("unknown resource %q; use mesh, models, operations, runtimes, llmd or front", n))
				return
			}
			only = append(only, n)
		}
	}
	rc := http.NewResponseController(w)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	// Flushed now, as in handleTrafficStream: otherwise EventSource never
	// fires `open` until the first sample lands.
	_ = rc.Flush()

	sub, cancel := s.feed.subscribe(only)
	defer cancel()

	keepalive := time.NewTicker(15 * time.Second)
	defer keepalive.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-sub.wake:
			for _, e := range sub.take(s.feed) {
				if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", e.name, e.data); err != nil {
					return
				}
			}
			if rc.Flush() != nil {
				return
			}
		case <-keepalive.C:
			if _, err := fmt.Fprint(w, ": keepalive\n\n"); err != nil || rc.Flush() != nil {
				return
			}
		}
	}
}
