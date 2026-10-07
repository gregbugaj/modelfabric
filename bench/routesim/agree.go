package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/gregbugaj/modelfabric/internal/mesh"
	"github.com/gregbugaj/modelfabric/internal/router"
)

// agree replays a recorded run call by call and asks the router's placement
// where it would have sent each one, given exactly what the recording says
// every engine was doing at that moment. It then lets the call go where it
// really went, so the next question starts from the recorded state and not
// from this placement's own earlier answers.
//
// It is for comparing placement with another scheduler on that scheduler's
// own run. A port of llm-d's rule was run live twice on 2026-10-06 and was a
// third slower than llm-d each time, with no way to see which decisions
// differed: this prints them. It uses no model of an engine, only the record.
var debugAgree = false

// sharedTools stands in for the tool definitions every request carries: about
// 321 tokens of identical text.
var sharedTools = strings.Repeat("tool: bash(command) runs a shell command in the sandbox and returns its output. ", 16)

func agree(tasks []task, engines []*engine, model, nodes string, homeSlot, noRoom bool) {
	name := map[string]string{}
	for _, kv := range strings.Split(nodes, ",") {
		if k, v, ok := strings.Cut(strings.TrimSpace(kv), "="); ok {
			name[k] = v
		}
	}
	type call struct {
		task          string
		first         bool
		body          []byte
		sent, end     float64
		went          string
		prompt, out   int
		choice        router.Choice
		attempt       *router.Attempt
		cachedAtStart int
	}
	var calls []*call
	for _, t := range tasks {
		for i, tn := range t.turns {
			went, ok := name[tn.recEngine]
			if !ok {
				continue
			}
			// A recorded trajectory keeps the messages and not the tool
			// definitions sent ahead of them, which every task shares: 321
			// tokens in these runs, read off the engines. Without them every
			// first call looks like a prompt cached nowhere, which it was not,
			// and placement answers a different question than llm-d was asked.
			body := tn.body
			if !bytes.Contains(body, []byte(`"tools":`)) {
				body = bytes.Replace(body, []byte(`"messages":[`), []byte(`"messages":[{"role":"system","content":"`+sharedTools+`"},`), 1)
			}
			calls = append(calls, &call{task: t.id, first: i == 0, body: body,
				sent: tn.recEnd - tn.recSecs - tn.recWait, end: tn.recEnd, went: went,
				prompt: tn.prompt, out: tn.out, cachedAtStart: tn.recCached})
		}
	}
	sort.Slice(calls, func(i, j int) bool { return calls[i].sent < calls[j].sent })

	now := 0.0
	place := router.NewPlacement(func() time.Time { return time.Unix(0, 0).Add(time.Duration(now * float64(time.Second))) })
	place.HomeSlot, place.NoRoomRule = homeSlot, noRoom
	var running []*call
	type tally struct{ n, same int }
	total := map[string]*tally{"a conversation's first call": {}, "a later call": {}}
	differ := map[string]int{}
	// Moves: a later call sent to a different engine than the conversation's
	// call before it, as recorded and as this placement would have it.
	lastWent := map[string]string{}
	var recMoves, ourMoves, bothMoves int
	for _, c := range calls {
		now = c.sent
		keep := running[:0]
		for _, r := range running {
			if r.end <= now {
				r.attempt.Finished(int64(r.prompt), int64(r.out))
			} else {
				keep = append(keep, r)
			}
		}
		running = keep
		cands := make([]mesh.Candidate, 0, len(engines))
		for _, e := range engines {
			busy := int64(0)
			for _, r := range running {
				if r.went == e.name {
					busy++
				}
			}
			cands = append(cands, mesh.Candidate{Local: true, Node: e.name, Name: e.name,
				Inflight: busy, Slots: int64(len(e.slots)), PrefillTokS: e.prefill, KVPool: int64(e.pool)})
		}
		mesh.Rank(cands, true, 0, "")
		c.choice = place.Order(cands, model, c.body, "")
		would := c.choice.Candidates[0].Name
		kind := "a later call"
		if c.first {
			kind = "a conversation's first call"
		}
		total[kind].n++
		if would == c.went {
			total[kind].same++
		} else {
			differ[fmt.Sprintf("%-28s recorded %-10s this placement %-10s", kind, c.went, would)]++
		}
		if c.first && debugAgree {
			busy := map[string]int{}
			for _, r := range running {
				busy[r.went]++
			}
			fmt.Printf("    first call at %5.0fs: recorded %-10s ours %-10s running now %v, to read %v\n", c.sent-calls[0].sent, c.went, would, busy, place.Reading())
			fmt.Printf("        %s\n", c.choice.Explain())
		}
		if prev, ok := lastWent[c.task]; ok {
			if c.went != prev {
				recMoves++
			}
			if would != prev {
				ourMoves++
			}
			if c.went != prev && would != prev {
				bothMoves++
			}
		}
		lastWent[c.task] = c.went
		c.attempt = place.Placed(c.choice, c.went)
		running = append(running, c)
	}
	fmt.Printf("placement asked where it would send each of %d recorded calls, engine state taken from the record\n\n", len(calls))
	for _, k := range []string{"a conversation's first call", "a later call"} {
		t := total[k]
		if t.n > 0 {
			fmt.Printf("  %-28s %4d calls, same engine as recorded %4d (%.1f%%)\n", k, t.n, t.same, 100*float64(t.same)/float64(t.n))
		}
	}
	fmt.Printf("\n  moved off the engine of its last call: recorded %d, this placement %d, both %d\n", recMoves, ourMoves, bothMoves)
	keys := make([]string, 0, len(differ))
	for k := range differ {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return differ[keys[i]] > differ[keys[j]] })
	fmt.Println("\n  where it differed:")
	for _, k := range keys {
		fmt.Printf("    %4d  %s\n", differ[k], k)
	}
}

// loadCalls reads a replayed run written one call per line, each with the node
// that served it, which a response alone no longer tells apart once two nodes
// run the same build.
func loadCalls(path string) ([]task, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	type line struct {
		Task                string
		Sent, End           float64
		Node, Body          string
		Prompt, Out, Cached int
	}
	by := map[string]*task{}
	var order []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	for sc.Scan() {
		var l line
		if err := json.Unmarshal(sc.Bytes(), &l); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		t := by[l.Task]
		if t == nil {
			t = &task{id: l.Task}
			by[l.Task] = t
			order = append(order, l.Task)
		}
		t.turns = append(t.turns, turn{prompt: l.Prompt, out: l.Out, body: []byte(l.Body),
			recCached: l.Cached, recSecs: l.End - l.Sent, recEngine: l.Node, recEnd: l.End})
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	tasks := make([]task, 0, len(order))
	for _, id := range order {
		t := by[id]
		sort.Slice(t.turns, func(i, j int) bool { return t.turns[i].recEnd < t.turns[j].recEnd })
		tasks = append(tasks, *t)
	}
	return tasks, nil
}
