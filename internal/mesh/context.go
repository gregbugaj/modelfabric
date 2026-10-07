package mesh

import "fmt"

// Track per-request context separately from the total KV pool. llama.cpp -c
// sets the pool; headroom checks need per-request capacity. Compare requested
// and reported values to expose load settings the engine ignored.

// reconcileContext reconciles requested per-slot capacity with /props.
// Builds disagree on default_generation_settings.n_ctx units:
//
//	LM Studio cuda12-avx2@2.40.0: 262144 with 4 slots, total pool
//	upstream b11153: 65536 with 2 slots, per-slot context
//
// Accept either interpretation when it matches the request; report a mismatch otherwise.
func reconcileContext(asked, slots, enginePool int) (perRequest int, mismatch string) {
	if slots <= 0 {
		slots = 1
	}
	if enginePool <= 0 {
		// Without /props (as on mlx-lm), use the requested context as unverified
		// capacity for headroom estimates.
		return asked, ""
	}
	if asked > 0 {
		if enginePool == asked*slots || enginePool == asked {
			return asked, ""
		}
	}
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
