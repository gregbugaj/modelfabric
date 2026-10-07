package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/gregbugaj/modelfabric/internal/router"
)

// Traffic events feed the CLI log and dashboard Activity page. Routing
// metadata is always recorded; body capture is opt-in and held in a bounded
// memory ring. Disk logging requires separate configuration.

const (
	trafficBacklogDefault = 200
	trafficBacklogMax     = 2000
)

type traffic struct {
	mu   sync.Mutex
	ring []router.Event
	subs map[chan router.Event]struct{}
	keep int
	// bodies is the live capture switch. The router and llm-d proxy read it
	// through settings(), so changes take effect on the next request.
	bodies bool
	max    int
	sink   *bodySink // file logging, nil unless configured
	// metrics counts every event that arrives here. The ring forgets and the
	// subscribers come and go; the counters are the part that must not.
	metrics *metrics
}

func newTraffic(m *metrics) *traffic {
	return &traffic{subs: map[chan router.Event]struct{}{}, keep: trafficBacklogDefault, max: router.DefaultBodyCap, metrics: m}
}

// Capture sets the node's starting capture state from config, and opens the
// file sink if one is configured.
func (s *Server) Capture(bodies bool, maxBytes, keep int, file string) error {
	s.traffic.mu.Lock()
	s.traffic.bodies = bodies
	if maxBytes > 0 {
		s.traffic.max = maxBytes
	}
	if keep > 0 {
		if keep > trafficBacklogMax {
			keep = trafficBacklogMax
		}
		s.traffic.keep = keep
	}
	s.traffic.mu.Unlock()
	if file == "" {
		return nil
	}
	sink, err := newBodySink(file)
	if err != nil {
		return err
	}
	s.traffic.mu.Lock()
	s.traffic.sink = sink
	s.traffic.mu.Unlock()
	return nil
}

func (t *traffic) settings() (bodies bool, keep, max int, toFile bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.bodies, t.keep, t.max, t.sink != nil
}

// setSettings updates capture at runtime. Shrinking keep drops the oldest
// events immediately, so turning the ring down is also how you clear it.
func (t *traffic) setSettings(bodies bool, keep int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.bodies = bodies
	if keep > 0 {
		if keep > trafficBacklogMax {
			keep = trafficBacklogMax
		}
		t.keep = keep
	}
	if !bodies {
		// Turning capture off forgets what was already captured, rather than
		// leaving prompts sitting in memory until the ring rolls over.
		for i := range t.ring {
			t.ring[i].ReqBody, t.ring[i].RespBody, t.ring[i].Truncated = "", "", false
		}
	}
	if len(t.ring) > t.keep {
		t.ring = t.ring[len(t.ring)-t.keep:]
	}
}

func (t *traffic) publish(e router.Event) {
	// Counted before the ring is touched, and outside its lock: a scrape must
	// not be able to hold up routing, and a body that is about to be stripped
	// makes no difference to a counter.
	if t.metrics != nil {
		t.metrics.record(e)
	}

	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.bodies {
		e.ReqBody, e.RespBody, e.Truncated = "", "", false
	} else if t.sink != nil {
		t.sink.write(e)
	}
	t.ring = append(t.ring, e)
	keep := t.keep
	if keep <= 0 {
		keep = trafficBacklogDefault
	}
	if len(t.ring) > keep {
		t.ring = t.ring[len(t.ring)-keep:]
	}
	for ch := range t.subs {
		// A slow reader drops events rather than stalling request routing.
		select {
		case ch <- e:
		default:
		}
	}
}

// recent returns at most limit events, newest first. Bodies are included only
// when requested, avoiding prompt transfer for metadata-only mesh views.
func (t *traffic) recent(limit int, bodies bool) []router.Event {
	t.mu.Lock()
	defer t.mu.Unlock()
	if limit <= 0 || limit > len(t.ring) {
		limit = len(t.ring)
	}
	out := make([]router.Event, 0, limit)
	for i := len(t.ring) - 1; i >= len(t.ring)-limit; i-- {
		e := t.ring[i]
		if !bodies {
			e.ReqBody, e.RespBody, e.Truncated = "", "", false
		}
		out = append(out, e)
	}
	return out
}

func (t *traffic) subscribe() (chan router.Event, []router.Event, func()) {
	ch := make(chan router.Event, 64)
	t.mu.Lock()
	t.subs[ch] = struct{}{}
	backlog := append([]router.Event(nil), t.ring...)
	t.mu.Unlock()
	return ch, backlog, func() {
		t.mu.Lock()
		delete(t.subs, ch)
		t.mu.Unlock()
	}
}

// handleTrafficStream serves routing events as server-sent events. ?backlog=1
// replays recent history first.
func (s *Server) handleTrafficStream(w http.ResponseWriter, r *http.Request) {
	rc := http.NewResponseController(w)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	// Flush immediately so idle clients receive headers before the first event
	// or keepalive, and EventSource can fire open.
	_ = rc.Flush()

	ch, backlog, cancel := s.traffic.subscribe()
	defer cancel()

	send := func(e router.Event) bool {
		b, _ := json.Marshal(e)
		if _, err := fmt.Fprintf(w, "data: %s\n\n", b); err != nil {
			return false
		}
		return rc.Flush() == nil
	}
	if r.URL.Query().Get("backlog") == "1" {
		for _, e := range backlog {
			if !send(e) {
				return
			}
		}
	}
	// A comment line keeps idle connections from being reaped by proxies.
	keepalive := time.NewTicker(15 * time.Second)
	defer keepalive.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case e := <-ch:
			if !send(e) {
				return
			}
		case <-keepalive.C:
			if _, err := fmt.Fprint(w, ": keepalive\n\n"); err != nil || rc.Flush() != nil {
				return
			}
		}
	}
}

// trafficRecent is one node's answer when another node's dashboard asks what
// it has served. Node names the machine that took the requests in - the front
// door they came through - which is not the same as Event.Node, the machine
// that ran the model.
type trafficRecent struct {
	Node    string         `json:"node"`
	Capture bool           `json:"capture"`
	Events  []router.Event `json:"events"`
}

// handleTrafficRecent serves this node's ring as a snapshot, so a dashboard on
// another node can merge it with its own. Over the tailnet this is reachable
// only from a device Tailscale reports as the same owner, like every other
// /api/v1 route (see peer.go).
func (s *Server) handleTrafficRecent(w http.ResponseWriter, r *http.Request) {
	limit := trafficBacklogDefault
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			writeError(w, http.StatusBadRequest, "limit must be a positive number")
			return
		}
		limit = min(n, trafficBacklogMax)
	}
	// Capture-off clears stored bodies, so snapshots cannot return them.
	bodies := r.URL.Query().Get("bodies") == "1"
	on, _, _, _ := s.traffic.settings()
	writeJSON(w, http.StatusOK, trafficRecent{
		Node: s.m.State().Node, Capture: on, Events: s.traffic.recent(limit, bodies),
	})
}

type trafficSettings struct {
	Bodies   bool `json:"bodies"`
	Keep     int  `json:"keep"`
	MaxBytes int  `json:"max_bytes"`
	ToFile   bool `json:"to_file"`
	// KeepMax bounds what the UI may ask for, so the control can offer the
	// real range instead of guessing.
	KeepMax int `json:"keep_max"`
}

// handleTrafficSettings manages live capture. Peer access requires the same
// Tailscale owner; mesh_admin=off restricts access to loopback.
func (s *Server) handleTrafficSettings(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		var in struct {
			Bodies *bool `json:"bodies"`
			Keep   *int  `json:"keep"`
		}
		if err := decodeBody(w, r, &in, 1<<16); err != nil {
			writeError(w, http.StatusBadRequest, "bad settings: "+err.Error())
			return
		}
		on, keep, _, _ := s.traffic.settings()
		if in.Bodies != nil {
			on = *in.Bodies
		}
		if in.Keep != nil {
			keep = *in.Keep
		}
		s.traffic.setSettings(on, keep)
		if in.Bodies != nil {
			s.log.Warn("traffic body capture changed", "bodies", on, "keep", keep)
		}
	}
	on, keep, max, toFile := s.traffic.settings()
	writeJSON(w, http.StatusOK, trafficSettings{
		Bodies: on, Keep: keep, MaxBytes: max, ToFile: toFile, KeepMax: trafficBacklogMax,
	})
}
