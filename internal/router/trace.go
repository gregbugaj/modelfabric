package router

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"strings"
)

// Trace context.
//
// Two headers do related jobs here, and they are trusted differently.
//
// X-Fabric-Trace is ModelFabric's own, and is honoured only on a hop from another node
// (see HopHeader). It is the id the dashboard groups rows by, so a client that
// could set it could make two unrelated requests look like one.
//
// traceparent is the W3C standard (https://www.w3.org/TR/trace-context/) and
// is accepted from anyone, which is the whole point of it: the caller is
// usually an agent framework that started the trace before ModelFabric was involved,
// and refusing it would leave ModelFabric a dead end in someone else's trace. It is
// not a credential — a caller who puts a silly value in it confuses its own
// view and nothing else.
//
// The trace id ModelFabric records is therefore the caller's when it brought one,
// and a fresh one otherwise. Either way it is W3C-shaped, so the same value
// can be handed back out in a traceparent without translation.
const (
	// TraceParentHeader carries the W3C trace id and the caller's span.
	TraceParentHeader = "traceparent"
	// TraceStateHeader is vendor-specific trace data. ModelFabric adds nothing to
	// it and forwards it untouched, as the spec requires.
	TraceStateHeader = "tracestate"
)

// TraceContext is what one request carries: the trace it belongs to, the span
// that called us (empty when nobody did), and this node's own span.
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

// Header renders a traceparent naming this node's span as the parent of
// whatever comes next. Forward writes it onto the inbound request, so every
// dispatch path carries it onward through copyRequestHeaders.
//
// The sampled flag is set: ModelFabric records every request it routes, so claiming
// otherwise would be a lie to whatever collects the trace.
func (tc TraceContext) Header() string {
	return "00-" + tc.TraceID + "-" + tc.SpanID + "-01"
}

// parseTraceParent reads a traceparent, returning the trace id and the
// caller's span id.
//
// Unknown future versions are accepted rather than refused: the spec requires
// that a version it does not know still has its first three fields in this
// shape, and dropping the trace because of a version bump would be worse than
// reading the part that is guaranteed.
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

// A trace id is 16 random bytes, as the spec requires. ModelFabric used to mint 8,
// which read more easily but could not be handed to anything else.
func newTraceID() string { return randomHex(16) }

// A span id is 8 bytes, per the spec.
func newSpanID() string { return randomHex(8) }

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		// Never expected. A degenerate id still correlates the hops of one
		// request, which is the job; it just stops being unique across a
		// fleet, so it is deliberately not silently plausible.
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

// validSpan is the same rule at 16 hex.
func validSpan(s string) bool { return len(s) == 16 && isHex(s) && !allZero(s) }

func isHex(s string) bool {
	_, err := hex.DecodeString(s)
	return err == nil
}

func allZero(s string) bool { return strings.Trim(s, "0") == "" }
