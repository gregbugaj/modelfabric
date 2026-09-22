// Package tokentap carries a model's output to whoever is watching, as it is
// generated — the counterpart to `lms log stream`.
//
// It is its own package because two different things in the request path need
// it: the node's front door, which carries every request an app sends here,
// and the engine shim, which llm-d dials on the machine that actually runs the
// model. Those are usually different machines, and a watcher on either should
// see the reply.
package tokentap

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Live output as it is generated, the counterpart to `lms log stream`.
//
// `mfsh log` prints one line per request once it has finished, which says
// nothing while a 90-second answer is being written. This taps the response
// while it streams, so a watcher sees tokens arriving and can tell a slow
// engine from a stuck one.
//
// It is not logging and deliberately not built on the body-capture path:
// nothing here is written to disk or kept in a ring buffer, the tap only runs
// while somebody is watching, and when nobody is the cost is one atomic read
// per write. ModelFabric's promise that prompts are never logged is unchanged —
// this shows the reply, live, to someone already on the node's loopback
// management interface, and forgets it immediately.

// Event is one piece of a reply as it was streamed.
type Event struct {
	Time  time.Time `json:"time"`
	Trace string    `json:"trace"`
	Model string    `json:"model,omitempty"`
	// Kind is "content" for the reply, "reasoning" for thinking the model
	// emits separately, and "done" for the end of a request. Keeping them
	// apart matters on reasoning models: a run of this fleet's benchmark was
	// 98% reasoning by character count, and a stream that merged the two
	// would look like an answer being written for two minutes.
	Kind string `json:"kind"`
	Text string `json:"text,omitempty"`
	// Deltas counts the SSE frames carrying text so far for this request.
	// llama.cpp emits one per token, so this is the token count in practice —
	// named for what it actually counts rather than what it usually equals.
	Deltas int `json:"deltas,omitempty"`
	// Millis is how long the request has been streaming, so a reader can show
	// a rate without keeping its own clock per trace.
	Millis int64 `json:"ms,omitempty"`
	// AtOnce marks a reply that was never streamed: the caller did not ask for
	// it, so the engine generated the whole answer and sent it in one piece.
	// There is no live view to give, and saying so beats a blank panel that
	// looks like a broken tap — which is how this read for every non-streaming
	// caller, a large share of real traffic.
	AtOnce bool `json:"at_once,omitempty"`
}

type Tap struct {
	mu   sync.RWMutex
	subs map[chan Event]struct{}
}

// New returns a tap nobody is watching yet.
func New() *Tap { return &Tap{subs: map[chan Event]struct{}{}} }

// active reports whether anyone is watching. Every response write asks, so it
// must be cheap and must not block a streaming reply.
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

// Publish sends an event to every watcher.
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

// ----------------------------------------------------------------- writer

// tokenWriter parses an SSE reply as it is written and publishes each delta.
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

// finish publishes the end of a request once, so a reader can close out its
// per-trace line whether the stream ended with [DONE] or the handler returned.
// Finish publishes the end of a request once.
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

// watchUpstream stops the writer parsing the caller's body and returns the sink
// the unstream path feeds instead. On an upgraded request the caller receives
// one assembled JSON body; parsing that would report the whole answer arriving
// at once, which is the opposite of what the upgrade is for.
// WatchUpstream stops the writer parsing the caller's body and returns the
// sink the unstream path feeds instead.
func (w *Writer) WatchUpstream() func([]byte) {
	w.giveUp, w.buf = true, nil
	return func(payload []byte) { w.chunk(string(payload)) }
}

// Unwrap lets http.ResponseController reach the real writer, so Flush still
// works through this wrapper. Without it a streamed reply would be buffered
// and arrive in one piece — the tap would break the thing it is watching.
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
