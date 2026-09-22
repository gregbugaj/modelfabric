package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
)

// Reading enough of a request to route it, without holding what ModelFabric does not
// need.
//
// The front door has to know the model before it can decide who serves the
// request: llm-d is enabled for one model and every other model is the router's
// business. The model is in the body, so the body has to be read — and the first
// version of this gave up on anything over 64KiB rather than buffer a large
// upload. An unread model is the empty string, nothing matches it, and the
// request quietly went to ModelFabric's own router. So llm-d never saw a long agent
// conversation, which is the workload it was brought in for: measured
// 2026-09-25, a 61,510-byte body was scheduled and a 71,750-byte one was not,
// with nothing said either way.
//
// Two things fix it. The cap is now large enough for the bodies this actually
// serves, and a body past it is no longer given up on — the model is read out of
// the prefix that was going to be read anyway.

// bodyPeekMax is how much of a request body ModelFabric will hold to read it.
//
// It has to cover what the fleet really sends: a 90k-token agent conversation is
// about 360KB, and an image request in the 2026-09-24 multimodal test was 986KB.
// 4MiB covers both with room to spare, and the cost is bounded by what is
// in flight rather than by this number — a small request still holds only its
// own size. The body is read before it is forwarded either way, so the only
// latency this adds is the time to read it from a local socket, against a
// prefill measured in seconds.
const bodyPeekMax = 4 << 20

// peeked is what reading the front of a request produced.
type peeked struct {
	// Model is the top-level "model" field, empty when it could not be read.
	Model string
	// Body is what was read: the whole body when Whole, otherwise its first
	// bodyPeekMax bytes. Either way the request's own body still yields
	// everything, so a proxy downstream sees the request intact.
	Body []byte
	// Whole says Body is the entire request body. Anything that rewrites or
	// fully parses the body needs this: a prefix is not a JSON document.
	Whole bool
}

// peekRequest reads what it can of a JSON request body and puts it back.
func peekRequest(r *http.Request) peeked {
	if r.Body == nil || !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		return peeked{}
	}
	buf, err := io.ReadAll(io.LimitReader(r.Body, bodyPeekMax+1))
	if err != nil {
		// Put back whatever arrived before the error; it is the proxy's problem
		// to report, not this function's to hide.
		r.Body = struct {
			io.Reader
			io.Closer
		}{io.MultiReader(bytes.NewReader(buf), r.Body), r.Body}
		return peeked{}
	}
	if len(buf) > bodyPeekMax {
		// Bigger than ModelFabric will hold. The rest streams as it always did, and
		// the model is read from the prefix rather than abandoned.
		r.Body = struct {
			io.Reader
			io.Closer
		}{io.MultiReader(bytes.NewReader(buf), r.Body), r.Body}
		model, _ := topLevelString(buf, "model")
		return peeked{Model: model, Body: buf}
	}
	_ = r.Body.Close()
	r.Body = io.NopCloser(bytes.NewReader(buf))
	var probe struct {
		Model string `json:"model"`
	}
	if json.Unmarshal(buf, &probe) != nil {
		// Whole, and not JSON ModelFabric understands. Still whole: a caller that
		// wants to rewrite it can see for itself.
		return peeked{Body: buf, Whole: true}
	}
	return peeked{Model: probe.Model, Body: buf, Whole: true}
}

// topLevelString reads one string field of the root object, from a document that
// may be cut off partway through.
//
// It walks tokens rather than matching bytes, because "model" appears inside
// conversations too — a request asking about a model, or a tool call naming one —
// and the field being looked for is the request's own, at the root. Truncation
// ends the walk: whatever was found before the cut stands, and a field beyond it
// is simply not found.
func topLevelString(buf []byte, key string) (string, bool) {
	dec := json.NewDecoder(bytes.NewReader(buf))
	// One frame per open container. Objects alternate key, value; arrays do not,
	// which is the whole reason this needs a stack rather than a depth counter.
	type frame struct {
		object   bool
		wantsKey bool
	}
	var stack []frame
	wanted := false // the last key read was the one being looked for
	for {
		t, err := dec.Token()
		if err != nil {
			return "", false // end of input, or the truncation
		}
		top := func() *frame {
			if len(stack) == 0 {
				return nil
			}
			return &stack[len(stack)-1]
		}
		if d, ok := t.(json.Delim); ok {
			switch d {
			case '{', '[':
				// A container in a value position consumes that value.
				if f := top(); f != nil && f.object {
					f.wantsKey = true
				}
				stack = append(stack, frame{object: d == '{', wantsKey: d == '{'})
			default:
				if len(stack) > 0 {
					stack = stack[:len(stack)-1]
				}
			}
			wanted = false
			continue
		}
		f := top()
		if f == nil {
			return "", false // a bare scalar: not the object this reads
		}
		if f.object && f.wantsKey {
			s, _ := t.(string)
			// Only the root object's own keys count.
			wanted = len(stack) == 1 && s == key
			f.wantsKey = false
			continue
		}
		if f.object {
			f.wantsKey = true
		}
		if wanted {
			if s, ok := t.(string); ok {
				return s, true
			}
			return "", false // present, and not a string
		}
	}
}

// imageMarkers are how a multimodal content part names itself, as the OpenAI
// and Responses shapes spell it.
var imageMarkers = [][]byte{
	[]byte(`"image_url"`), []byte(`"input_image"`), []byte(`"image"`),
}

// carriesImage reports whether a request has an image in it, from a body that may
// be only a prefix.
//
// A whole body is decoded properly. A prefix cannot be, so it is searched for the
// markers instead, and that is deliberately the loose direction: a false positive
// sends a text request to an engine that can also read images, which works, while
// a false negative sends an image to an engine with no projector, which fails
// inside llama.cpp with "failed to process mtmd chunk". The markers are quoted,
// so prose mentioning an image does not match.
func carriesImage(p peeked, precise func([]byte) bool) bool {
	if len(p.Body) == 0 {
		return false
	}
	if p.Whole {
		return precise(p.Body)
	}
	for _, m := range imageMarkers {
		if bytes.Contains(p.Body, m) {
			return true
		}
	}
	return false
}
