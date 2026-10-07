// Package tokentap streams model output to watchers at the entry node and
// engine shim, including requests that llm-d sends directly to the shim.
package tokentap

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Taps run only while subscribed through the loopback management interface.
// Output is neither written to disk nor retained in a ring buffer.

// Event is one piece of a reply as it was streamed.
type Event struct {
	Time  time.Time `json:"time"`
	Trace string    `json:"trace"`
	Model string    `json:"model,omitempty"`
	// Kind is "content", "reasoning", or "done". Reasoning remains separate
	// from the visible answer.
	Kind string `json:"kind"`
	Text string `json:"text,omitempty"`
	// Deltas counts SSE frames containing text, not tokenizer-derived tokens.
	Deltas int `json:"deltas,omitempty"`
	// Millis is how long the request has been streaming, so a reader can show
	// a rate without keeping its own clock per trace.
	Millis int64 `json:"ms,omitempty"`
	// AtOnce marks a non-streaming reply delivered as one complete body.
	AtOnce bool `json:"at_once,omitempty"`
}

type Tap struct {
	mu   sync.RWMutex
	subs map[chan Event]struct{}
}

func New() *Tap { return &Tap{subs: map[chan Event]struct{}{}} }

// Active reports whether anyone is watching. Every response write asks, so
// it must be cheap and must not block a streaming reply.
func (t *Tap) Active() bool {
	if t == nil {
		return false
	}
	t.mu.RLock()
	n := len(t.subs)
	t.mu.RUnlock()
	return n > 0
}

func (t *Tap) Publish(e Event) {
	if t == nil {
		return
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	for ch := range t.subs {
		// A slow watcher drops events rather than stalling the reply going to
		// the actual caller. Watching must never be able to slow serving.
		select {
		case ch <- e:
		default:
		}
	}
}

// Anything that is not an OpenAI-style event stream passes through untouched.
// Writer parses an SSE reply as it is written and publishes each delta.
type Writer struct {
	http.ResponseWriter
	tap   *Tap
	trace string
	model string
	start time.Time

	buf    []byte // bytes not yet forming a complete frame
	deltas int
	// give up stops parsing for the rest of this response: either the reply
	// is not an event stream, or it is malformed. Either way, keep serving.
	giveUp bool
	done   bool
}

// sseScanLimit is how much unterminated output is tolerated before deciding
// this is not an event stream. A non-streaming JSON reply arrives as one blob
// with no frame boundary, and buffering all of it would cost memory for
// nothing.
const sseScanLimit = 64 << 10

func (w *Writer) Write(b []byte) (int, error) {
	n, err := w.ResponseWriter.Write(b)
	if n > 0 && !w.giveUp {
		w.scan(b[:n])
	}
	return n, err
}

func (w *Writer) scan(b []byte) {
	w.buf = append(w.buf, b...)
	for {
		i := strings.Index(string(w.buf), "\n\n")
		if i < 0 {
			if len(w.buf) > sseScanLimit {
				w.giveUp, w.buf = true, nil
			}
			return
		}
		frame := string(w.buf[:i])
		w.buf = w.buf[i+2:]
		w.frame(frame)
	}
}

func (w *Writer) frame(frame string) {
	for _, line := range strings.Split(frame, "\n") {
		data, ok := strings.CutPrefix(strings.TrimSpace(line), "data:")
		if !ok {
			continue
		}
		data = strings.TrimSpace(data)
		if data == "[DONE]" {
			w.Finish()
			return
		}
		w.chunk(data)
	}
}

// chunk publishes the deltas in one chunk payload. Separate from frame() so
// the unstream path can feed it the upstream chunks directly: there the
// caller's response is one assembled JSON body, and watching that would show
// the answer arriving all at once when it did not.
func (w *Writer) chunk(data string) {
	{
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content          string `json:"content"`
					ReasoningContent string `json:"reasoning_content"`
					Reasoning        string `json:"reasoning"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if json.Unmarshal([]byte(data), &chunk) != nil || len(chunk.Choices) == 0 {
			return
		}
		d := chunk.Choices[0].Delta
		// Engines disagree on the field name for thinking; llama.cpp uses
		// reasoning_content, others reasoning. Take whichever is present.
		reasoning := d.ReasoningContent
		if reasoning == "" {
			reasoning = d.Reasoning
		}
		if reasoning != "" {
			w.emit("reasoning", reasoning)
		}
		if d.Content != "" {
			w.emit("content", d.Content)
		}
	}
}

// atOnce publishes a reply that arrived whole, which is what a caller that did
// not set "stream": true receives. The buffer already holds it: scan() only
// drains on a frame boundary, and a JSON body has none.
func (w *Writer) atOnce() {
	if w.deltas > 0 || len(w.buf) == 0 {
		return
	}
	var body struct {
		Choices []struct {
			Message struct {
				Content          string `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
				Reasoning        string `json:"reasoning"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if json.Unmarshal(w.buf, &body) != nil || len(body.Choices) == 0 {
		return
	}
	m := body.Choices[0].Message
	reasoning := m.ReasoningContent
	if reasoning == "" {
		reasoning = m.Reasoning
	}
	send := func(kind, text string) {
		if text == "" {
			return
		}
		w.tap.Publish(Event{
			Time: time.Now(), Trace: w.trace, Model: w.model, Kind: kind,
			Text: text, Millis: time.Since(w.start).Milliseconds(), AtOnce: true,
		})
	}
	send("reasoning", reasoning)
	send("content", m.Content)
	// The engine counted the tokens; counting frames that never existed would
	// report zero for a reply of several thousand.
	if n := body.Usage.CompletionTokens; n > 0 {
		w.deltas = n
	}
}

func (w *Writer) emit(kind, text string) {
	w.deltas++
	w.tap.Publish(Event{
		Time: time.Now(), Trace: w.trace, Model: w.model, Kind: kind, Text: text,
		Deltas: w.deltas, Millis: time.Since(w.start).Milliseconds(),
	})
}

// Finish publishes request completion once, whether triggered by [DONE]
// or by the handler returning.
func (w *Writer) Finish() {
	if w.done {
		return
	}
	w.done = true
	atOnce := false
	if w.deltas == 0 {
		before := w.deltas
		w.atOnce()
		atOnce = w.deltas != before || len(w.buf) > 0
	}
	w.tap.Publish(Event{
		Time: time.Now(), Trace: w.trace, Model: w.model, Kind: "done",
		Deltas: w.deltas, Millis: time.Since(w.start).Milliseconds(), AtOnce: atOnce,
	})
	w.buf = nil
}

// WatchUpstream returns a sink for the actual engine stream and disables
// parsing of the assembled JSON response sent to a non-streaming caller.
func (w *Writer) WatchUpstream() func([]byte) {
	w.giveUp, w.buf = true, nil
	return func(payload []byte) { w.chunk(string(payload)) }
}

// Unwrap lets http.ResponseController reach the real writer, so Flush still
// works through this wrapper. Without it a streamed reply would be buffered
// and arrive in one piece; the tap would break the thing it is watching.
func (w *Writer) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// Subscribe registers a watcher and returns the channel and a cancel.
func (t *Tap) Subscribe(buffer int) (chan Event, func()) {
	ch := make(chan Event, buffer)
	t.mu.Lock()
	t.subs[ch] = struct{}{}
	t.mu.Unlock()
	return ch, func() {
		t.mu.Lock()
		delete(t.subs, ch)
		t.mu.Unlock()
	}
}

// Wrap returns a Writer that watches w's reply, or w unchanged when nobody is
// watching, along with the function that closes the request out.
func (t *Tap) Wrap(w http.ResponseWriter, trace, model string) (http.ResponseWriter, func()) {
	if !t.Active() {
		return w, func() {}
	}
	tw := &Writer{ResponseWriter: w, tap: t, trace: trace, model: model, start: time.Now()}
	return tw, tw.Finish
}

// WriterOf finds the tap's writer in a chain of response writers, or nil when
// nobody is watching.
func WriterOf(w http.ResponseWriter) *Writer {
	for range 8 { // a wrapper chain this deep is a bug, not a configuration
		if tw, ok := w.(*Writer); ok {
			return tw
		}
		u, ok := w.(interface{ Unwrap() http.ResponseWriter })
		if !ok {
			return nil
		}
		w = u.Unwrap()
	}
	return nil
}
