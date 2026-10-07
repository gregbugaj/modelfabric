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

// `mfsh log -tokens` shows replies as they are generated.
// The tap observes only requests served or proxied by this node; it does not
// collect peer reply text across the network.

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

func tokenStreamCmd(addr string, text, asJSON bool) error {
	// Live streams are loopback-only; reject remote addresses to avoid following the wrong node.
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
	// Track the last printed trace so interleaved requests receive identifying headers.
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
		// Throttle counting output; printing every delta can produce hundreds of lines per second.
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
