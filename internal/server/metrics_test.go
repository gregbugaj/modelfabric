package server

import (
	"strings"
	"testing"

	"github.com/gregbugaj/modelfabric/internal/router"
)

func render(t *testing.T, m *metrics, gauges []gauge) string {
	t.Helper()
	var b strings.Builder
	m.write(&b, gauges)
	return b.String()
}

func TestMetricsExposition(t *testing.T) {
	m := newMetrics()
	m.record(router.Event{Model: "qwen/qwen3-0.6b", Node: "minion", Status: 200, Millis: 120, BytesOut: 800, Local: false})
	m.record(router.Event{Model: "qwen/qwen3-0.6b", Node: "minion", Status: 200, Millis: 4000, BytesOut: 200, Local: false})
	// No node and no via: the router could not say who served it.
	m.record(router.Event{Model: "qwen/qwen3-0.6b", Status: 404, Millis: 2, Error: "no node serves this model"})
	m.record(router.Event{Model: "qwen/qwen3.8-27b", Status: 200, Millis: 90, Via: "llm-d"})

	out := render(t, m, []gauge{{name: "modelfabric_nodes_up", help: "Nodes answering probes.", value: 3}})

	for _, want := range []string{
		"modelfabric_nodes_up 3",
		`modelfabric_requests_total{model="qwen/qwen3-0.6b",node="minion",status="200",via="router",local="false"} 2`,
		`modelfabric_response_bytes_total{model="qwen/qwen3-0.6b",node="minion",status="200",via="router",local="false"} 1000`,
		`modelfabric_requests_total{model="qwen/qwen3-0.6b",node="unknown",status="404",via="router",local="false"} 1`,
		`modelfabric_requests_total{model="qwen/qwen3.8-27b",node="unknown",status="200",via="llm-d",local="false"} 1`,
		"modelfabric_request_errors_total 1",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing line:\n  %s\ngot:\n%s", want, out)
		}
	}
}

// A scrape is rejected outright if a metric family's HELP or TYPE appears more
// than once, which is easy to do when the families share a loop.
func TestMetricsNoDuplicateFamilyHeaders(t *testing.T) {
	m := newMetrics()
	m.record(router.Event{Model: "a", Node: "n1", Status: 200, Millis: 10})
	m.record(router.Event{Model: "b", Node: "n2", Status: 200, Millis: 10})

	seen := map[string]int{}
	for _, line := range strings.Split(render(t, m, nil), "\n") {
		if strings.HasPrefix(line, "# HELP ") || strings.HasPrefix(line, "# TYPE ") {
			seen[line[:strings.LastIndex(line, " ")]]++
		}
	}
	for k, n := range seen {
		if n > 1 {
			t.Errorf("%q appears %d times; a family may declare HELP and TYPE once", k, n)
		}
	}
}

// Buckets are cumulative: each one counts everything at or below its bound, so
// they must never decrease and must end at the series' own count.
func TestMetricsBucketsAreCumulative(t *testing.T) {
	m := newMetrics()
	for _, ms := range []int64{10, 80, 300, 900, 4000, 45000} {
		m.record(router.Event{Model: "m", Node: "n", Status: 200, Millis: ms})
	}
	out := render(t, m, nil)

	var last uint64
	var seen int
	for _, line := range strings.Split(out, "\n") {
		if !strings.HasPrefix(line, "modelfabric_request_duration_seconds_bucket") {
			continue
		}
		var n uint64
		if _, err := fmtSscan(line, &n); err != nil {
			t.Fatalf("unparseable bucket line %q: %v", line, err)
		}
		if n < last {
			t.Errorf("buckets went backwards at %q: %d after %d", line, n, last)
		}
		last, seen = n, seen+1
	}
	if seen != len(durationBuckets)+1 {
		t.Errorf("got %d bucket lines, want %d (bounds plus +Inf)", seen, len(durationBuckets)+1)
	}
	if last != 6 {
		t.Errorf("+Inf bucket is %d, want 6 — every observation must land in it", last)
	}
}

// A model id comes from a filename, so a quote or a backslash in one must not
// produce a line that cannot be parsed.
func TestMetricsEscapesLabelValues(t *testing.T) {
	m := newMetrics()
	m.record(router.Event{Model: `we"ird\name`, Node: "n", Status: 200, Millis: 5})
	out := render(t, m, nil)
	if !strings.Contains(out, `model="we\"ird\\name"`) {
		t.Errorf("label value not escaped:\n%s", out)
	}
}

func fmtSscan(line string, n *uint64) (int, error) {
	i := strings.LastIndex(line, " ")
	if i < 0 {
		return 0, errNoValue
	}
	var v uint64
	for _, c := range line[i+1:] {
		if c < '0' || c > '9' {
			return 0, errNoValue
		}
		v = v*10 + uint64(c-'0')
	}
	*n = v
	return 1, nil
}

var errNoValue = errStr("no numeric value at end of line")

type errStr string

func (e errStr) Error() string { return string(e) }
