package router

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"strings"
)

// X-Fabric-Trace is accepted only on peer hops to prevent clients from merging
// unrelated Activity records. W3C traceparent is accepted from callers for
// distributed tracing; it is not an authentication credential.
// See https://www.w3.org/TR/trace-context/.
const (
	// TraceParentHeader carries the W3C trace id and the caller's span.
	TraceParentHeader = "traceparent"
	// TraceStateHeader is vendor-specific trace data. ModelFabric adds nothing to
	// it and forwards it untouched, as the spec requires.
	TraceStateHeader = "tracestate"
)

type TraceContext struct {
	TraceID  string // 32 hex
	ParentID string // 16 hex, empty when this node starts the trace
	SpanID   string // 16 hex, minted here
}

// TraceOf resolves the trace context for a request.
//
// Precedence: a W3C traceparent from anyone, then ModelFabric's own header on a hop
// from a peer, then a fresh trace. The middle case keeps an internal hop
// correlated even for a caller that sends no traceparent at all.
func TraceOf(req *http.Request) TraceContext {
	tc := TraceContext{SpanID: newSpanID()}
	if id, parent, ok := parseTraceParent(req.Header.Get(TraceParentHeader)); ok {
		tc.TraceID, tc.ParentID = id, parent
		return tc
	}
	if req.Header.Get(HopHeader) != "" {
		if t := req.Header.Get(TraceHeader); validTrace(t) {
			tc.TraceID = t
			return tc
		}
	}
	tc.TraceID = newTraceID()
	return tc
}

// Header names this node's span as the next hop's parent. The sampled flag
// is set because ModelFabric records every routed request.
func (tc TraceContext) Header() string {
	return "00-" + tc.TraceID + "-" + tc.SpanID + "-01"
}

// parseTraceParent extracts the trace and parent span IDs. Future versions
// retain these fields under the W3C compatibility rules.
func parseTraceParent(v string) (traceID, parentID string, ok bool) {
	parts := strings.Split(strings.TrimSpace(v), "-")
	if len(parts) < 4 {
		return "", "", false
	}
	version, id, span := parts[0], parts[1], parts[2]
	if len(version) != 2 || !isHex(version) || version == "ff" {
		return "", "", false
	}
	if !validTrace(id) || !validSpan(span) {
		return "", "", false
	}
	return id, span, true
}

// Trace IDs require 16 random bytes for W3C interoperability.
func newTraceID() string { return randomHex(16) }

// A span id is 8 bytes, per the spec.
func newSpanID() string { return randomHex(8) }

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		// On entropy failure, use a recognizable fallback for hop correlation;
		// it cannot guarantee uniqueness across the mesh.
		for i := range b {
			b[i] = 0xAA
		}
	}
	return hex.EncodeToString(b)
}

// validTrace accepts a 32-hex trace id that is not all zeros, which the spec
// reserves as "no trace". It also keeps a peer from writing anything else into
// a field shown in a dashboard and written to a log file.
func validTrace(t string) bool { return len(t) == 32 && isHex(t) && !allZero(t) }

func validSpan(s string) bool { return len(s) == 16 && isHex(s) && !allZero(s) }

func isHex(s string) bool {
	_, err := hex.DecodeString(s)
	return err == nil
}

func allZero(s string) bool { return strings.Trim(s, "0") == "" }
