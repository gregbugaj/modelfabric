package router

import (
	"sort"

	"github.com/gregbugaj/modelfabric/internal/mesh"
)

// Default placement follows llm-d-router v0.10.0's tuned scheduling stages:
// queue filter, prefix affinity, token-load and cold-LRU scoring, then max score.
// ModelFabric adds conversation residency, weights load by each engine's prefill
// rate, and preserves mesh order for ties. Burst co-location is omitted; pending
// placements provide affinity hints without a batching window.
// Benchmark history: bench/swe/README.md, "Routing experiments".
const (
	// stickyShare is how much of a prompt an engine must hold to keep the
	// request (llm-d's affinityThreshold).
	stickyShare = 0.80
	// maxPenaltySeconds is how much longer the first token may take on the
	// engine holding the prompt than on the best other before the request is
	// free to go elsewhere (llm-d's maxTTFTPenaltyMs).
	maxPenaltySeconds = 18.0
	// tokenThreshold is the in-flight token count at which an engine's token
	// load score reaches zero (llm-d's queueThresholdTokens: 128 requests of
	// 32K).
	tokenThreshold = 4194304

	tokenLoadWeight = 2.0
	coldLRUWeight   = 1.0
	// fallbackRate stands in for an engine whose prefill rate has not been
	// measured, when no other engine's has either.
	fallbackRate = 1000.0
)

// waitingFor reports whether c already has a request queued for a slot. An
// engine that does not report its slots has no queue that can be counted.
func waitingFor(c mesh.Candidate) bool {
	return c.Slots > 0 && c.Load() > c.Slots
}

// schedule orders candidates for a request. shares is how much of its prompt
// each engine holds, 0 to 1, for those that hold any. tokens is the prompt's
// size. It returns every candidate:
// the choice first, then the others it considered by score, then those it
// filtered out, so that a failed dispatch still has somewhere to go.
func (p *Placement) schedule(cands []mesh.Candidate, shares map[string]float64, tokens int64, preferred string, self *conversation) []mesh.Candidate {
	if len(cands) < 2 {
		return cands
	}
	// The operator's preferred node, when the mesh put it first, is an
	// explicit choice and outranks everything here.
	if preferred != "" && cands[0].Node == preferred {
		return cands
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	// 1. room
	kept := make([]mesh.Candidate, 0, len(cands))
	for _, c := range cands {
		if !waitingFor(c) {
			kept = append(kept, c)
		}
	}
	if len(kept) == 0 {
		kept = append(kept, cands...)
	}

	// Idle slots may still hold conversations between turns. Counting only
	// running requests repeatedly evicted those caches under oversubscription.
	// Exclude this conversation when testing whether its home has room for it.
	living := map[string]int64{}
	now := p.aff.now()
	for _, v := range p.homes {
		if v != self && (v.inflight > 0 || now.Sub(v.last) <= residentFor) {
			living[v.engine]++
		}
	}
	roomy := func(c mesh.Candidate) bool { return c.Slots <= 0 || living[c.Name] < c.Slots }
	var withRoom []mesh.Candidate
	for _, c := range kept {
		if roomy(c) {
			withRoom = append(withRoom, c)
		}
	}
	homeHasRoom := false
	for _, c := range withRoom {
		if shares[c.Name] >= stickyShare {
			homeHasRoom = true
		}
	}
	if len(withRoom) > 0 && !homeHasRoom && !p.NoRoomRule {
		kept = withRoom
	}

	// 2. affinity
	peak := 0.0
	for _, c := range cands {
		peak = max(peak, c.PrefillTokS)
	}
	if peak <= 0 {
		peak = fallbackRate
	}
	wait := func(c mesh.Candidate) float64 {
		rate := c.PrefillTokS
		if rate <= 0 {
			rate = peak
		}
		return float64(p.reading[c.Name]) / rate
	}
	var sticky, loose []mesh.Candidate
	for _, c := range kept {
		if shares[c.Name] >= stickyShare {
			sticky = append(sticky, c)
		} else {
			loose = append(loose, c)
		}
	}
	best := func(cs []mesh.Candidate) float64 {
		b := wait(cs[0])
		for _, c := range cs[1:] {
			b = min(b, wait(c))
		}
		return b
	}
	if len(sticky) > 0 && (len(loose) == 0 || best(sticky)-best(loose) <= maxPenaltySeconds) {
		kept = sticky
	}

	// 3. score. Cold means no engine left holds any of the prompt.
	cold := true
	for _, c := range kept {
		if shares[c.Name] > 0 {
			cold = false
		}
	}
	rank := p.coldRank(kept)
	score := make(map[string]float64, len(kept))
	for _, c := range kept {
		// Normalize by prefill time so equal token counts on different GPUs
		// do not imply equal work. Express the result in peak-rate tokens.
		rate := c.PrefillTokS
		if rate <= 0 {
			rate = peak
		}
		load := float64(p.reading[c.Name]+uncached(shares[c.Name], tokens)) / rate * peak
		s := tokenLoadWeight * (1 - min(load, tokenThreshold)/tokenThreshold)
		if cold {
			s += coldLRUWeight * rank[c.Name]
		} else {
			s += coldLRUWeight * 0.5
		}
		score[c.Name] = s
	}

	// 4. highest first; the mesh's order decides between equals.
	sort.SliceStable(kept, func(a, b int) bool { return score[kept[a].Name] > score[kept[b].Name] })
	out := append([]mesh.Candidate(nil), kept...)
	for _, c := range cands {
		if _, ok := score[c.Name]; !ok {
			out = append(out, c)
		}
	}
	return out
}

func uncached(share float64, tokens int64) int64 {
	return tokens - int64(share*float64(tokens))
}

// coldRank scores engines for a prompt cached nowhere: 1 for the engine least
// recently given one, down to 0 for the most recent, with engines never given
// one ahead of all (llm-d's no-hit-lru-scorer). p.mu is held.
func (p *Placement) coldRank(cands []mesh.Candidate) map[string]float64 {
	out := make(map[string]float64, len(cands))
	if len(cands) == 1 {
		out[cands[0].Name] = 1
		return out
	}
	in := map[string]bool{}
	for _, c := range cands {
		in[c.Name] = true
	}
	pos := map[string]int{}
	n := 0
	for _, name := range p.coldOrder { // oldest first
		if in[name] {
			pos[name] = n
			n++
		}
	}
	never := 0
	for _, c := range cands {
		if _, used := pos[c.Name]; !used {
			out[c.Name] = 1 - float64(never)/float64(len(cands)-1)
			never++
		}
	}
	for name, i := range pos {
		out[name] = max(1-float64(never+i)/float64(len(cands)-1), 0)
	}
	return out
}

// tookCold records that the named engine was just given a prompt cached
// nowhere. p.mu is held.
func (p *Placement) tookCold(name string) {
	for i, n := range p.coldOrder {
		if n == name {
			p.coldOrder = append(p.coldOrder[:i], p.coldOrder[i+1:]...)
			break
		}
	}
	p.coldOrder = append(p.coldOrder, name)
}
