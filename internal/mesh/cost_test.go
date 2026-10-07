package mesh

import (
	"math"
	"testing"
)

const (
	rate5090 = 2221.5 // xpredator, RTX 5090
	rate6000 = 1009.9 // minion, RTX 6000 Ada
	rateMac  = 301.4  // helion, Apple Silicon
)

// Queue depth alone preferred an idle slow engine over a faster busy one,
// concentrating prefill on the slower node. Compare rate-weighted load.
func TestAFastEngineWithWorkBeatsAnIdleSlowOne(t *testing.T) {
	ref := rate5090
	fast := cost(1, rate5090, ref, 0, false)
	slow := cost(0, rateMac, ref, 0, false)
	if !(fast < slow) {
		t.Errorf("the 5090 with one request (%.2f) should beat the idle Mac (%.2f)", fast, slow)
	}
}

// Queue growth must eventually outweigh speed, near the engines' rate ratio.
func TestTheSlowEngineWinsOnceTheFastOneIsBusyEnough(t *testing.T) {
	ref := rate5090
	slow := cost(0, rateMac, ref, 0, false)
	ratio := rate5090 / rateMac // ~7.4

	var crossover int64 = -1
	for n := int64(0); n < 30; n++ {
		if cost(n, rate5090, ref, 0, false) > slow {
			crossover = n
			break
		}
	}
	if crossover < 0 {
		t.Fatal("the slow engine never wins, however deep the fast one's queue")
	}
	if math.Abs(float64(crossover)-ratio) > 2 {
		t.Errorf("crossover at %d requests, expected near the %.1fx rate ratio", crossover, ratio)
	}
}

// Two idle engines: the faster one wins outright now, rather than depending on a
// tie-break that only fired when the scores were exactly equal.
func TestBetweenIdleEnginesTheFasterWins(t *testing.T) {
	ref := rate5090
	if !(cost(0, rate5090, ref, 0, false) < cost(0, rate6000, ref, 0, false)) {
		t.Error("the 5090 should beat the RTX 6000 Ada when both are idle")
	}
	if !(cost(0, rate6000, ref, 0, false) < cost(0, rateMac, ref, 0, false)) {
		t.Error("the RTX 6000 Ada should beat the Mac when both are idle")
	}
}

// Unmeasured engines need traffic to establish a rate, so do not penalize them.
func TestAnUnmeasuredEngineIsNotPunished(t *testing.T) {
	ref := rate5090
	fresh := cost(0, 0, ref, 0, false)
	measured := cost(0, rate5090, ref, 0, false)
	if fresh != measured {
		t.Errorf("an unmeasured idle engine costs %.2f but a measured one costs %.2f; "+
			"a rate nobody has taken yet must not decide against it", fresh, measured)
	}
	if got := cost(2, 0, 0, 0, false); got != 3 {
		t.Errorf("with no reference rate the cost should be depth+1, got %.2f", got)
	}
}

// LocalBias keeps its meaning: a nudge, in units of requests, not an override.
func TestLocalBiasStillNudgesWithoutOverriding(t *testing.T) {
	ref := rate5090
	localSlow := cost(0, rateMac, ref, 0.5, true)
	remoteFast := cost(0, rate5090, ref, 0, false)
	if !(remoteFast < localSlow) {
		t.Errorf("a 0.5 bias should not make a local Mac (%.2f) beat a remote 5090 (%.2f)",
			localSlow, remoteFast)
	}
	if !(cost(0, rate5090, ref, 0.5, true) < cost(0, rate5090, ref, 0, false)) {
		t.Error("with equal rates and load, local should win")
	}
}

func TestReferenceRateIsTheFastestKnown(t *testing.T) {
	cs := []Candidate{{PrefillTokS: rateMac}, {PrefillTokS: rate5090}, {PrefillTokS: 0}}
	if got := referenceRate(cs); got != rate5090 {
		t.Errorf("referenceRate = %.1f, want %.1f", got, rate5090)
	}
	if got := referenceRate([]Candidate{{}, {}}); got != 0 {
		t.Errorf("with nothing measured the reference should be 0, got %.1f", got)
	}
}

// Through Candidates, not just the arithmetic: the fleet shape that produced the
// finding. A local 5090 already serving two requests against an idle Mac peer.
func TestCandidatesSendWorkToTheFastEngineDespiteItsQueue(t *testing.T) {
	m := testMesh(t)
	setEngine(m.engines[0], "qwen") // gpu0, the 5090
	m.engines[0].SetRates(EngineRates{PrefillTokS: rate5090, Trusted: true})
	m.engines[0].SetSlots(2)
	m.engines[0].inflight.Store(1)

	mac := addPeer(m, "helion", 0, "qwen")
	mac.engines = []EngineState{{
		Name: "gpu0", Healthy: true, Models: []string{"qwen"},
		Slots: 1, EngineStats: EngineStats{PrefillTokS: rateMac, PrefillTrusted: true},
	}}

	got := names(m.Candidates("qwen", false))
	if len(got) == 0 || got[0] != "gpu0" {
		t.Fatalf("order %v: a 5090 with one request should still beat an idle Mac "+
			"seven times slower -- ranking by queue depth is what gave that Mac "+
			"49%% of the fleet's prefill", got)
	}
}

func TestRateWeightingCanBeTurnedOff(t *testing.T) {
	off := false
	m := testMesh(t)
	m.cfg.RateWeightedRouting = &off
	setEngine(m.engines[0], "qwen")
	m.engines[0].SetRates(EngineRates{PrefillTokS: rate5090, Trusted: true})
	m.engines[0].SetSlots(2)
	m.engines[0].inflight.Store(1)

	mac := addPeer(m, "helion", 0, "qwen")
	mac.engines = []EngineState{{
		Name: "gpu0", Healthy: true, Models: []string{"qwen"},
		Slots: 1, EngineStats: EngineStats{PrefillTokS: rateMac, PrefillTrusted: true},
	}}

	if got := names(m.Candidates("qwen", false)); got[0] != "helion" {
		t.Errorf("with weighting off the old depth-first order should return, got %v", got)
	}
}

// Engines without metrics cannot establish a rate from traffic and must not
// retain the optimistic score given to newly started measurable engines.
func TestAnUnmeasurableEngineIsNotFlattered(t *testing.T) {
	cs := []Candidate{
		{Name: "5090", PrefillTokS: rate5090},
		{Name: "mac", PrefillTokS: rateMac},
		{Name: "mac-mlx", RateUnmeasurable: true},
	}
	slowest, ref := slowestRate(cs), referenceRate(cs)
	if got := assumedRate(cs[2], slowest); got != rateMac {
		t.Fatalf("assumed rate %.0f, want the slowest measured %.0f", got, rateMac)
	}
	fast := cost(0, assumedRate(cs[0], slowest), ref, 0, false)
	mlx := cost(0, assumedRate(cs[2], slowest), ref, 0, false)
	if !(fast < mlx) {
		t.Errorf("idle 5090 costs %.2f and the idle unmeasurable engine %.2f; the "+
			"measured engine must win", fast, mlx)
	}

	if got := assumedRate(Candidate{Name: "just-started"}, slowest); got != 0 {
		t.Errorf("an unmeasured engine should carry no adjustment, got %.0f", got)
	}
}

func TestAMeasuredEngineWinsATieAgainstAnUnknownOne(t *testing.T) {
	m := testMesh(t)
	setEngine(m.engines[0], "qwen")
	m.engines[0].SetRates(EngineRates{PrefillTokS: rate5090, Trusted: true})
	m.engines[0].SetSlots(2)

	mac := addPeer(m, "helion", 0, "qwen")
	mac.instances = []InstanceState{{Model: "qwen", Engine: "mlx", Slots: 1}}
	mac.engines = []EngineState{{
		Name: "mlx0", Healthy: true, Models: []string{"qwen"}, Slots: 1,
	}}

	got := names(m.Candidates("qwen", false))
	if len(got) == 0 || got[0] != "gpu0" {
		t.Errorf("order %v: a measured engine should outrank one whose rate can "+
			"never be known", got)
	}
}

func TestAnUnmeasurableEngineStillEarnsWorkWhenTheOthersFillUp(t *testing.T) {
	cs := []Candidate{
		{Name: "5090", PrefillTokS: rate5090},
		{Name: "mac", PrefillTokS: rateMac},
		{Name: "mac-mlx", RateUnmeasurable: true},
	}
	slowest, ref := slowestRate(cs), referenceRate(cs)
	if slowest != rateMac {
		t.Fatalf("slowest = %.0f, want the Mac's %.0f", slowest, rateMac)
	}

	mlx := cost(0, assumedRate(cs[2], slowest), ref, 0, false)
	for n := int64(0); n < 40; n++ {
		if cost(n, rate5090, ref, 0, false) > mlx {
			return
		}
	}
	t.Error("an unmeasurable engine never wins, however deep the fast engine's queue")
}

func TestUnmeasurableWithNothingMeasuredFallsBackToDepth(t *testing.T) {
	cs := []Candidate{{Name: "a", RateUnmeasurable: true}, {Name: "b", RateUnmeasurable: true}}
	slowest := slowestRate(cs)
	if slowest != 0 {
		t.Fatalf("slowest = %.0f, want 0", slowest)
	}
	if got := cost(2, assumedRate(cs[0], slowest), referenceRate(cs), 0, false); got != 3 {
		t.Errorf("cost = %.2f, want depth+1", got)
	}
}
