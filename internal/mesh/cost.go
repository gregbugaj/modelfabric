package mesh

// Compare queue cost in units of one request on the fastest measured engine.
// Prefill-rate weighting accounts for heterogeneous hardware and prompt-heavy
// workloads. Decode-heavy workloads are not modeled here.

// referenceRate returns the fastest candidate rate, or zero if unmeasured.
// Relative weighting preserves placement proportions across different hardware.
func referenceRate(cs []Candidate) float64 {
	var ref float64
	for _, c := range cs {
		if c.PrefillTokS > ref {
			ref = c.PrefillTokS
		}
	}
	return ref
}

// slowestRate supplies a conservative rate for engines without throughput
// counters. Unlike newly started measurable engines, these cannot acquire a
// rate from traffic and would otherwise remain permanently overrated.
func slowestRate(cs []Candidate) float64 {
	slowest := 0.0
	for _, c := range cs {
		if c.PrefillTokS > 0 && (slowest == 0 || c.PrefillTokS < slowest) {
			slowest = c.PrefillTokS
		}
	}
	return slowest
}

// assumedRate uses a measured rate when available, the fleet's slowest rate
// for unmeasurable engines, and zero (no adjustment) for unmeasured engines.
func assumedRate(c Candidate, slowest float64) float64 {
	if c.PrefillTokS > 0 {
		return c.PrefillTokS
	}
	if c.RateUnmeasurable {
		return slowest
	}
	return 0
}

// cost includes the new request so faster idle engines rank ahead of slower ones.
// Unmeasured engines use the reference rate so they receive enough traffic
// to establish a measurement.
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
