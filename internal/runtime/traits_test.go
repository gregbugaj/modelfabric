package runtime

import "testing"

// The trait that keeps schema-constrained requests off an engine that would
// ignore them. mlx-lm's server parses neither response_format nor a grammar,
// so it answers such a request with free prose and a 200 — see
// https://github.com/ml-explore/mlx-lm (mlx_lm/server.py).
func TestEngineTraitsConstrainedDecoding(t *testing.T) {
	if !EngineTraits("mlx").NoConstrainedDecoding {
		t.Error("mlx must be marked as ignoring constrained output, or the router will send schema requests to it")
	}
	if EngineTraits("llama.cpp").NoConstrainedDecoding {
		t.Error("llama.cpp honours response_format and grammar; marking it otherwise would strand requests")
	}
	// An engine family ModelFabric does not know gets the zero value, which
	// describes llama.cpp — the optimistic reading, and the one that keeps a
	// node too old to name its family working as it always did.
	if EngineTraits("").NoConstrainedDecoding || EngineTraits("something-new").NoConstrainedDecoding {
		t.Error("an unknown engine must default to capable, as the zero value does everywhere else")
	}
}
