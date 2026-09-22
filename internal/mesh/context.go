package mesh

import "fmt"

// How much room a conversation has left.
//
// The mesh published slots, KV usage, prefill and decode rates and lifetime
// token counters, but never the context an engine was loaded with — so nothing
// reading it could say how close a conversation was to the limit. The only tasks
// a 2026-09-24 benchmark run failed to solve died exactly there, at about
// 130,900 tokens of 131,072, and nothing anywhere said they were near it.
//
// Two numbers, because they answer different questions and confusing them is
// how this stays broken:
//
//   - the per-request context: what one conversation may use, and the only
//     number a headroom warning can be built on.
//   - the KV pool: what the engine holds in total, which is the per-request
//     context once for every slot. llama.cpp's -c is this one.
//
// ModelFabric knows what it asked for. The engine knows what it got, and those differ
// more often than they should: a load whose settings were silently ignored ran
// 32768 while `mfsh ps` reported 65536, and every long conversation routed there
// overflowed with nothing saying why. So both are published, and a disagreement
// is reported rather than averaged away.

// reconcileContext works out the per-request context an engine is really
// running, from what ModelFabric asked for and what the engine reports as its pool.
//
// Reading the engine's number alone is not enough, because what it means differs
// by build. Measured on this fleet the same day, all serving the same model with
// `default_generation_settings.n_ctx` and no top-level n_ctx:
//
//	LM Studio cuda12-avx2@2.40.0   262144 with 4 slots   -> the whole pool
//	Metal advsimd@2.41.0            65536 with 1 slot    -> pool, indistinguishable
//	upstream b11153                 65536 with 2 slots   -> per slot
//
// So the same field is a pool on one build and a per-slot figure on another, and
// a mesh that runs three builds by design cannot pick one reading. What it can
// do is check the engine's number against both possibilities and only complain
// when it matches neither — which is precisely the case worth complaining about.
func reconcileContext(asked, slots, enginePool int) (perRequest int, mismatch string) {
	if slots <= 0 {
		slots = 1
	}
	if enginePool <= 0 {
		// The engine did not say — mlx-lm serves no /props at all. What ModelFabric
		// asked for is the best answer available, and saying nothing would
		// leave every caller with no headroom figure rather than an unverified
		// one.
		return asked, ""
	}
	if asked > 0 {
		if enginePool == asked*slots || enginePool == asked {
			return asked, ""
		}
	}
	// They disagree, so the engine wins: it is the thing that will refuse the
	// request. Divide when it divides evenly, since a pool is the more common
	// reading; otherwise take it as it stands.
	if slots > 1 && enginePool%slots == 0 {
		perRequest = enginePool / slots
	} else {
		perRequest = enginePool
	}
	if asked <= 0 {
		return perRequest, ""
	}
	return perRequest, fmt.Sprintf(
		"loaded for %d tokens per request but the engine reports %d across %s: it is running %d",
		asked, enginePool, plural(slots, "slot"), perRequest)
}

func plural(n int, word string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, word)
	}
	return fmt.Sprintf("%d %ss", n, word)
}
