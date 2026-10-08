package supervisor

import (
	"regexp"
	"strconv"
	"sync"
	"time"
)

// liveGen is how fast each engine is writing right now, summed over its
// slots.
//
// The rates an engine reports through its metrics are per request and
// averaged since it started. Those say how fast one answer is written. They
// do not say what a machine is producing: an engine with four busy slots at
// 22 tok/s each read as the slowest in the mesh while it was writing about
// four times that. The engine's token counters do not help either, because
// llama.cpp adds a request's tokens when the request ends: polled every two
// seconds, a busy engine read +0 for half a minute and then +1400 at once.
//
// What does say it is the engine's log. While a slot generates, llama.cpp
// prints that slot's rate over the last three seconds, about every three
// seconds. The tail (devtail.go) already reads those lines.
type liveGen struct {
	mu      sync.Mutex
	engines map[string]map[int]slotRate
}

type slotRate struct {
	tokS float64
	at   time.Time
}

// liveGenFresh is how long a slot's last rate stands. llama.cpp prints one
// about every three seconds, so two missed in a row means the slot has
// stopped writing even if its release line was not seen.
const liveGenFresh = 7 * time.Second

var (
	genProgress = regexp.MustCompile(`print_timing: id\s+(\d+) \| task \d+ \| n_gen =\s*\d+, tg =\s*[0-9.]+ t/s, tg_3s =\s*([0-9.]+) t/s`)
	slotRelease = regexp.MustCompile(`slot\s+release: id\s+(\d+) \|`)
)

// saw marks that the engine's log is being read and is in llama.cpp's
// format, so an engine with nothing generating reads as 0 and not unknown.
func (g *liveGen) saw(id string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.engines == nil {
		g.engines = map[string]map[int]slotRate{}
	}
	if g.engines[id] == nil {
		g.engines[id] = map[int]slotRate{}
	}
}

// line takes one line of an engine's log, read at now.
func (g *liveGen) line(id, line string, now time.Time) {
	if m := genProgress.FindStringSubmatch(line); m != nil {
		slot, _ := strconv.Atoi(m[1])
		rate, err := strconv.ParseFloat(m[2], 64)
		if err != nil {
			return
		}
		g.saw(id)
		g.mu.Lock()
		g.engines[id][slot] = slotRate{tokS: rate, at: now}
		g.mu.Unlock()
		return
	}
	if m := slotRelease.FindStringSubmatch(line); m != nil {
		slot, _ := strconv.Atoi(m[1])
		g.mu.Lock()
		delete(g.engines[id], slot)
		g.mu.Unlock()
	}
}

// forget drops an engine that is gone.
func (g *liveGen) forget(id string) {
	g.mu.Lock()
	delete(g.engines, id)
	g.mu.Unlock()
}

// rate is the tokens per second the engine is writing at now, across its
// slots. ok is false when that is not known: the engine's log has not been
// read, or is not one this can read.
func (g *liveGen) rate(id string, now time.Time) (tokS float64, ok bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	slots, ok := g.engines[id]
	if !ok {
		return 0, false
	}
	for _, r := range slots {
		if now.Sub(r.at) <= liveGenFresh {
			tokS += r.tokS
		}
	}
	return tokS, true
}
