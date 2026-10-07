package router

import (
	"context"
	"sync"
	"time"

	"github.com/gregbugaj/modelfabric/internal/mesh"
)

// Queue defers engine selection until a suitable slot is available.
// Returning conversations may reclaim their free home slot immediately;
// other requests wait for Grace to preserve cached follow-up turns.
// MaxWait expiry dispatches using placement order.
//
// Slot availability also requires enough shared KV capacity for resident
// conversations (see fits). Waiting state is local to this entry node.
type Queue struct {
	// Grace is how long a slot must have stayed free before a request that is
	// not its conversation may take it.
	Grace time.Duration
	// MaxWait is the longest a request is held before it is placed regardless.
	MaxWait time.Duration

	mu sync.Mutex
	// freed is, per engine, when each slot this node's requests left was
	// freed, for those freed within Grace and not taken back since.
	freed map[string][]time.Time
	// pending counts admitted requests not yet reflected in mesh load.
	// Read mesh state under q.mu so a stale snapshot cannot miss a request
	// that was admitted, dispatched, and removed from pending.
	pending map[string]int
	// residents is the conversations this queue has placed, by the hash of
	// the last block of their most recent prompt, with where each lives and
	// how large it is. See fits.
	residents map[[32]byte]*resident
	// waiting is the requests held, oldest first.
	waiting []*waiter
	wake    chan struct{} // closed and replaced whenever something changes

	now  func() time.Time
	tick time.Duration // how often a waiting request looks again unprompted
}

type resident struct {
	leaf   [32]byte
	engine string
	// tokens is the conversation's size as the engine last reported it: the
	// prompt of its latest request plus the answer. Zero until one has
	// finished, when the request's length is all there is to go by.
	tokens   int64
	inflight bool
	lastDone time.Time
}

// Ask is what the queue needs to know about a request: the chained block
// hashes of its prompt, which say which conversation it continues, and the
// length of its body, a rough size for a conversation not seen before.
type Ask struct {
	Blocks [][32]byte
	Bytes  int
}

// Grant is a request the queue has let through. Pass it to Done.
type Grant struct {
	name    string
	res     *resident
	pending bool // let through, and not yet counted by the mesh
}

// Sent records that the request has been dispatched and is now in its
// engine's load as the mesh counts it. Call it straight after Acquire.
func (q *Queue) Sent(g *Grant) {
	if g == nil {
		return
	}
	q.mu.Lock()
	q.settle(g)
	q.mu.Unlock()
}

// q.mu is held.
func (q *Queue) settle(g *Grant) {
	if g.pending {
		g.pending = false
		q.pending[g.name]--
	}
}

// headroom is added to a conversation's last known size for what its next
// request will add: a tool's output and the answer to it.
const headroom = 8192

// bytesPerToken estimates unseen prompt size until the engine reports usage.
// The estimate favors admission; observed ratios ranged from 0.8 to 3.9.
const bytesPerToken = 4

type waiter struct {
	since     time.Time
	res       *resident // nil for a conversation not seen before
	need      int64     // tokens of KV pool it needs
	home, why string
}

type Held struct {
	Waited time.Duration
	Home   string // the engine holding its conversation, "" for a new one
	Why    string
}

// Holding lists the requests held right now, longest first.
func (q *Queue) Holding() []Held {
	q.mu.Lock()
	defer q.mu.Unlock()
	out := make([]Held, 0, len(q.waiting))
	for _, w := range q.waiting {
		out = append(out, Held{Waited: q.now().Sub(w.since), Home: w.home, Why: w.why})
	}
	return out
}

func NewQueue(grace, maxWait time.Duration) *Queue {
	return &Queue{Grace: grace, MaxWait: maxWait, freed: map[string][]time.Time{}, pending: map[string]int{}, residents: map[[32]byte]*resident{},
		wake: make(chan struct{}), now: time.Now, tick: 250 * time.Millisecond}
}

func (q *Queue) Waiting() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.waiting)
}

// Done records that a request the queue let through has ended, freeing its
// slot for its conversation to come back to. promptTokens and
// completionTokens are what the engine said the request used, zero when it
// did not say.
func (q *Queue) Done(g *Grant, promptTokens, completionTokens int64) {
	if g == nil {
		return
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	q.settle(g) // a request that never reached an engine is still off the count
	if g.res != nil {
		g.res.inflight, g.res.lastDone = false, q.now()
		if promptTokens > 0 {
			g.res.tokens = promptTokens + completionTokens
		}
	}
	q.freed[g.name] = append(q.fresh(g.name), q.now())
	q.signal()
}

// fresh is the engine's slots freed within Grace, oldest first. q.mu is held.
func (q *Queue) fresh(name string) []time.Time {
	ts := q.freed[name]
	cut := q.now().Add(-q.Grace)
	i := 0
	for i < len(ts) && !ts[i].After(cut) {
		i++
	}
	ts = ts[i:]
	q.freed[name] = ts
	return ts
}

// q.mu is held.
func (q *Queue) signal() {
	close(q.wake)
	q.wake = make(chan struct{})
}

// free counts available slots after admitted requests. Unknown capacity is
// treated as available. q.mu must be held.
func (q *Queue) free(c mesh.Candidate) int64 {
	if c.Slots <= 0 {
		return 1
	}
	return c.Slots - c.Load() - int64(q.pending[c.Name])
}

// alive reports whether a conversation still counts as living on its engine:
// a request of its is running, or its last one ended within Grace. The same
// test as for a slot, and for the same reason. q.mu is held.
func (q *Queue) alive(r *resident) bool {
	return r.inflight || q.now().Sub(r.lastDone) <= q.Grace
}

// fits reports whether c's KV pool can hold w's request beside the
// conversations living there, other than its own. An engine that does not
// report its pool cannot be asked, and fits. q.mu is held.
func (q *Queue) fits(c mesh.Candidate, w *waiter) bool {
	return q.over(c, w) <= 0
}

// over is how many tokens w's request would push c's KV pool past full: what
// the engine would have to throw out of other conversations' caches to take
// it. Zero or less means it fits. q.mu is held.
func (q *Queue) over(c mesh.Candidate, w *waiter) int64 {
	if c.KVPool <= 0 {
		return 0
	}
	used := int64(0)
	for _, r := range q.residents {
		if r != w.res && r.engine == c.Name && q.alive(r) {
			used += r.tokens
		}
	}
	return used + w.need - c.KVPool
}

// leastOver is where to send a request that fits nowhere: the engine with a
// free slot whose other conversations lose the least to it, home on a tie
// since its own cache is there. ok is false when no engine has a slot.
// q.mu is held.
func (q *Queue) leastOver(ordered []mesh.Candidate, home string, w *waiter) (string, bool) {
	best, least := "", int64(0)
	for _, c := range ordered {
		if q.free(c) <= 0 {
			continue
		}
		n := max(q.over(c, w), 0)
		if best == "" || n < least || (n == least && c.Name == home) {
			best, least = c.Name, n
		}
	}
	return best, best != ""
}

// hopeless reports whether no engine's pool could hold w's request even with
// nothing else on it. Waiting for room is then waiting for nothing.
func hopeless(ordered []mesh.Candidate, w *waiter) bool {
	for _, c := range ordered {
		if c.KVPool <= 0 || w.need <= c.KVPool {
			return false
		}
	}
	return true
}

// q.mu is held.
func (q *Queue) lookup(blocks [][32]byte) *resident {
	for i := len(blocks) - 1; i >= affinityMinBlocks-1 && i >= 0; i-- {
		if r, ok := q.residents[blocks[i]]; ok {
			return r
		}
	}
	return nil
}

// grant lets w through to the named engine and records where its
// conversation now lives. q.mu is held.
func (q *Queue) grant(name string, w *waiter, blocks [][32]byte) *Grant {
	q.pending[name]++
	g := &Grant{name: name, pending: true}
	if len(blocks) == 0 {
		return g
	}
	r := w.res
	if r == nil {
		r = &resident{tokens: w.need - headroom}
		w.res = r
	} else {
		delete(q.residents, r.leaf)
	}
	r.leaf, r.engine, r.inflight = blocks[len(blocks)-1], name, true
	q.residents[r.leaf] = r
	g.res = r
	for k, x := range q.residents {
		if !x.inflight && q.now().Sub(x.lastDone) > 10*time.Minute {
			delete(q.residents, k)
		}
	}
	return g
}

// settled reports whether c has a slot that has stayed free for Grace: free
// slots beyond those just vacated. q.mu is held.
func (q *Queue) settled(c mesh.Candidate) bool {
	if c.Slots <= 0 {
		return true
	}
	return q.free(c)-int64(len(q.fresh(c.Name))) > 0
}

// q.mu is held.
func (q *Queue) first(w *waiter) bool {
	return len(q.waiting) == 0 || q.waiting[0] == w
}

// crowdedBy is the prefill-speed ratio required to leave shared home capacity
// for an unowned slot. It is lower than the threshold for an exclusive home.
const crowdedBy = 1.5

// leave selects an alternative when placement prefers a much faster engine,
// or when a faster engine has an unowned slot and fewer residents than home.
// q.mu must be held.
func (q *Queue) leave(ordered []mesh.Candidate, home mesh.Candidate, w *waiter) (string, bool) {
	if !q.first(w) {
		return "", false
	}
	if top := ordered[0]; top.Name != home.Name && q.settled(top) && q.fits(top, w) {
		return top.Name, true
	}
	if home.PrefillTokS <= 0 {
		return "", false
	}
	for _, c := range ordered {
		if c.Name == home.Name || c.Slots <= 0 || c.PrefillTokS < crowdedBy*home.PrefillTokS {
			continue
		}
		if c.Load()+1 <= home.Load() && q.settled(c) && q.fits(c, w) {
			return c.Name, true
		}
	}
	return "", false
}

// decide is one look at the engines for the held request w. ordered is
// placement's order for it and home the engine holding its prompt ("" for
// none). It returns the engine to send it to, first in the order to dispatch
// in, or ok false to keep waiting. q.mu is held.
func (q *Queue) decide(ordered []mesh.Candidate, home string, w *waiter) ([]mesh.Candidate, bool) {
	if len(ordered) == 0 {
		return ordered, true
	}
	// Find home anywhere in the placement order; speed-based ranking may
	// have moved it. Reclaim its newest free slot to avoid waiting through
	// Grace beside the conversation's own cache.
	outgrown := false
	for _, c := range ordered {
		if home == "" || c.Name != home || q.free(c) <= 0 {
			continue
		}
		// A slot, but no room: the conversation has outgrown what its home's
		// pool can hold beside the others there. Going home would push one of
		// them out, so it looks for room like a request with no home.
		if !q.fits(c, w) {
			outgrown = true
			break
		}
		if to, ok := q.leave(ordered, c, w); ok {
			return toFront(ordered, to), true
		}
		if ts := q.fresh(home); len(ts) > 0 {
			q.freed[home] = ts[:len(ts)-1]
		}
		return toFront(ordered, home), true
	}
	if q.first(w) {
		for _, c := range ordered {
			if q.settled(c) && q.fits(c, w) {
				return toFront(ordered, c.Name), true
			}
		}
	}
	// If no pool can ever fit the request, dispatch immediately. Otherwise wait
	// for capacity, then choose the destination with the least cache overflow.
	if outgrown && (hopeless(ordered, w) || q.now().Sub(w.since) >= q.MaxWait) {
		if to, ok := q.leastOver(ordered, home, w); ok {
			return toFront(ordered, to), true
		}
	}
	if q.now().Sub(w.since) >= q.MaxWait {
		return ordered, true
	}
	w.home = home
	switch {
	case !q.first(w):
		w.why = "behind a request that has waited longer"
	case outgrown:
		w.why = "its conversation no longer fits in its own engine's memory beside the others there, and no other engine has room yet"
	case home != "":
		w.why = "its own engine is full, and no other slot has stayed free"
	default:
		w.why = "a new conversation, and no slot has stayed free"
	}
	return nil, false
}

// Admit waits for capacity and returns dispatch order, a Grant to pass to
// Done, and elapsed wait time. It calls order under q.mu to compare current
// mesh load with pending admissions; order must not block.
func (q *Queue) Admit(ctx context.Context, ask Ask, order func() ([]mesh.Candidate, string)) (out []mesh.Candidate, waited time.Duration, g *Grant) {
	w := &waiter{since: q.now()}
	q.mu.Lock()
	w.res = q.lookup(ask.Blocks)
	if w.res != nil && w.res.tokens > 0 {
		w.need = w.res.tokens + headroom
	} else {
		w.need = int64(ask.Bytes/bytesPerToken) + headroom
	}
	q.mu.Unlock()

	queued := false
	defer func() {
		if !queued {
			return
		}
		q.mu.Lock()
		for i, x := range q.waiting {
			if x == w {
				q.waiting = append(q.waiting[:i], q.waiting[i+1:]...)
				break
			}
		}
		// The next in line may have been waiting only on this one's turn.
		q.signal()
		q.mu.Unlock()
	}()
	for {
		q.mu.Lock()
		ordered, home := order()
		if out, ok := q.decide(ordered, home, w); ok {
			if len(out) > 0 {
				g = q.grant(out[0].Name, w, ask.Blocks)
			}
			q.mu.Unlock()
			return out, q.now().Sub(w.since), g
		}
		if !queued {
			q.waiting = append(q.waiting, w)
			queued = true
		}
		wake := q.wake
		q.mu.Unlock()

		// Woken by a slot being freed or a request leaving the queue; the tick
		// covers what nothing announces, a slot's grace running out and a
		// peer's load changing at its next poll.
		t := time.NewTimer(q.tick)
		select {
		case <-ctx.Done():
			t.Stop()
			// The caller is gone. Hand back the order as it stands; the
			// dispatch will fail on the same context and say so.
			return ordered, q.now().Sub(w.since), nil
		case <-wake:
			t.Stop()
		case <-t.C:
		}
	}
}
