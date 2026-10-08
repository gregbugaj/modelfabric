package router

import (
	"fmt"
	"strings"
	"time"

	"github.com/gregbugaj/modelfabric/internal/devlog"
	"github.com/gregbugaj/modelfabric/internal/mesh"
)

// The router's half of the developer log: three entries a request, as they
// happen. Received, when it arrives. Sent, when an engine is chosen, with the
// reason. Finished, with what it cost. The engine's own account of the
// request falls between the last two (see supervisor/devtail.go).
//
// Bodies go in only while capture is on, cut at the same cap as the traffic
// log's. Everything else is sizes and counts, which is what "prompts are not
// recorded unless capture is switched on" allows.

// devReceived records a request's arrival.
func (r *Router) devReceived(trace, path, model string, body []byte, messages int, stream, forwarded bool) {
	if r.Dev == nil {
		return
	}
	fields := map[string]any{"path": path, "bytes": len(body), "stream": stream}
	what := "POST " + path
	if messages > 0 {
		fields["messages"] = messages
		what += fmt.Sprintf(", %d %s", messages, plural(messages, "message"))
	}
	what += ", " + sizeOf(len(body))
	if stream {
		what += ", streaming"
	}
	e := devlog.Entry{Source: devlog.Router, Trace: trace, Model: model, Fields: fields,
		Msg: "request received: " + what}
	if forwarded {
		// The node the client called has already logged the request and why
		// it came here. This is the same request arriving, not a second one.
		e.Level, e.Msg = devlog.Debug, "request received from a peer: "+what
	} else if bl := r.bodyLog(); bl.Enabled {
		e.Body, e.Truncated = bl.Clip(body)
	}
	r.Dev.Add(e)
}

// devSent records that the request is being sent to c, and why c.
func (r *Router) devSent(trace, model string, c mesh.Candidate, choice Choice, attempt int, queued time.Duration) {
	if r.Dev == nil {
		return
	}
	fields := map[string]any{"node": c.Node, "local": c.Local, "in_flight": c.Load()}
	if c.Slots > 0 {
		fields["slots"] = c.Slots
	}
	to := candidateName(c)
	msg := "sent to " + to
	if r.aff != nil {
		why, more := choice.Why(c.Name)
		for k, v := range more {
			fields[k] = v
		}
		msg += ": " + why
	}
	if attempt > 0 {
		fields["attempt"] = attempt + 1
		msg = "retrying on " + to + ", the next candidate"
	}
	if queued > 0 {
		fields["queued_ms"] = queued.Milliseconds()
		msg += fmt.Sprintf(" (held %s for a slot first)", queued.Round(10*time.Millisecond))
	}
	if c.Slots > 0 && c.Load() >= c.Slots {
		msg += fmt.Sprintf(". Every slot there is busy (%d in flight on %d), so it waits in that engine's queue", c.Load(), c.Slots)
	}
	r.Dev.Add(devlog.Entry{Source: devlog.Router, Trace: trace, Model: model, Engine: to, Msg: msg, Fields: fields})
}

// devFinished records how a request ended, from the event the traffic log
// gets. Every way a request can end emits one, a refusal included.
func (r *Router) devFinished(e Event) {
	if r.Dev == nil {
		return
	}
	fields := map[string]any{"status": e.Status, "ms": e.Millis}
	took := (time.Duration(e.Millis) * time.Millisecond).Round(10 * time.Millisecond)
	engine := e.Node
	if e.Engine != "" && e.Engine != e.Node {
		engine += "/" + e.Engine
	}
	out := devlog.Entry{Time: time.Now(), Source: devlog.Router, Trace: e.Trace, Model: e.Model, Engine: engine, Fields: fields}
	if e.Via != "" {
		fields["via"] = e.Via
	}
	if e.Error != "" || e.Status >= 400 {
		out.Level = devlog.Error
		if e.Status < 500 && e.Status != 0 {
			// The caller's request was refused: wrong model, bad body. Worth
			// seeing, not a fault of the mesh.
			out.Level = devlog.Warn
		}
		out.Msg = fmt.Sprintf("failed with %d", e.Status)
		if e.Error != "" {
			out.Msg += ": " + e.Error
			fields["error"] = e.Error
		}
	} else {
		parts := []string{fmt.Sprintf("finished: %d in %s", e.Status, took)}
		if e.PromptTokens > 0 {
			fields["prompt_tokens"], fields["cached_tokens"] = e.PromptTokens, e.CachedTokens
			parts = append(parts, fmt.Sprintf("%d prompt tokens, %d from cache (%d%%)",
				e.PromptTokens, e.CachedTokens, 100*e.CachedTokens/e.PromptTokens))
		}
		if e.CompletionTokens > 0 {
			fields["completion_tokens"] = e.CompletionTokens
			gen := fmt.Sprintf("%d generated", e.CompletionTokens)
			if e.TokensPerSec > 0 {
				fields["tokens_per_sec"] = e.TokensPerSec
				gen += fmt.Sprintf(" at %.1f tok/s", e.TokensPerSec)
			}
			parts = append(parts, gen)
		}
		if e.FirstOutputMillis > 0 {
			fields["first_output_ms"] = e.FirstOutputMillis
			parts = append(parts, fmt.Sprintf("first output after %s",
				(time.Duration(e.FirstOutputMillis)*time.Millisecond).Round(10*time.Millisecond)))
		}
		if e.Drafted > 0 {
			fields["drafted"], fields["draft_accepted"] = e.Drafted, e.DraftAccepted
			parts = append(parts, fmt.Sprintf("%d of %d drafted tokens accepted", e.DraftAccepted, e.Drafted))
		}
		if e.QueuedMillis > 0 {
			fields["queued_ms"] = e.QueuedMillis
		}
		out.Msg = strings.Join(parts, "; ")
	}
	// The event carries bodies only while capture is on (see Forward).
	out.Body, out.Truncated = e.RespBody, e.Truncated && e.RespBody != ""
	r.Dev.Add(out)
}

func plural(n int, word string) string {
	if n == 1 {
		return word
	}
	return word + "s"
}

// sizeOf writes a byte count the way a person says it.
func sizeOf(n int) string {
	switch {
	case n < 1024:
		return fmt.Sprintf("%d bytes", n)
	case n < 1<<20:
		return fmt.Sprintf("%.1f KB", float64(n)/1024)
	default:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	}
}
