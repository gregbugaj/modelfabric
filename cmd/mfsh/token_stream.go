package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

// `mfsh log -tokens`: the reply text as it is generated.
//
// `mfsh log` prints one line when a request finishes, which says nothing
// during the ninety seconds it takes to write an answer — a slow engine and a
// stuck one look identical until the row appears. This shows the tokens
// arriving.
//
// Deliberately this node only. The tap sits on this node's front door, so it
// sees the traffic this node is serving or proxying and nothing a peer is
// doing on its own. Watching the whole mesh would mean shipping reply text
// between machines, which is not something to do by default for a view that
// exists to be glanced at.

// tokenEvent mirrors internal/server.TokenEvent.
type tokenEvent struct {
	Time   time.Time `json:"time"`
	Trace  string    `json:"trace"`
	Model  string    `json:"model"`
	Kind   string    `json:"kind"`
	Text   string    `json:"text"`
	Deltas int       `json:"deltas"`
	Millis int64     `json:"ms"`
	AtOnce bool      `json:"at_once"`
}

// tokenStreamCmd follows live replies. With text it prints what is generated;
// otherwise one updating line per request, which is what you want when four
// agents are answering at once.
func tokenStreamCmd(addr string, text, asJSON bool) error {
	// A live stream is served only on the node's own loopback listener, so a
	// remote -addr would either be refused by the peer listener or, worse,
	// quietly follow the wrong node.
	if err := mustBeLocalBecause(addr, "the live token stream",
		"is served only on that node's own loopback listener", "log -tokens"); err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		strings.TrimRight(addr, "/")+"/z/log/tokens", nil)
	if err != nil {
		return err
	}
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return fmt.Errorf("stream refused: %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	if !asJSON {
		fmt.Fprintln(os.Stderr, dim("Live replies from this node. Nothing is recorded; closing this stops the tap. Ctrl-C to stop."))
		if why := routerBypassed(addr); why != "" {
			fmt.Fprintln(os.Stderr, dim(why))
		}
	}

	w := &tokenPrinter{text: text, json: asJSON, seen: map[string]*tokenLine{}}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	for sc.Scan() {
		data, ok := strings.CutPrefix(sc.Text(), "data: ")
		if !ok {
			continue
		}
		if asJSON {
			fmt.Println(data)
			continue
		}
		var e tokenEvent
		if json.Unmarshal([]byte(data), &e) != nil {
			continue
		}
		w.on(e)
	}
	if ctx.Err() != nil {
		return nil
	}
	return sc.Err()
}

type tokenLine struct {
	model    string
	reason   int
	content  int
	lastKind string
}

type tokenPrinter struct {
	text bool
	json bool
	seen map[string]*tokenLine
	// last is the trace whose text was printed most recently. With several
	// requests in flight the output would otherwise be an unattributable
	// braid, so a header is printed whenever the speaker changes.
	last string
}

func (p *tokenPrinter) on(e tokenEvent) {
	id := e.Trace
	if len(id) > 8 {
		id = id[:8]
	}
	l := p.seen[e.Trace]
	if l == nil {
		l = &tokenLine{model: e.Model}
		p.seen[e.Trace] = l
	}
	switch e.Kind {
	case "reasoning":
		l.reason++
	case "content":
		l.content++
	}

	if e.Kind == "done" {
		delete(p.seen, e.Trace)
		rate := 0.0
		if e.Millis > 0 {
			rate = float64(e.Deltas) / (float64(e.Millis) / 1000)
		}
		if p.text && p.last == e.Trace {
			fmt.Println()
			p.last = ""
		}
		how := ""
		if e.AtOnce {
			// Not a live view and must not read as one: the caller did not ask
			// for streaming, so the engine wrote the whole reply before
			// sending any of it.
			how = dim(" · not streamed")
		}
		fmt.Printf("%s  %s %s  %s  %s  %s%s\n",
			dim(e.Time.Local().Format("15:04:05")), dim(id), green("done"),
			dim(e.Model),
			fmt.Sprintf("%d tokens (%d thinking, %d reply)", e.Deltas, l.reason, l.content),
			cyan(fmt.Sprintf("%.0f tok/s", rate)), how)
		return
	}

	if !p.text {
		// Counting mode: one line per request, only when the rate is worth
		// reprinting. Every delta would be hundreds of lines a second.
		if e.Deltas%25 != 0 {
			return
		}
		rate := 0.0
		if e.Millis > 0 {
			rate = float64(e.Deltas) / (float64(e.Millis) / 1000)
		}
		what := "thinking"
		if e.Kind == "content" {
			what = "replying"
		}
		fmt.Printf("%s  %s %-8s %s  %5d tokens  %s\n",
			dim(e.Time.Local().Format("15:04:05")), dim(id), what, dim(e.Model),
			e.Deltas, cyan(fmt.Sprintf("%.0f tok/s", rate)))
		return
	}

	// Text mode. A header whenever the speaker or the kind changes, so
	// interleaved requests stay readable and thinking is never mistaken for
	// the answer.
	if p.last != e.Trace || l.lastKind != e.Kind {
		if p.last != "" {
			fmt.Println()
		}
		label := yellow("thinking")
		if e.Kind == "content" {
			label = green("reply")
		}
		fmt.Printf("%s %s %s\n", dim(id), label, dim(e.Model))
		p.last, l.lastKind = e.Trace, e.Kind
	}
	fmt.Print(e.Text)
}
