package router

import (
	"strings"
	"testing"
	"time"

	"github.com/gregbugaj/modelfabric/internal/mesh"
	"github.com/gregbugaj/modelfabric/internal/prefixchain"
)

func cands(names ...string) []mesh.Candidate {
	out := make([]mesh.Candidate, len(names))
	for i, n := range names {
		out[i] = mesh.Candidate{Name: n, Node: "n", Score: float64(i)}
	}
	return out
}

func order(cs []mesh.Candidate) string {
	s := ""
	for _, c := range cs {
		s += c.Name
	}
	return s
}

func chat(system, user string) []byte {
	return []byte(`{"messages":[{"role":"system","content":"` + system + `"},{"role":"user","content":"` + user + `"}]}`)
}

func doc(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = 'a' + byte(i%26)
	}
	return string(b)
}

// A fixed-window hash included changing questions after a shared document,
// preventing prefix matches. Block hashes must preserve the shared prefix.
func TestSharedDocumentMatchesDespiteDifferentQuestions(t *testing.T) {
	d := doc(3000)
	a := newAffinity(1024, time.Hour)
	a.record(prefixBlocks("m", chat(d, "first question")), "replica-2")

	target, depth := a.lookup(prefixBlocks("m", chat(d, "an entirely different question")))
	if target != "replica-2" {
		t.Fatalf("shared document did not match (target %q)", target)
	}
	if depth < 3000/affinityBlock-1 {
		t.Fatalf("matched %d blocks; the whole document should match", depth)
	}
}

func TestLongestPrefixWins(t *testing.T) {
	a := newAffinity(1024, time.Hour)
	short, long := doc(1000), doc(4000)
	a.record(prefixBlocks("m", chat(short, "x")), "has-short")
	a.record(prefixBlocks("m", chat(long, "x")), "has-long")
	if target, _ := a.lookup(prefixBlocks("m", chat(long, "y"))); target != "has-long" {
		t.Fatalf("the replica holding more of the prefix should win, got %q", target)
	}
}

func TestPrefixBlocksAreModelScoped(t *testing.T) {
	d := doc(2000)
	a := newAffinity(1024, time.Hour)
	a.record(prefixBlocks("model-a", chat(d, "q")), "r1")
	if target, _ := a.lookup(prefixBlocks("model-b", chat(d, "q"))); target != "" {
		t.Fatal("the same prompt on a different model is a different cache")
	}
}

func TestShortOrMissingPromptsHaveNoAffinity(t *testing.T) {
	if b := prefixBlocks("m", []byte(`{"model":"m"}`)); len(b) != 0 {
		t.Fatal("a body with no prompt has nothing to be affine to")
	}
	a := newAffinity(16, time.Hour)
	tiny := prefixBlocks("m", chat("hi", "there"))
	a.record(tiny, "r1")
	if target, _ := a.lookup(tiny); target != "" {
		t.Fatal("a prompt shorter than the minimum shared prefix should not steer routing")
	}
}

func TestApplyAffinityPrefersWarmWithinSlack(t *testing.T) {
	cs := cands("a", "b", "c") // scores 0,1,2
	if got := order(applyAffinity(cs, "c", "")); got != "cab" {
		t.Fatalf("warm c within slack should lead, got %s", got)
	}
	if got := order(applyAffinity(cs, "zz", "")); got != "abc" {
		t.Fatalf("an unknown target must not reorder, got %s", got)
	}
}

// Regression: AffinitySlack is a request count, not a rate-weighted score.
// Apply it to in-flight counts only when engine capacity is unknown.
func TestApplyAffinityYieldsToLoad(t *testing.T) {
	cs := []mesh.Candidate{
		{Name: "a", Local: true, Inflight: 0},
		{Name: "b", Local: true, Inflight: AffinitySlack + 1},
	}
	if got := order(applyAffinity(cs, "b", "")); got != "ab" {
		t.Fatalf("a warm but much busier candidate must not win, got %s", got)
	}
	cs[1].Inflight = AffinitySlack
	if got := order(applyAffinity(cs, "b", "")); got != "ba" {
		t.Fatalf("a warm candidate within the slack should lead, got %s", got)
	}
}

func TestApplyAffinityRespectsPreferredNode(t *testing.T) {
	cs := []mesh.Candidate{
		{Name: "pref", Node: "predator", Score: 0},
		{Name: "warm", Node: "xpredator", Score: 0},
	}
	if got := order(applyAffinity(cs, "warm", "predator")); got != "prefwarm" {
		t.Fatalf("cache warmth must not override an operator's preferred node, got %s", got)
	}
}

func TestAffinityLRUBoundsAndExpiresEntries(t *testing.T) {
	a := newAffinity(2, time.Hour)
	k := func(b byte) [32]byte { return [32]byte{b} }
	a.put(k(1), "x")
	a.put(k(2), "y")
	a.put(k(3), "z")
	if _, ok := a.get(k(1)); ok {
		t.Fatal("oldest entry should be evicted past capacity")
	}
	if v, _ := a.get(k(3)); v != "z" {
		t.Fatal("newest entry missing")
	}
	short := newAffinity(4, time.Nanosecond)
	short.put(k(9), "x")
	time.Sleep(time.Millisecond)
	if _, ok := short.get(k(9)); ok {
		t.Fatal("expired entries must not be trusted")
	}
}

// The SWE-bench pile-up: the warm engine is full (2 running + 1 queued on a
// 2-slot engine) and the other has room. The slot rule refuses the warm engine.
func TestApplyAffinityRefusesFullWarmEngine(t *testing.T) {
	cs := []mesh.Candidate{
		{Name: "idle", Local: true, Score: 1, Inflight: 1, Slots: 2},
		{Name: "warm", Local: true, Score: 3, Inflight: 3, Slots: 2},
	}
	if got := order(applyAffinity(cs, "warm", "")); got != "idlewarm" {
		t.Fatalf("a full warm engine must not beat one with room, got %s", got)
	}
	cs[1].Inflight, cs[1].Score = 1, 1
	if got := order(applyAffinity(cs, "warm", "")); got != "warmidle" {
		t.Fatalf("a warm engine with room should lead, got %s", got)
	}
	// A full engine may have evicted this conversation. Rank by in-flight
	// count rather than queueing there on the assumption its cache survives.
	cs[0].Inflight, cs[1].Inflight = 2, 3
	if got := order(applyAffinity(cs, "warm", "")); got != "idlewarm" {
		t.Fatalf("with every engine full the less loaded one should lead, got %s", got)
	}
}

// Affinity now covers the full prompt: truncating at 64 KiB erased the cost
// of later turns. It must still agree with the disk cache over their overlap.
func TestPrefixChainExtendsAffinityBlocks(t *testing.T) {
	body := []byte(`{"model":"m","messages":[{"role":"user","content":"` + strings.Repeat("abcdefgh", 20000) + `"}]}`)
	short := prefixBlocks("m", body)
	long := prefixchain.Chain("m", body, prefixchain.ColdMaxPrefix)
	if len(long) != len(short) {
		t.Fatalf("disk chain has %d blocks, affinity's has %d", len(long), len(short))
	}
	for i := range short {
		if short[i] != long[i] {
			t.Fatalf("block %d differs between the two chains", i)
		}
	}
}

// A new SWE-bench task shares its system prompt with every other, so its chain
// matches the blocks covering it. That is not the conversation continuing, and
// treating it as one sent each new task to wherever the last one went.
func TestFindTellsAConversationFromASharedPrefix(t *testing.T) {
	system := doc(4000)
	a := newAffinity(1024, time.Hour)
	a.record(prefixBlocks("m", chat(system, "task one: "+doc(1500))), "minion")

	turn2 := prefixBlocks("m", chat(system, "task one: "+doc(1500)+doc(900)))
	if target, _, continuing := a.find(turn2); target != "minion" || !continuing {
		t.Errorf("the same conversation, longer: got %q continuing=%v", target, continuing)
	}
	other := prefixBlocks("m", chat(system, "task two: "+doc(1500)))
	if target, _, continuing := a.find(other); target != "minion" || continuing {
		t.Errorf("another task with the same system prompt: got %q continuing=%v, want a shared prefix only", target, continuing)
	}
}

// Regression cases from live routing and routesim replay on a heterogeneous
// fleet. Candidates arrive fastest first, using measured prefill rates.
func TestPlacementOnTheBenchmarkFleet(t *testing.T) {
	fleet := func(x, m, h int64) []mesh.Candidate {
		return []mesh.Candidate{
			{Name: "xpredator", Local: true, Inflight: x, Slots: 2, PrefillTokS: 1761},
			{Name: "minion", Local: true, Inflight: m, Slots: 4, PrefillTokS: 840},
			{Name: "helion", Local: true, Inflight: h, Slots: 1, PrefillTokS: 264},
		}
	}
	for _, c := range []struct {
		name      string
		cands     []mesh.Candidate
		warm      string
		wantFirst string
	}{
		// Affinity slack uses request counts; rate-weighted scores prevented
		// conversations from returning to slower engines.
		{"home on a slower engine, two of four slots busy", fleet(0, 2, 0), "minion", "minion"},
		{"home on a slower engine, three of four busy", fleet(1, 3, 0), "minion", "minion"},
		// "Home is full, wait there": the slot it waits for is another
		// conversation's, and the engine stays one over, cold in every slot.
		{"home full: the fewest in flight, not home", fleet(2, 1, 1), "xpredator", "minion"},
		{"home full: not the fastest either, if it is busier", fleet(2, 4, 0), "minion", "helion"},
		// Everything full is where the conversation with no slot is placed.
		// On the fastest engine it made the 5090 the one that thrashed.
		{"everything full and level: the slower engine", fleet(4, 4, 4), "xpredator", "helion"},
		{"a new conversation on an idle fleet: the fastest", fleet(0, 0, 0), "", "xpredator"},
		{"a new conversation: the fewest in flight", fleet(1, 0, 1), "", "minion"},
		// An open slot may still hold another conversation's cache. Preferring
		// it here caused cascading evictions in live runs.
		{"home full: the fewest in flight, even past an open slot", fleet(2, 2, 1), "xpredator", "helion"},
		{"home full, no slot open anywhere: the fewest in flight", fleet(3, 4, 2), "minion", "helion"},
		// Do not cap each engine at one queued request: displacement can consume
		// another conversation's cached slot.
		{"home full: behind a request already waiting, if that is the fewest", fleet(2, 4, 2), "minion", "helion"},
		// Sticky placement alone left a lone conversation on whichever node
		// it began: 749 calls on the Apple Silicon node beside an idle 5090.
		{"home is several times slower than a free engine", fleet(0, 0, 0), "helion", "xpredator"},
		{"but the two GPUs do not trade conversations", fleet(0, 1, 0), "minion", "minion"},
		{"and a faster engine with no free slot is not worth leaving for", fleet(2, 4, 0), "helion", "helion"},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := applyAffinity(c.cands, c.warm, "")
			if got[0].Name != c.wantFirst {
				t.Errorf("first choice %s, want %s (order %s)", got[0].Name, c.wantFirst, order(got))
			}
		})
	}
}

// Only traffic measures an engine. A lone conversation is the only traffic,
// so it tries an unmeasured engine with a free slot once, and after that the
// rates decide.
func TestALoneConversationTriesAnUnmeasuredEngine(t *testing.T) {
	cs := []mesh.Candidate{
		{Name: "helion", Local: true, Slots: 1, PrefillTokS: 264},
		{Name: "minion", Local: true, Slots: 4},
		{Name: "xpredator", Local: true, Slots: 2},
	}
	if got := applyAffinity(cs, "helion", "")[0].Name; got != "minion" {
		t.Errorf("first choice %s, want an unmeasured engine", got)
	}
	// An engine that can never report a rate (mlx-lm) is not one to try for
	// the sake of measuring it.
	cs[1].RateUnmeasurable, cs[2].RateUnmeasurable = true, true
	if got := applyAffinity(cs, "helion", "")[0].Name; got != "helion" {
		t.Errorf("first choice %s, want home: the others can never be measured", got)
	}
	cs[0].PrefillTokS = 0
	cs[1].RateUnmeasurable, cs[2].RateUnmeasurable = false, false
	if got := applyAffinity(cs, "helion", "")[0].Name; got != "helion" {
		t.Errorf("first choice %s, want home", got)
	}
}
