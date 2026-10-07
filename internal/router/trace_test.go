package router

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const (
	callerTrace = "4bf92f3577b34da6a3ce929d0e0e4736"
	callerSpan  = "00f067aa0ba902b7"
	peerTrace   = "0af7651916cd43dd8448eb211c80319c"
)

func reqWith(h map[string]string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	for k, v := range h {
		r.Header.Set(k, v)
	}
	return r
}

func TestTraceOfAdoptsCallerTraceParent(t *testing.T) {
	tc := TraceOf(reqWith(map[string]string{
		TraceParentHeader: "00-" + callerTrace + "-" + callerSpan + "-01",
	}))
	if tc.TraceID != callerTrace {
		t.Errorf("trace id %q, want the caller's %q", tc.TraceID, callerTrace)
	}
	if tc.ParentID != callerSpan {
		t.Errorf("parent %q, want the caller's span %q", tc.ParentID, callerSpan)
	}
	if !validSpan(tc.SpanID) || tc.SpanID == callerSpan {
		t.Errorf("span %q must be freshly minted, not the caller's", tc.SpanID)
	}
}

// Unlike traceparent, ModelFabric's own header is honoured only from a peer hop.
func TestTraceOfInternalHeaderNeedsAHop(t *testing.T) {
	fromPeer := TraceOf(reqWith(map[string]string{HopHeader: "1", TraceHeader: peerTrace}))
	if fromPeer.TraceID != peerTrace {
		t.Errorf("a peer hop should keep the trace: got %q", fromPeer.TraceID)
	}
	fromClient := TraceOf(reqWith(map[string]string{TraceHeader: peerTrace}))
	if fromClient.TraceID == peerTrace {
		t.Error("a client must not be able to set ModelFabric's own trace header")
	}
	if !validTrace(fromClient.TraceID) {
		t.Errorf("minted trace %q is not W3C-shaped", fromClient.TraceID)
	}
}

// traceparent wins over the internal header: it is the wider context, and the
// two agree in practice because Forward writes both.
func TestTraceOfPrefersTraceParentOverInternal(t *testing.T) {
	tc := TraceOf(reqWith(map[string]string{
		HopHeader:         "1",
		TraceHeader:       peerTrace,
		TraceParentHeader: "00-" + callerTrace + "-" + callerSpan + "-01",
	}))
	if tc.TraceID != callerTrace {
		t.Errorf("trace id %q, want the traceparent's %q", tc.TraceID, callerTrace)
	}
}

func TestTraceOfRejectsBadTraceParent(t *testing.T) {
	for _, bad := range []string{
		"",
		"garbage",
		"00-" + callerTrace, // too few fields
		"00-00000000000000000000000000000000-" + callerSpan + "-01",
		"00-" + callerTrace + "-0000000000000000-01",
		"00-nothex77b34da6a3ce929d0e0e4736-" + callerSpan + "-01",
		"ff-" + callerTrace + "-" + callerSpan + "-01", // version ff is forbidden
		"00-" + callerTrace[:16] + "-" + callerSpan + "-01",
	} {
		t.Run(bad, func(t *testing.T) {
			tc := TraceOf(reqWith(map[string]string{TraceParentHeader: bad}))
			if tc.TraceID == callerTrace || !validTrace(tc.TraceID) {
				t.Errorf("got %q from %q; want a freshly minted valid trace", tc.TraceID, bad)
			}
			if tc.ParentID != "" {
				t.Errorf("parent %q, want none — nothing valid called us", tc.ParentID)
			}
		})
	}
}

// A version ModelFabric does not know still has its first three fields in the
// required shape, so the trace survives a spec bump.
func TestTraceOfAcceptsFutureVersions(t *testing.T) {
	tc := TraceOf(reqWith(map[string]string{
		TraceParentHeader: "01-" + callerTrace + "-" + callerSpan + "-01-somethingnew",
	}))
	if tc.TraceID != callerTrace {
		t.Errorf("trace id %q, want %q — a newer version must not drop the trace", tc.TraceID, callerTrace)
	}
}

func TestHeaderNamesThisNodesSpan(t *testing.T) {
	tc := TraceOf(reqWith(map[string]string{
		TraceParentHeader: "00-" + callerTrace + "-" + callerSpan + "-01",
	}))
	h := tc.Header()
	parts := strings.Split(h, "-")
	if len(parts) != 4 {
		t.Fatalf("header %q is not four fields", h)
	}
	if parts[0] != "00" || parts[1] != callerTrace || parts[2] != tc.SpanID || parts[3] != "01" {
		t.Errorf("header %q: want version 00, the caller's trace, our span, sampled", h)
	}
	if parts[2] == callerSpan {
		t.Error("the next hop's parent must be us, not whoever called us")
	}
}

func TestMintedTracesAreDistinct(t *testing.T) {
	seen := map[string]bool{}
	for range 1000 {
		id := newTraceID()
		if !validTrace(id) {
			t.Fatalf("minted %q, not a valid trace id", id)
		}
		if seen[id] {
			t.Fatalf("minted %q twice", id)
		}
		seen[id] = true
	}
}
