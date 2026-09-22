package server

import (
	"fmt"
	"io"
	"maps"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/gregbugaj/modelfabric/internal/router"
)

// Prometheus metrics for what this node's router did.
//
// ModelFabric already reads engines' /metrics and republishes them for llm-d; this
// is the other direction — what ModelFabric itself knows and nothing else can see:
// which node and engine served a request, whether it crossed the tailnet, and
// who chose the destination. The engine's own counters stay the engine's.
//
// Written by hand rather than through a client library. The text format is a
// dozen lines of printing, and ModelFabric is a single binary with no dependency it
// did not have to have.
//
// Cardinality is the thing that kills a metrics endpoint, so the labels are
// only ever values from a small fixed set: models and nodes in one mesh, a
// status code, and who routed it. Engine instance ids are deliberately left
// out — they change on every reload, so a long-lived node would grow a new
// time series every time a model restarted.

// durationBuckets are upper bounds in seconds. Chosen around what a local mesh
// actually produces: a cached short completion in tens of milliseconds, a cold
// 27B prefill in tens of seconds.
var durationBuckets = []float64{0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60}

// seriesKey is one labelled series. Every field is low-cardinality by
// construction; see the note above.
type seriesKey struct {
	model  string
	node   string // the node that served it; empty when nobody could say
	status int
	via    string // "" when this node's own router chose
	local  bool
}

type seriesValue struct {
	count   uint64
	bytes   uint64
	seconds float64
	buckets []uint64 // cumulative counts, aligned with durationBuckets
}

type metrics struct {
	mu     sync.Mutex
	series map[seriesKey]*seriesValue
	// errors counts events that carried an error string, which is not always
	// the same as a 4xx or 5xx: a client that hangs up mid-stream leaves a 200
	// with an error.
	errors uint64
}

func newMetrics() *metrics {
	return &metrics{series: map[seriesKey]*seriesValue{}}
}

// record folds one routed request into the counters. Called from
// traffic.publish, which every path (this node's router and the llm-d
// proxy) already goes through.
func (m *metrics) record(e router.Event) {
	k := seriesKey{model: e.Model, node: e.Node, status: e.Status, via: e.Via, local: e.Local}
	secs := float64(e.Millis) / 1000

	m.mu.Lock()
	defer m.mu.Unlock()
	v := m.series[k]
	if v == nil {
		v = &seriesValue{buckets: make([]uint64, len(durationBuckets))}
		m.series[k] = v
	}
	v.count++
	if e.BytesOut > 0 {
		v.bytes += uint64(e.BytesOut)
	}
	v.seconds += secs
	for i, ub := range durationBuckets {
		if secs <= ub {
			v.buckets[i]++
		}
	}
	if e.Error != "" {
		m.errors++
	}
}

// handleMetrics serves the Prometheus text exposition format.
//
// On loopback it is open, like the dashboard. Over the tailnet it is reachable
// only from a device Tailscale reports as the same owner — a scraper on
// another of your own machines — because model names and traffic volumes are
// not something every tailnet member should read. No prompt ever appears here,
// captured or not.
func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	s.metrics.write(w, s.meshGauges())
}

// meshGauges is the state half: what the node is, rather than what it did.
func (s *Server) meshGauges() []gauge {
	if s.m == nil {
		return nil
	}
	self := s.m.State()
	peers := s.m.Peers()

	nodesUp := 1 // this node is up, by definition of answering
	for _, p := range peers {
		if p.Alive {
			nodesUp++
		}
	}
	models := map[string]bool{}
	enginesReady := 0
	for _, i := range self.Instances {
		models[i.Model] = true
		if i.State == "ready" {
			enginesReady++
		}
	}
	for _, p := range peers {
		if !p.Alive {
			continue
		}
		for _, i := range p.Instances {
			models[i.Model] = true
			if i.State == "ready" {
				enginesReady++
			}
		}
	}
	return []gauge{
		{name: "modelfabric_nodes_total", help: "Nodes in the mesh, including this one.", value: float64(len(peers) + 1)},
		{name: "modelfabric_nodes_up", help: "Nodes answering probes, including this one.", value: float64(nodesUp)},
		{name: "modelfabric_engines_ready", help: "Engine instances ready to serve, across the mesh.", value: float64(enginesReady)},
		{name: "modelfabric_models_served", help: "Distinct models with at least one ready engine.", value: float64(len(models))},
		{name: "modelfabric_inflight_requests", help: "Requests this node is currently forwarding.", value: float64(self.Inflight)},
	}
}

type gauge struct {
	name  string
	help  string
	value float64
}

func (m *metrics) write(w io.Writer, gauges []gauge) {
	m.mu.Lock()
	// Copied so the response is rendered without holding the lock that every
	// routed request needs.
	snapshot := make(map[seriesKey]seriesValue, len(m.series))
	for k, v := range m.series {
		cp := *v
		cp.buckets = slices.Clone(v.buckets)
		snapshot[k] = cp
	}
	errs := m.errors
	m.mu.Unlock()

	keys := slices.Collect(maps.Keys(snapshot))
	// Sorted so a diff of two scrapes is readable and the output is stable.
	sort.Slice(keys, func(i, j int) bool {
		a, b := keys[i], keys[j]
		if a.model != b.model {
			return a.model < b.model
		}
		if a.node != b.node {
			return a.node < b.node
		}
		if a.status != b.status {
			return a.status < b.status
		}
		return a.via < b.via
	})

	var b strings.Builder
	for _, g := range gauges {
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s gauge\n%s %s\n", g.name, g.help, g.name, g.name, num(g.value))
	}

	b.WriteString("# HELP modelfabric_requests_total Requests this node routed, by destination and outcome.\n")
	b.WriteString("# TYPE modelfabric_requests_total counter\n")
	for _, k := range keys {
		fmt.Fprintf(&b, "modelfabric_requests_total%s %d\n", labels(k), snapshot[k].count)
	}

	b.WriteString("# HELP modelfabric_response_bytes_total Response bytes this node forwarded.\n")
	b.WriteString("# TYPE modelfabric_response_bytes_total counter\n")
	for _, k := range keys {
		fmt.Fprintf(&b, "modelfabric_response_bytes_total%s %d\n", labels(k), snapshot[k].bytes)
	}

	b.WriteString("# HELP modelfabric_request_duration_seconds End-to-end time this node measured, from request to last byte.\n")
	b.WriteString("# TYPE modelfabric_request_duration_seconds histogram\n")
	for _, k := range keys {
		v := snapshot[k]
		base := labels(k)
		for i, ub := range durationBuckets {
			fmt.Fprintf(&b, "modelfabric_request_duration_seconds_bucket%s %d\n",
				withLabel(base, "le", num(ub)), v.buckets[i])
		}
		fmt.Fprintf(&b, "modelfabric_request_duration_seconds_bucket%s %d\n", withLabel(base, "le", "+Inf"), v.count)
		fmt.Fprintf(&b, "modelfabric_request_duration_seconds_sum%s %s\n", base, num(v.seconds))
		fmt.Fprintf(&b, "modelfabric_request_duration_seconds_count%s %d\n", base, v.count)
	}

	fmt.Fprintf(&b, "# HELP modelfabric_request_errors_total Requests that recorded an error, including ones that answered 200 and then failed mid-stream.\n")
	fmt.Fprintf(&b, "# TYPE modelfabric_request_errors_total counter\nmodelfabric_request_errors_total %d\n", errs)

	_, _ = io.WriteString(w, b.String())
}

// labels renders one series' label set, always in the same order.
func labels(k seriesKey) string {
	node := k.node
	if node == "" {
		node = "unknown"
	}
	via := k.via
	if via == "" {
		via = "router"
	}
	// "%s" with our own escaping rather than %q: Go's quoting also escapes
	// non-printables as \x.., which the exposition format does not define.
	// The three sequences it does define are handled by escape.
	return fmt.Sprintf(`{model="%s",node="%s",status="%d",via="%s",local="%t"}`,
		escape(k.model), escape(node), k.status, escape(via), k.local)
}

// withLabel adds one label to a rendered set, which is how a histogram bucket
// differs from the series it belongs to.
func withLabel(rendered, name, value string) string {
	return rendered[:len(rendered)-1] + fmt.Sprintf(`,%s="%s"}`, name, escape(value))
}

// escape applies the exposition format's rules for a label value: backslash,
// double quote and newline. A model id comes from a filename, so this is not
// hypothetical.
func escape(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)
	return r.Replace(s)
}

// num prints a float the way Prometheus expects: no exponent for ordinary
// magnitudes, and no trailing zeros.
func num(f float64) string {
	return strconv.FormatFloat(f, 'g', -1, 64)
}
