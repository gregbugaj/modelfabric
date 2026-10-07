package router

import (
	"context"
	"sync"
	"time"

	"github.com/gregbugaj/modelfabric/internal/mesh"
)

// Queue holds a request at the router while no engine has a slot it should
// take, and chooses its engine when one does, not when it arrived.
//
// Without it a request is sent the moment it arrives, and when every slot is
// busy the waiting happens inside whichever engine was picked. That queue is
// first come first served, belongs to one machine and cannot be undone: in the
// SWE runs of 2026-10-05 requests sat behind a ten-minute call on the one-slot
// node while GPU slots opened and closed beside them, 108 to 126 minutes of
// waiting in every run whatever the placement rule was.
//
// The idea is llm-d's flow control (queue in the scheduler, bind late) with
// one thing it does not have and llama.cpp needs. A slot that has just
// finished a call is not free: its conversation asks again 0.2 seconds later
// (92 to 96% within two seconds, over 775 recorded calls) and its cache is in
// that slot. Handing such a slot to whoever is waiting was tried as a placement
// rule and took 104 minutes where leaving it alone took 75, because the owner
// then takes the next one's slot, and so on. So a slot is offered to a waiting
// request only after it has stayed free for Grace: after its owner had the
// chance to come back and did not.
//
// What a request may do, each time it looks:
//
//   - Its own engine has a free slot: go there at once. Nothing waits for home.
//   - Some engine has a slot that has stayed free for Grace, and this request
//     has waited longest: take it.
//   - It has waited MaxWait: go wherever placement says, as if there were no
//     queue. A request is never refused or lost by waiting here.
//
// "Has a slot" also means "has room for this conversation". An engine's slots
// share one KV pool, and a free slot says nothing about whether the pool can
// hold what would go in it. In the run of 2026-10-06 three conversations of
// 85, 92 and 97 thousand tokens all lived on a four-slot engine with a
// 262,144-token pool. A slot was always free, so each went home, and each
// turn the engine threw out another's cache to make room: three cold reads of
// 90,000 tokens in rotation, the other slots writing at one token a second
// meanwhile, and a 131,072-token pool idle on the machine beside it. So the
// queue keeps each conversation's size, as the engine last reported it, and
// an engine has room only if its pool holds that beside the conversations
// already living there (see fits).
//
// The queue is this node's alone and lives in memory. Two entry nodes each
// have their own and know nothing of each other's waiting requests.
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
	// pending is, per engine, requests this queue has let through that the
	// mesh does not count yet: between deciding and the dispatch that makes
	// the engine's load show it, a second request must not be given the same
	// slot. It is only right because each request reads the mesh while it
	// holds the queue's lock (see Admit). Read before taking the lock, as it
	// first was, a snapshot could predate a request that had since been let
	// through, sent and taken off this count, and the slot was given twice:
	// eight agents starting together put five conversations on four slots.
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

// resident is one conversation and the engine it lives on.
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

// settle takes g off the pending count, once. q.mu is held.
func (q *Queue) settle(g *Grant) {
	if g.pending {
		g.pending = false
		q.pending[g.name]--
	}
}

// headroom is added to a conversation's last known size for what its next
// request will add: a tool's output and the answer to it.
const headroom = 8192

// bytesPerToken turns the length of a request never seen before into a size.
// It is a guess, and a low one, so that a new conversation is not thought too
// large to place: measured over 756 recorded requests the ratio ran from 0.8
// to 3.9. It is used once per conversation, until its first answer says.
const bytesPerToken = 4

type waiter struct {
	since time.Time
	res   *resident // nil for a conversation not seen before
	need  int64     // tokens of KV pool it needs
	// home and why are for whoever is watching: where this request's
	// conversation lives, and what it was waiting for at its last look.
	home, why string
}

// Held is one request the queue is holding, for the dashboard.
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

// NewQueue returns a queue with the given grace and longest wait.
func NewQueue(grace, maxWait time.Duration) *Queue {
	return &Queue{Grace: grace, MaxWait: maxWait, freed: map[string][]time.Time{}, pending: map[string]int{}, residents: map[[32]byte]*resident{},
		wake: make(chan struct{}), now: time.Now, tick: 250 * time.Millisecond}
}

// Waiting reports how many requests are held right now.
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

// signal wakes every waiting request to look again. q.mu is held.
func (q *Queue) signal() {
	close(q.wake)
	q.wake = make(chan struct{})
}

// free is how many of c's slots are open, counting requests already let
// through to it. An engine that does not report its slots cannot be asked and
// is never held against: it reads as having room. q.mu is held.
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

// lookup finds the conversation a prompt continues, from its deepest block
// down. q.mu is held.
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
	// Conversations that ended are forgotten once they are well past mattering.
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

// first reports whether w is the request that has waited longest, or nothing
// is waiting. q.mu is held.
func (q *Queue) first(w *waiter) bool {
	return len(q.waiting) == 0 || q.waiting[0] == w
}

// crowdedBy is how much faster an engine has to read than a conversation's
// home before the conversation leaves a home it is sharing for a slot there
// that nobody owns. Lower than placement's three times, which is for leaving
// a home it has to itself: here staying means writing beside others' reads.
const crowdedBy = 1.5

// leave says whether a request whose home has room should go elsewhere
// anyway, and where. q.mu is held.
//
// Two reasons. Placement's own: an engine several times faster with a slot
// (it put that engine first). And a crowded home beside an engine with a slot
// nobody owns: in the run of 2026-10-06 the last four tasks, all at home on
// the four-slot GPU, shared it for the final half hour while the 5090 stood
// idle, because the 5090 reads 2.4 times as fast and not three. Each cold
// read there slowed the other three to a tenth of their writing speed. The
// move costs one re-read on the faster engine, and it is made only where the
// request then has fewer neighbours than it has at home.
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
	// Home with a slot free, wherever placement put home in the order. The
	// slot it takes back is the newest one freed there: its own, 0.2 seconds
	// ago.
	//
	// "Wherever" is a fault found in the first live run of this queue. Placement
	// moves a conversation off a slow engine when one three times faster has a
	// slot free, so home was not always first; a request whose home was not
	// first was treated as having none, waited out the grace beside its own
	// free slot, and then took someone else's. 22 of that run's 29 moves were
	// between the two GPUs, most from an engine that had its slot open.
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
		// Leaving home is still allowed, on the same terms as anyone taking a
		// slot that is not theirs.
		if to, ok := q.leave(ordered, c, w); ok {
			return toFront(ordered, to), true
		}
		if ts := q.fresh(home); len(ts) > 0 {
			q.freed[home] = ts[:len(ts)-1]
		}
		return toFront(ordered, home), true
	}
	// Anything else takes a slot that is not this conversation's, and only
	// the request that has waited longest may.
	if q.first(w) {
		for _, c := range ordered {
			if q.settled(c) && q.fits(c, w) {
				return toFront(ordered, c.Name), true
			}
		}
	}
	// A conversation with a slot at home but no room for it anywhere. One too
	// large for any engine's pool has nothing to wait for and goes at once;
	// otherwise it waits for room, and when it has waited long enough it goes
	// where it costs the others least, not simply home.
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

// Admit returns the order to dispatch a request in, holding it first for as
// long as the rules above say, and a Grant to hand to Done when the request
// has ended. order is called each time the request looks, and returns
// placement's current order for it and its home engine. It is called with
// the queue's lock held, so that what it reads of the mesh is no older than
// the queue's own count of requests on their way (see pending); it must not
// block. waited is how long the request was held.
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
