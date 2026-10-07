package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
)

// Read enough of the body to select a model's scheduler. The former 64 KiB
// limit left oversized requests with an empty model and bypassed llm-d.
// Read the model from the buffered prefix when the full body exceeds the cap.

// bodyPeekMax bounds buffered request bytes at 4 MiB, covering long
// conversations and inline images. Small requests allocate only their own size.
const bodyPeekMax = 4 << 20

type peeked struct {
	Model string
	// Body is what was read: the whole body when Whole, otherwise its first
	// bodyPeekMax bytes. Either way the request's own body still yields
	// everything, so a proxy downstream sees the request intact.
	Body []byte
	// Whole says Body is the entire request body. Anything that rewrites or
	// fully parses the body needs this: a prefix is not a JSON document.
	Whole bool
}

func peekRequest(r *http.Request) peeked {
	if r.Body == nil || !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		return peeked{}
	}
	buf, err := io.ReadAll(io.LimitReader(r.Body, bodyPeekMax+1))
	if err != nil {
		// Restore partial reads so downstream forwarding reports the body error.
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
		return peeked{Body: buf, Whole: true}
	}
	return peeked{Model: probe.Model, Body: buf, Whole: true}
}

// topLevelString reads a root-object string field from a possibly truncated
// JSON document. Token parsing avoids matching fields inside conversations;
// fields beyond the truncation point are unavailable.
func topLevelString(buf []byte, key string) (string, bool) {
	dec := json.NewDecoder(bytes.NewReader(buf))
	type frame struct {
		object   bool
		wantsKey bool
	}
	var stack []frame
	wanted := false // the last key read was the one being looked for
	for {
		t, err := dec.Token()
		if err != nil {
			return "", false
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
			return "", false
		}
	}
}

// imageMarkers are how a multimodal content part names itself, as the OpenAI
// and Responses shapes spell it.
var imageMarkers = [][]byte{
	[]byte(`"image_url"`), []byte(`"input_image"`), []byte(`"image"`),
}

// carriesImage checks complete JSON bodies or quoted markers in a prefix.
// Prefix detection favors false positives: a vision engine can serve text,
// but an engine without a projector fails image requests.
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
