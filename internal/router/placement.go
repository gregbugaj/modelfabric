package router

import (
	"fmt"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/gregbugaj/modelfabric/internal/mesh"
)

// Placement ranks destinations and tracks their prefix affinity and load.
// Forward and bench/routesim share it so replays exercise the serving policy.
type Placement struct {
	aff *affinity
	// HomeSlot selects the earlier rule, "home while it has a free slot,
	// otherwise the fewest in flight" (applyAffinity), in place of the
	// default, which is llm-d's (schedule).
	HomeSlot bool
	// NoRoomRule disables conversation residency filtering. Per-engine rate
	// weighting and deterministic tie-breaking still apply.
	NoRoomRule bool

	mu sync.Mutex
	// reading is, per engine, the prompt tokens of requests placed there and
	// not yet prefilled, less what the engine already held of each: what it
	// still has to read. It is the load placement balances on.
	reading map[string]int64
	// coldOrder is the engines in the order they were last given a prompt
	// cached nowhere, oldest first.
	coldOrder []string
	// homes is the conversations placed, by the hash of the last block of
	// their latest prompt, with the engine each lives on. See room.
	homes    map[[32]byte]*conversation
	attempts map[*Attempt]struct{}
	sequence uint64
}

type conversation struct {
	leaf              [32]byte
	engine            string
	inflight          int
	last              time.Time // when its latest successful request ended
	confirmedEngine   string
	confirmedLeaf     [32]byte
	confirmedSequence uint64
}

// residentFor is how long after its last request a conversation still counts
// as living on its engine. An agent asks again within a second or two; one
// that has been quiet this long has finished or is far from its next call.
const residentFor = 30 * time.Second

// NewPlacement returns a Placement that tells time by clock; nil means the
// wall clock.
func NewPlacement(clock func() time.Time) *Placement {
	a := newAffinity(1<<18, 10*time.Minute)
	if clock != nil {
		a.now = clock
	}
	return &Placement{aff: a, reading: map[string]int64{}, homes: map[[32]byte]*conversation{}, attempts: map[*Attempt]struct{}{}}
}

// A Choice is one request's placement: the candidates in the order to try
// them, and what Placed needs to record it.
type Choice struct {
	Candidates []mesh.Candidate
	// Target is the engine the prompt was last seen on and worth following,
	// "" when there was none.
	Target string
	blocks [][32]byte
	// shares is how much of the prompt each engine holds, 0 to 1, for those
	// that hold any, and tokens the prompt's size: the conversation's size
	// as the engine last reported it, or a quarter of the request's length
	// for one not seen before.
	shares map[string]float64
	tokens int64
	cold   bool
	conv   *conversation // nil for a conversation not placed before
}

// Order ranks candidates for a request, given its model, body and the
// operator's preferred node. Candidates must already be in the mesh's order
// (mesh.Rank).
func (p *Placement) Order(cands []mesh.Candidate, model string, body []byte, preferred string) Choice {
	blocks := prefixBlocks(model, body)
	if len(blocks) == 0 {
		return Choice{Candidates: cands}
	}
	target, matched, continuing := p.aff.find(blocks)
	held := p.aff.heldBy(blocks)
	// Pending placements are hints, not confirmed cache entries. Keeping them
	// separate lets one rejected attempt disappear without erasing a concurrent
	// request or a prefix this engine successfully served earlier.
	p.mu.Lock()
	var newest uint64
	for a := range p.attempts {
		if a.forgotten || a.prefilled {
			continue
		}
		n := sort.Search(min(len(blocks), len(a.choice.blocks)), func(i int) bool { return blocks[i] != a.choice.blocks[i] })
		if n < affinityMinBlocks {
			continue
		}
		held[a.name] = max(held[a.name], n)
		if n > matched || (n == matched && a.sequence > newest) {
			target, matched, continuing = a.name, n, n == len(a.choice.blocks)
			newest = a.sequence
		}
	}
	p.mu.Unlock()
	if p.HomeSlot {
		// A short shared system prompt must not pull unrelated conversations
		// onto the same engine.
		if !continuing && matched*2 < len(blocks) {
			target = ""
		}
		return Choice{Candidates: applyAffinity(cands, target, preferred), Target: target, blocks: blocks}
	}
	c := Choice{blocks: blocks, tokens: int64(len(body) / 4), shares: map[string]float64{}}
	deepest := 0
	for name, n := range held {
		c.shares[name] = float64(n) / float64(len(blocks))
		if n > deepest {
			deepest = n
		}
	}
	if deepest > 0 {
		// The size the engine reported for this conversation's last request,
		// where there is one: exact, where the length of a body is a guess.
		if n := p.aff.size(blocks[deepest-1]); n > 0 {
			held := float64(deepest) / float64(len(blocks))
			c.tokens = max(n, int64(float64(n)/held))
		}
	}
	c.cold = len(c.shares) == 0
	p.mu.Lock()
	for i := len(blocks) - 1; i >= 0 && c.conv == nil; i-- {
		c.conv = p.homes[blocks[i]]
	}
	p.mu.Unlock()
	c.Candidates = p.schedule(cands, c.shares, c.tokens, preferred, c.conv)
	// Target is what the router reports as "placed by affinity": the engine
	// chosen, when it held enough of the prompt to keep the request.
	if len(c.Candidates) > 0 && c.shares[c.Candidates[0].Name] >= stickyShare {
		c.Target = c.Candidates[0].Name
	} else {
		// Or the engine that would have kept it, had it gone there.
		for name, sh := range c.shares {
			if sh >= stickyShare && (c.Target == "" || sh > c.shares[c.Target]) {
				c.Target = name
			}
		}
	}
	return c
}

// Placed reserves a dispatch attempt before sending it, so requests arriving
// during prefill can follow its prefix. Complete the returned attempt with
// Finished or Failed; nil means there was no prompt to track.
func (p *Placement) Placed(c Choice, name string) *Attempt {
	if len(c.blocks) == 0 {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sequence++
	a := &Attempt{placement: p, choice: c, name: name, sequence: p.sequence}
	p.attempts[a] = struct{}{}
	if !p.HomeSlot {
		a.reading = c.cost(name)
		p.reading[name] += a.reading
		if c.cold {
			p.tookCold(name)
		}
		a.conv = p.moveIn(c, name)
	}
	return a
}

// Forget invalidates cache hints, residents and cold-LRU history for a stopped
// or replaced engine. Reloading clears KV cache even when its name is unchanged.
func (p *Placement) Forget(name string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.aff.forget(name)
	for a := range p.attempts {
		if a.conv != nil && a.conv.confirmedEngine == name {
			a.conv.confirmedEngine = ""
		}
		if a.name == name {
			a.forgotten = true
			a.releaseReading()
		}
	}
	for k, v := range p.homes {
		// A migration may still be speculative. Its fallback disappears too
		// when the old engine reloads, even if the new attempt later fails.
		if v.confirmedEngine == name {
			v.confirmedEngine = ""
		}
		if v.engine == name {
			delete(p.homes, k)
		}
	}
	p.coldOrder = slices.DeleteFunc(p.coldOrder, func(n string) bool { return n == name })
}

// moveIn records that the conversation c continues is now on the named
// engine, with a request running. p.mu is held.
func (p *Placement) moveIn(c Choice, name string) *conversation {
	leaf := c.blocks[len(c.blocks)-1]
	v := c.conv
	if v != nil && p.homes[v.leaf] != v {
		v = nil
	}
	if v == nil {
		// Placed twice for one request (a retry on the next candidate), or a
		// second request of a conversation still on its first: find it.
		v = p.homes[leaf]
	}
	if v == nil {
		v = &conversation{}
	} else {
		delete(p.homes, v.leaf)
	}
	v.leaf, v.engine = leaf, name
	v.inflight++
	p.homes[leaf] = v
	now := p.aff.now()
	for k, x := range p.homes {
		if x.inflight <= 0 && now.Sub(x.last) > 10*time.Minute {
			delete(p.homes, k)
		}
	}
	return v
}

func (c Choice) cost(name string) int64 {
	return uncached(c.shares[name], c.tokens)
}

// Reading reports, per engine, the prompt tokens placed there and not yet
// read: the load placement balances on. For tools.
func (p *Placement) Reading() map[string]int64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make(map[string]int64, len(p.reading))
	for k, v := range p.reading {
		out[k] = v
	}
	return out
}

// Explain formats the token estimate and cache shares for replay diagnostics.
func (c Choice) Explain() string {
	return fmt.Sprintf("tokens %d cold %v shares %v", c.tokens, c.cold, c.shares)
}
