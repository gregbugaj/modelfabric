package mesh

import "testing"

func peerWith(insts ...InstanceState) *Peer {
	return &Peer{alive: true, instances: insts}
}

// Evaluate peers by engines serving the requested model; missing capability
// fields retain backward-compatible defaults.
func TestPeerConstrains(t *testing.T) {
	const model = "qwen/qwen3-0.6b"
	ready := func(engine string) InstanceState {
		return InstanceState{Model: model, State: "ready", Engine: engine}
	}

	for _, c := range []struct {
		name string
		peer *Peer
		want bool
	}{
		{"llama.cpp only", peerWith(ready(EngineLlamaCPP)), true},
		{"mlx only", peerWith(ready(EngineMLX)), false},
		// A mix is capable: forwarding reaches that node's router, which
		// applies this same rule to its own engines.
		{"a mix", peerWith(ready(EngineMLX), ready(EngineLlamaCPP)), true},
		// Empty means llama.cpp; it is what every node ran before the field
		// existed, so an older peer is not penalised.
		{"peer too old to say", peerWith(ready("")), true},
		// Without an instance list, defer capability filtering to the peer.
		{"no instances reported", peerWith(), true},
		// An instance that is not ready cannot serve, so it neither qualifies
		// nor disqualifies the peer.
		{"mlx not ready", peerWith(InstanceState{Model: model, State: "loading", Engine: EngineMLX}), true},
		{"mlx for a different model", peerWith(InstanceState{Model: "other", State: "ready", Engine: EngineMLX}), true},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := c.peer.constrains(model); got != c.want {
				t.Errorf("constrains(%q) = %v, want %v", model, got, c.want)
			}
		})
	}
}
