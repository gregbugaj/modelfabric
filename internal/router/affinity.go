package router

import (
	"container/list"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/gregbugaj/modelfabric/internal/mesh"
	"github.com/gregbugaj/modelfabric/internal/prefixchain"
)

// Affinity tracks confirmed prompt prefixes independently of placement policy.
// Chained hashes identify the entire prefix through each block, so requests
// sharing a document can match even when their final questions differ.
const (
	affinityBlock     = prefixchain.Block // bytes per block
	affinityMinBlocks = 2                 // shorter shared prefixes are not worth a detour
)

// AffinitySlack is how many more in-flight requests the warm candidate may
// carry than the least-loaded one and still be chosen, for an engine that
// does not report its slots. Beyond that, queueing costs more than the cached
// prefill saves. An engine that does report them has an exact answer instead:
// a free slot, or none.
const AffinitySlack = 2

type affinityEntry struct {
	key    [32]byte // one block of a chain
	target string   // candidate name
	seen   time.Time
	// leaf says this block was the end of a request's chain: the whole of
	// some prompt, not just a part several prompts share. A later request
	// whose chain reaches a leaf is that conversation continuing. One that
	// only reaches an interior block shares a system prompt or a document
	// with it and is a different conversation.
	leaf bool
	// tokens is the conversation's size as the engine reported it for the
	// request this block ended, zero when it has not said.
	tokens int64
	// holders is every engine this block has been sent to, most recent
	// first. target is only the latest, and a block several conversations
	// share (a system prompt) is held by every engine that has served any of
	// them: see heldBy.
	holders []string
}

// maxHolders bounds how many engines are remembered for one block.
const maxHolders = 8

// affinity is a bounded LRU from prompt-prefix hash to candidate.
type affinity struct {
	mu    sync.Mutex
	max   int
	ttl   time.Duration
	order *list.List
	items map[[32]byte]*list.Element
	// now is the clock. time.Now, except in bench/routesim, which replays a
	// recorded run in virtual time and needs entries to expire on that clock.
	now func() time.Time
}

func newAffinity(max int, ttl time.Duration) *affinity {
	return &affinity{max: max, ttl: ttl, order: list.New(), items: map[[32]byte]*list.Element{}, now: time.Now}
}

func (a *affinity) get(k [32]byte) (string, bool) {
	target, _, ok := a.entry(k)
	return target, ok
}

// entry is get, and whether the block was the end of a request's chain.
func (a *affinity) entry(k [32]byte) (target string, leaf, ok bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	el, ok := a.items[k]
	if !ok {
		return "", false, false
	}
	e := el.Value.(*affinityEntry)
	if a.now().Sub(e.seen) > a.ttl {
		// Engines evict too; an old memory of a prefix is a guess, not a hit.
		a.order.Remove(el)
		delete(a.items, k)
		return "", false, false
	}
	return e.target, e.leaf, true
}

func (a *affinity) put(k [32]byte, target string) { a.putBlock(k, target, false) }

// size is the token count recorded for the request block k ended, or zero.
func (a *affinity) size(k [32]byte) int64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	if el, ok := a.items[k]; ok {
		return el.Value.(*affinityEntry).tokens
	}
	return 0
}

// setSize records the token count of the request block k ended.
func (a *affinity) setSize(k [32]byte, tokens int64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if el, ok := a.items[k]; ok {
		el.Value.(*affinityEntry).tokens = tokens
	}
}

func (a *affinity) putBlock(k [32]byte, target string, leaf bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if el, ok := a.items[k]; ok {
		e := el.Value.(*affinityEntry)
		e.target, e.seen, e.leaf = target, a.now(), leaf
		e.holders = holding(e.holders, target)
		a.order.MoveToFront(el)
		return
	}
	a.items[k] = a.order.PushFront(&affinityEntry{key: k, target: target, seen: a.now(), leaf: leaf, holders: []string{target}})
	for a.order.Len() > a.max {
		old := a.order.Back()
		a.order.Remove(old)
		delete(a.items, old.Value.(*affinityEntry).key)
	}
}

// forget drops name from every block it was recorded as holding, and the
// blocks nothing else holds.
func (a *affinity) forget(name string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for el := a.order.Front(); el != nil; {
		next := el.Next()
		e := el.Value.(*affinityEntry)
		if slices.Contains(e.holders, name) {
			e.holders = slices.DeleteFunc(e.holders, func(h string) bool { return h == name })
			if len(e.holders) == 0 {
				a.order.Remove(el)
				delete(a.items, e.key)
			} else if e.target == name {
				// The most recent of those left. It is no longer known to be
				// where a conversation's latest request ended.
				e.target, e.leaf = e.holders[0], false
			}
		}
		el = next
	}
}

// holding puts target first in holders, once, keeping at most maxHolders.
func holding(holders []string, target string) []string {
	out := make([]string, 0, min(len(holders)+1, maxHolders))
	out = append(out, target)
	for _, h := range holders {
		if h != target && len(out) < maxHolders {
			out = append(out, h)
		}
	}
	return out
}

// heldBy returns each holder's longest matching prefix in blocks. Tracking all
// holders prevents a shared system prompt from pulling new conversations toward
// whichever engine served it last.
func (a *affinity) heldBy(blocks [][32]byte) map[string]int {
	out := map[string]int{}
	a.mu.Lock()
	defer a.mu.Unlock()
	now := a.now()
	for i := len(blocks) - 1; i >= affinityMinBlocks-1 && i >= 0; i-- {
		el, ok := a.items[blocks[i]]
		if !ok {
			continue
		}
		e := el.Value.(*affinityEntry)
		if now.Sub(e.seen) > a.ttl {
			continue
		}
		for _, h := range e.holders {
			if _, seen := out[h]; !seen {
				out[h] = i + 1
			}
		}
	}
	return out
}

// prefixBlocks returns the chained block hashes identifying a request's prompt,
// scoped by model. It understands the OpenAI chat, completions, Responses and
// Anthropic shapes; anything else yields no blocks, which means no affinity.
func prefixBlocks(model string, body []byte) [][32]byte {
	// A capped chain identified a conversation, but mistook every byte after
	// 64 KiB for a cache hit and merged branches with different long suffixes.
	// Match the whole prompt; the affinity LRU still bounds retained state.
	return prefixchain.Chain(model, body, len(body))
}

// lookup returns the most recent holder of the longest matching prefix and
// its length in blocks.
func (a *affinity) lookup(blocks [][32]byte) (string, int) {
	target, n, _ := a.find(blocks)
	return target, n
}

// find also reports whether the match ended an earlier request. An interior
// block can be a shared system prompt, not a continuation of that conversation.
func (a *affinity) find(blocks [][32]byte) (target string, matched int, continuing bool) {
	for i := len(blocks) - 1; i >= affinityMinBlocks-1; i-- {
		if target, leaf, ok := a.entry(blocks[i]); ok {
			return target, i + 1, leaf
		}
	}
	return "", 0, false
}

// record remembers that target now holds this whole prefix.
func (a *affinity) record(blocks [][32]byte, target string) {
	last := len(blocks) - 1
	for i, k := range blocks {
		a.putBlock(k, target, i == last)
	}
}

// applyAffinity implements the legacy home-slot policy. It prefers a warm
// engine with room unless a much faster one is available; otherwise it ranks
// by request count. An operator's preferred node always takes precedence.
// Rate-weighted load concentrated displaced conversations on the fastest
// engine and thrashed its cache. See bench/swe/README.md, "Routing experiments".
func applyAffinity(cands []mesh.Candidate, target, preferred string) []mesh.Candidate {
	if len(cands) < 2 {
		return cands
	}
	// pinned: the mesh put the operator's preferred node first. Nothing below
	// may move another node ahead of it.
	pinned := preferred != "" && cands[0].Node == preferred
	for _, warm := range cands {
		if warm.Name != target || target == "" {
			continue
		}
		if pinned && warm.Node != preferred {
			return cands
		}
		home := !warm.Full()
		if warm.Slots == 0 {
			// Capacity unknown, so "full" cannot be asked. The slack instead,
			// in requests in flight against the least-loaded candidate.
			least := cands[0].Load()
			for _, c := range cands[1:] {
				least = min(least, c.Load())
			}
			home = warm.Load() <= least+AffinitySlack
		}
		if home {
			// Let a conversation escape a slow initial home once rates are known.
			if fast, ok := muchFaster(cands, warm); ok {
				return toFront(cands, fast)
			}
			return toFront(cands, target)
		}
	}
	if pinned {
		return cands
	}
	// Preserve mesh order for ties unless both engines are full.
	out := append([]mesh.Candidate(nil), cands...)
	sort.SliceStable(out, func(a, b int) bool {
		// Favoring free slots evicted conversations between turns and caused
		// cascading cache misses. Compare request counts even across queues.
		if out[a].Load() != out[b].Load() {
			return out[a].Load() < out[b].Load()
		}
		// When both are full, displace the slower engine's cache so the faster
		// engine can keep serving its existing conversations.
		if out[a].Full() && out[b].Full() {
			return out[a].PrefillTokS < out[b].PrefillTokS
		}
		return false
	})
	return out
}

// A large speed gap justifies rebuilding a warm prompt elsewhere; small
// differences made conversations oscillate between the two GPUs.
const fasterBy = 3.0

// muchFaster chooses an available unmeasured engine to probe, or the fastest
// available engine at least fasterBy times as fast as the measured home.
func muchFaster(cands []mesh.Candidate, home mesh.Candidate) (string, bool) {
	if home.PrefillTokS <= 0 {
		return "", false
	}
	best, rate := "", home.PrefillTokS*fasterBy
	for _, c := range cands {
		if c.Name == home.Name || c.Full() {
			continue
		}
		// Probe an unmeasured engine: without traffic, its rate stays unknown
		// and a lone conversation could remain on a slower home indefinitely.
		if c.PrefillTokS == 0 && !c.RateUnmeasurable {
			return c.Name, true
		}
		if c.PrefillTokS >= rate {
			best, rate = c.Name, c.PrefillTokS
		}
	}
	return best, best != ""
}

// toFront moves the named candidate first, keeping the rest in order.
func toFront(cands []mesh.Candidate, name string) []mesh.Candidate {
	for j, c := range cands {
		if c.Name == name {
			if j == 0 {
				return cands
			}
			out := make([]mesh.Candidate, 0, len(cands))
			out = append(out, cands[j])
			out = append(out, cands[:j]...)
			return append(out, cands[j+1:]...)
		}
	}
	return cands
}
