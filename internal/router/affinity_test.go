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

// The case the first version of this got wrong: a ~3KB document followed by a
// different question each time. A single hash over a fixed window included the
// varying question, so no two requests ever matched.
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

func TestApplyAffinityYieldsToLoad(t *testing.T) {
	cs := cands("a", "b")
	cs[1].Score = cs[0].Score + AffinitySlack + 1
	if got := order(applyAffinity(cs, "b", "")); got != "ab" {
		t.Fatalf("a warm but much busier candidate must not win, got %s", got)
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
// 2-slot engine) and the other has room. The in-flight gap (3 vs 1) is within
// AffinitySlack, but the slot rule must still refuse the warm engine.
func TestApplyAffinityRefusesFullWarmEngine(t *testing.T) {
	cs := []mesh.Candidate{
		{Name: "idle", Score: 1, Inflight: 1, Slots: 2},
		{Name: "warm", Score: 3, Inflight: 3, Slots: 2},
	}
	if got := order(applyAffinity(cs, "warm", "")); got != "idlewarm" {
		t.Fatalf("a full warm engine must not beat one with room, got %s", got)
	}
	// With a free slot the warm engine still wins.
	cs[1].Inflight, cs[1].Score = 1, 1
	if got := order(applyAffinity(cs, "warm", "")); got != "warmidle" {
		t.Fatalf("a warm engine with room should lead, got %s", got)
	}
	// When everything is full, warmth decides again (queueing somewhere is
	// unavoidable, and there it at least finds its cache).
	cs[0].Inflight, cs[1].Inflight = 2, 2
	if got := order(applyAffinity(cs, "warm", "")); got != "warmidle" {
		t.Fatalf("with every engine full the warm one should lead, got %s", got)
	}
}

// Affinity and the disk cache share one hashing, which only works if the
// longer chain begins with the shorter one.
func TestPrefixChainExtendsAffinityBlocks(t *testing.T) {
	body := []byte(`{"model":"m","messages":[{"role":"user","content":"` + strings.Repeat("abcdefgh", 20000) + `"}]}`)
	short := prefixBlocks("m", body)
	long := prefixchain.Chain("m", body, prefixchain.ColdMaxPrefix)
	if len(long) <= len(short) {
		t.Fatalf("long chain has %d blocks, affinity's has %d", len(long), len(short))
	}
	for i := range short {
		if short[i] != long[i] {
			t.Fatalf("block %d differs between the two chains", i)
		}
	}
}
