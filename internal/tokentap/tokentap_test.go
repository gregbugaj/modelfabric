package tokentap

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// collect runs a response through the tap and returns what a watcher saw.
func collect(t *testing.T, writes []string) ([]Event, *Writer) {
	t.Helper()
	tap := New()
	ch := make(chan Event, 256)
	tap.subs[ch] = struct{}{}

	w := &Writer{ResponseWriter: httptest.NewRecorder(), tap: tap, trace: "abc123"}
	for _, s := range writes {
		if _, err := w.Write([]byte(s)); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	close(ch)
	var got []Event
	for e := range ch {
		got = append(got, e)
	}
	return got, w
}

func frame(payload string) string { return "data: " + payload + "\n\n" }

func chunk(field, text string) string {
	return frame(`{"choices":[{"delta":{"` + field + `":"` + text + `"}}]}`)
}

// The response reaches Write in whatever sizes the network produced, so a
// frame is routinely split across calls. Parsing per-write rather than
// buffering would drop every token that straddled a boundary — and it would
// look like a slow model, not a bug.
func TestTokenWriterFramesSplitAcrossWrites(t *testing.T) {
	whole := chunk("content", "Hello") + chunk("content", " world")
	// One byte at a time is the worst case and the cheapest way to prove the
	// buffer is doing its job.
	var writes []string
	for _, b := range []byte(whole) {
		writes = append(writes, string(b))
	}
	got, _ := collect(t, writes)

	var text strings.Builder
	for _, e := range got {
		if e.Kind == "content" {
			text.WriteString(e.Text)
		}
	}
	if text.String() != "Hello world" {
		t.Fatalf("reassembled %q, want %q", text.String(), "Hello world")
	}
	if len(got) != 2 {
		t.Fatalf("got %d events, want 2", len(got))
	}
	if got[1].Deltas != 2 {
		t.Fatalf("second event counted %d deltas, want 2", got[1].Deltas)
	}
}

// Thinking and reply must stay apart. On this fleet's benchmark model 98% of
// the output is reasoning, so merging them would show an answer apparently
// being written for two minutes.
func TestTokenWriterSeparatesReasoningFromContent(t *testing.T) {
	for _, field := range []string{"reasoning_content", "reasoning"} {
		t.Run(field, func(t *testing.T) {
			got, _ := collect(t, []string{chunk(field, "thinking..."), chunk("content", "answer")})
			if len(got) != 2 {
				t.Fatalf("got %d events, want 2", len(got))
			}
			if got[0].Kind != "reasoning" || got[0].Text != "thinking..." {
				t.Errorf("first event = %+v, want reasoning/thinking...", got[0])
			}
			if got[1].Kind != "content" || got[1].Text != "answer" {
				t.Errorf("second event = %+v, want content/answer", got[1])
			}
		})
	}
}

func TestTokenWriterDoneOnlyOnce(t *testing.T) {
	got, w := collect(t, []string{chunk("content", "hi"), frame("[DONE]")})
	// finish() also runs from the handler's defer, and a watcher that sees two
	// "done" events for one request closes its line and then reopens it.
	w.Finish()

	done := 0
	for _, e := range got {
		if e.Kind == "done" {
			done++
			if e.Deltas != 1 {
				t.Errorf("done reported %d deltas, want 1", e.Deltas)
			}
		}
	}
	if done != 1 {
		t.Fatalf("got %d done events, want 1", done)
	}
}

// A non-streaming reply is one JSON blob with no frame boundary. Buffering it
// forever would cost memory for a response that can never yield a token.
func TestTokenWriterGivesUpOnNonSSE(t *testing.T) {
	big := strings.Repeat("x", sseScanLimit+1)
	got, w := collect(t, []string{big})
	if !w.giveUp {
		t.Fatal("still parsing a reply with no frame boundary past the scan limit")
	}
	if w.buf != nil {
		t.Fatalf("held %d buffered bytes after giving up", len(w.buf))
	}
	if len(got) != 0 {
		t.Fatalf("got %d events from a non-SSE body, want 0", len(got))
	}
}

// The reply must reach the caller byte for byte whatever the tap decides.
func TestTokenWriterPassesBodyThrough(t *testing.T) {
	tap := New()
	rec := httptest.NewRecorder()
	w := &Writer{ResponseWriter: rec, tap: tap, trace: "t"}
	body := chunk("content", "one") + "garbage that is not a frame\n\n" + chunk("content", "two")
	if _, err := w.Write([]byte(body)); err != nil {
		t.Fatalf("write: %v", err)
	}
	if rec.Body.String() != body {
		t.Fatalf("body was altered:\n got %q\nwant %q", rec.Body.String(), body)
	}
}

// With nobody watching the tap must be inert: active() is consulted on every
// request, and publish must not block on a full subscriber channel.
func TestTokenTapInactiveAndNonBlocking(t *testing.T) {
	var nilTap *Tap
	if nilTap.Active() {
		t.Error("a nil tap reported itself active")
	}
	tap := New()
	if tap.Active() {
		t.Error("a tap with no subscribers reported itself active")
	}

	full := make(chan Event, 1)
	tap.subs[full] = struct{}{}
	if !tap.Active() {
		t.Error("a tap with a subscriber reported itself inactive")
	}
	// Two more than the buffer holds: a slow watcher must lose events rather
	// than stall the reply going to the actual caller.
	done := make(chan struct{})
	go func() {
		for i := 0; i < 3; i++ {
			tap.Publish(Event{Kind: "content", Text: "x"})
		}
		close(done)
	}()
	<-done
}

// A caller that does not ask for streaming gets the whole reply in one JSON
// body. There is no live view to give, but showing nothing made the panel look
// broken for a large share of real traffic — aider's benchmark, for one, never
// sets "stream": true.
func TestNonStreamingReplyIsStillShown(t *testing.T) {
	body := `{"id":"chatcmpl-1","choices":[{"message":{"reasoning_content":"thinking hard","content":"the answer"}}],` +
		`"usage":{"completion_tokens":137}}`

	tap := New()
	ch := make(chan Event, 8)
	tap.subs[ch] = struct{}{}
	w := &Writer{ResponseWriter: httptest.NewRecorder(), tap: tap, trace: "t"}
	if _, err := w.Write([]byte(body)); err != nil {
		t.Fatalf("write: %v", err)
	}
	// Nothing may be published until the request ends: the body is not an event
	// stream, so there is no point at which part of it is known to be complete.
	if len(ch) != 0 {
		t.Fatalf("a non-streaming body produced %d events before the request ended", len(ch))
	}
	w.Finish()
	close(ch)

	var kinds []string
	var done Event
	for e := range ch {
		kinds = append(kinds, e.Kind)
		if !e.AtOnce {
			t.Errorf("%s event was not marked as delivered at once", e.Kind)
		}
		if e.Kind == "done" {
			done = e
		}
	}
	want := []string{"reasoning", "content", "done"}
	if strings.Join(kinds, ",") != strings.Join(want, ",") {
		t.Fatalf("kinds = %v, want %v", kinds, want)
	}
	// The engine counted them; counting SSE frames that never existed would
	// report zero for a reply of several thousand tokens.
	if done.Deltas != 137 {
		t.Errorf("done reported %d tokens, want 137 from usage", done.Deltas)
	}
}
