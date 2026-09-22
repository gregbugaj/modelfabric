package router

import (
	"container/list"
	"sync"
	"time"

	"github.com/gregbugaj/modelfabric/internal/mesh"
	"github.com/gregbugaj/modelfabric/internal/prefixchain"
)

// Prefix affinity: send a request to the engine that most recently served the
// same prompt prefix, so its KV cache is still warm.
//
// A cache hit is worth a lot. Measured on the RTX 5090 with a ~2.9k-token
// prompt: Qwen3.8-27B falls from 1399 ms to 241 ms time-to-first-token when
// the prefix is cached. Plain least-outstanding routing scatters requests that
// share a system prompt or reference document across replicas, and each one
// pays the cold prefill again.
//
// This is the same idea as llm-d's approximate prefix-cache scorer, kept in the
// data path without Envoy or a separate scheduler: the router remembers which
// candidate served each prefix, and prefers it unless it is materially busier.
// It is a preference, never a pin — load always gets a veto.

// The prompt is hashed in fixed-size blocks, each chained to the one before,
// so a block's hash identifies the entire prefix up to and including it. Two
// requests that share a 3KB document and differ only in the question after it
// share every block covering the document, and diverge only after. Matching
// the longest shared chain is what makes affinity work for the case that
// matters — the same long context with a different ending. A single hash over
// a fixed window would not: the window would include the varying part.
const (
	affinityBlock     = prefixchain.Block // bytes per block
	affinityMaxPrefix = 64 << 10          // stop hashing after this much prompt
	affinityMinBlocks = 2                 // shorter shared prefixes are not worth a detour
)

// AffinitySlack is how many more in-flight requests the warm candidate may
// carry than the least-loaded one and still be chosen. Beyond that, queueing
// costs more than the cached prefill saves.
//
// It is the veto for engines whose capacity is unknown. When slots are known
// the stricter rule applies first: a warm candidate with no free slot never
// wins over one with room. On llama.cpp a request beyond the slots waits for
// one and then evicts another conversation from the KV pool, so for long
// agent conversations "slightly busier" is already a cache miss for someone
// else. The SWE-bench runs piled three 90k-token conversations onto one
// 2-slot engine (3 vs 1 in flight, within this slack) while the other idled.
const AffinitySlack = 2

type affinityEntry struct {
	key    [32]byte // one block of a chain
	target string   // candidate name
	seen   time.Time
}

// affinity is a bounded LRU from prompt-prefix hash to candidate.
type affinity struct {
	mu    sync.Mutex
	max   int
	ttl   time.Duration
	order *list.List
	items map[[32]byte]*list.Element
}

func newAffinity(max int, ttl time.Duration) *affinity {
	return &affinity{max: max, ttl: ttl, order: list.New(), items: map[[32]byte]*list.Element{}}
}

func (a *affinity) get(k [32]byte) (string, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	el, ok := a.items[k]
	if !ok {
		return "", false
	}
	e := el.Value.(*affinityEntry)
	if time.Since(e.seen) > a.ttl {
		// Engines evict too; an old memory of a prefix is a guess, not a hit.
		a.order.Remove(el)
		delete(a.items, k)
		return "", false
	}
	return e.target, true
}

func (a *affinity) put(k [32]byte, target string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if el, ok := a.items[k]; ok {
		e := el.Value.(*affinityEntry)
		e.target, e.seen = target, time.Now()
		a.order.MoveToFront(el)
		return
	}
	a.items[k] = a.order.PushFront(&affinityEntry{key: k, target: target, seen: time.Now()})
	for a.order.Len() > a.max {
		old := a.order.Back()
		a.order.Remove(old)
		delete(a.items, old.Value.(*affinityEntry).key)
	}
}

// prefixBlocks returns the chained block hashes identifying a request's prompt,
// scoped by model. It understands the OpenAI chat, completions, Responses and
// Anthropic shapes; anything else yields no blocks, which means no affinity.
func prefixBlocks(model string, body []byte) [][32]byte {
	return prefixchain.Chain(model, body, affinityMaxPrefix)
}

// lookup returns the candidate that served the longest shared prefix, and how
// many blocks matched. Walking from the deepest block down finds the longest
// match first.
func (a *affinity) lookup(blocks [][32]byte) (string, int) {
	for i := len(blocks) - 1; i >= affinityMinBlocks-1; i-- {
		if target, ok := a.get(blocks[i]); ok {
			return target, i + 1
		}
	}
	return "", 0
}

// record remembers that target now holds this whole prefix.
func (a *affinity) record(blocks [][32]byte, target string) {
	for _, k := range blocks {
		a.put(k, target)
	}
}

// applyAffinity moves the warm candidate to the front when it is not
// materially busier than the best one. It never reorders across the preferred
// node boundary: that is an operator's explicit choice and outranks cache
// warmth.
func applyAffinity(cands []mesh.Candidate, target, preferred string) []mesh.Candidate {
	if len(cands) < 2 || target == "" {
		return cands
	}
	j := -1
	for i, c := range cands {
		if c.Name == target {
			j = i
			break
		}
	}
	if j <= 0 {
		return cands // absent, or already first
	}
	if preferred != "" && cands[0].Node == preferred && cands[j].Node != preferred {
		return cands
	}
	if cands[j].Full() && !cands[0].Full() {
		return cands // warm, but a request there would queue while cands[0] has room
	}
	if cands[j].Score > cands[0].Score+AffinitySlack {
		return cands // warm, but busy enough that queueing would cost more
	}
	out := make([]mesh.Candidate, 0, len(cands))
	out = append(out, cands[j])
	out = append(out, cands[:j]...)
	return append(out, cands[j+1:]...)
}
