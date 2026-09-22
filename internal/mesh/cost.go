package mesh

// A slot is not a slot.
//
// The router compared candidates by how many requests they already had, and
// used the measured prefill rate only to break ties. On a mesh of identical
// machines that is right. On this one it is badly wrong: measured the same
// afternoon, prefill ran at 2,221 tok/s on the RTX 5090, 1,010 on the RTX 6000
// Ada and 301 on the Apple Silicon node — a 7.4x spread. Queue depth alone said
// an idle Mac was the best place for a request while a 5090 with one request on
// it was worse, which is how a 24 tok/s node came to absorb 49% of the fleet's
// prefill and produce a twelfth of the output. Every router in front of it
// believed the same arithmetic, because a slot is the unit they all count in.
//
// So the comparison is time, not depth: roughly how long until this candidate
// could get to a new request, expressed in units of "one request on the fastest
// engine in the fleet". A slow engine's queue costs more per entry, which is
// exactly what it does in reality.
//
// Prefill rate is the measure because it is the one ModelFabric already measures and
// trusts (see TrustedPrefillRate), and because it dominates the workloads this
// mesh serves: an agentic coding turn sends tens of thousands of prompt tokens
// and takes back a few hundred. A decode-heavy workload would want the decode
// rate weighted in too, which the mesh publishes per instance but candidates do
// not carry yet.

// referenceRate is the fastest measured rate among the candidates, or zero when
// none of them has been measured. Relative rather than absolute: the question is
// which of *these* engines to use, and a fleet where everything is slow should
// still spread work the same way a fast one does.
func referenceRate(cs []Candidate) float64 {
	var ref float64
	for _, c := range cs {
		if c.PrefillTokS > ref {
			ref = c.PrefillTokS
		}
	}
	return ref
}

// slowestRate is the lowest measured rate among the candidates, and what an
// engine that can never report one is assumed to manage.
//
// An engine with no rate yet is given the benefit of the doubt — see cost —
// because it needs traffic before anything can measure it. That reasoning does
// not hold for an engine with no counters at all: mlx-lm serves no /metrics, so
// it will never be measured, and the optimistic reading would make it
// permanently indistinguishable from the fastest card in the fleet.
//
// Which way to be wrong is not symmetric. Over-rating a slow engine degrades
// everything — a 24 tok/s Mac given a fast engine's share of the work took 49%
// of this fleet's prefill and produced a twelfth of the output. Under-rating one
// leaves capacity idle and degrades nothing, and it self-corrects: cost rises
// with queue depth, so an unmeasurable engine still earns work as the measured
// ones fill up. What it never gets is preference on the strength of a guess.
func slowestRate(cs []Candidate) float64 {
	slowest := 0.0
	for _, c := range cs {
		if c.PrefillTokS > 0 && (slowest == 0 || c.PrefillTokS < slowest) {
			slowest = c.PrefillTokS
		}
	}
	return slowest
}

// assumedRate is the rate to score a candidate by: its own when it has one, the
// slowest in the fleet when it can never have one, and zero — meaning "no
// adjustment" — when it has merely not been measured yet.
func assumedRate(c Candidate, slowest float64) float64 {
	if c.PrefillTokS > 0 {
		return c.PrefillTokS
	}
	if c.RateUnmeasurable {
		return slowest
	}
	return 0
}

// cost is what a new request would cost on this candidate, lower being better.
//
// outstanding+1 counts the new request itself, so an idle engine still has a
// cost and a faster idle engine wins — previously that only happened by
// tie-break, and only when the scores were exactly equal.
//
// An unmeasured rate is treated as the reference rate rather than as slow. A
// freshly started engine has no measurement yet, and punishing it for that
// would keep it unmeasured: nothing would be sent to it, so nothing would ever
// measure it.
func cost(outstanding int64, rate, ref, localBias float64, local bool) float64 {
	c := float64(outstanding + 1)
	if ref > 0 && rate > 0 {
		c *= ref / rate
	}
	if local {
		// Unchanged in meaning: a nudge towards this machine when the estimates
		// are otherwise close, because a local engine costs no network hop.
		c -= localBias
	}
	return c
}
