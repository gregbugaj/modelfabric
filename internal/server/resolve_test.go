package server

import (
	"testing"

	"github.com/gregbugaj/modelfabric/internal/mesh"
)

// resolveEngine turns the upstream a scheduler reports back into a node and an
// instance, so a request ModelFabric did not route still says where it ran. The
// first version compared a bare host against host:port and so matched
// nothing; these cases pin the join.
func TestResolveEngineMatchesHostAndPort(t *testing.T) {
	self := mesh.NodeState{
		Node: "xpredator", Addr: "100.64.0.1",
		Instances: []mesh.InstanceState{{ID: "inst-local", Address: "127.0.0.1", Port: 18000}},
	}
	peers := []mesh.PeerView{{
		Node: "minion", Addr: "100.64.0.2", Alive: true,
		Instances: []mesh.InstanceState{
			{ID: "inst-a", Address: "100.64.0.2", Port: 18000},
			{ID: "inst-b", Address: "100.64.0.2", Port: 18002},
		},
	}}
	for _, c := range []struct {
		name, base, node, engine string
		ok                       bool
	}{
		{"local engine", "http://127.0.0.1:18000/v1", "xpredator", "inst-local", true},
		{"peer engine", "http://100.64.0.2:18002/v1", "minion", "inst-b", true},
		{"no trailing path", "http://100.64.0.2:18000", "minion", "inst-a", true},
		// The port still decides which engine, so an unknown one leaves the
		// engine blank — but the machine is named, not dropped.
		{"right host, wrong port", "http://100.64.0.2:18001/v1", "minion", "", true},
		{"unknown host", "http://10.0.0.9:18000/v1", "", "", false},
		{"empty", "", "", "", false},
		{"not a url", "://nope", "", "", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			node, engine, ok := matchEngine(c.base, self, peers)
			if ok != c.ok || node != c.node || engine != c.engine {
				t.Errorf("matchEngine(%q) = (%q, %q, %v), want (%q, %q, %v)",
					c.base, node, engine, ok, c.node, c.engine, c.ok)
			}
		})
	}
}

// A base whose port matches no engine still names the machine, and that holds
// for this node too, whose engines are recorded at a loopback address: it is
// what a request through a shim looks like.
func TestMatchEngineNamesThisNodeForAnUnknownLoopbackPort(t *testing.T) {
	self := mesh.NodeState{Node: "xpredator", Addr: "100.64.0.1",
		Instances: []mesh.InstanceState{{ID: "inst-a", Address: "127.0.0.1", Port: 18000}}}
	node, engine, ok := matchEngine("http://127.0.0.1:18001/v1", self, nil)
	if !ok || node != "xpredator" || engine != "" {
		t.Fatalf("got (%q, %q, %v), want (xpredator, \"\", true)", node, engine, ok)
	}
}

// ModelFabric's own proxies are not engines. llm-d's Envoy listens on this machine
// while the EPP behind it may schedule onto any node, so matching the address
// would name this node as the server when it holds no engine at all.
func TestResolveEngineRefusesOwnProxies(t *testing.T) {
	// isOwnProxy reads only this node's own addresses; the mesh is never
	// consulted.
	s := &Server{frontListen: "127.0.0.1:1234"}
	for _, c := range []struct {
		name, base string
		want       bool
	}{
		{"ModelFabric's own front door", "http://127.0.0.1:1234/v1", true},
		{"an actual engine", "http://127.0.0.1:18000/v1", false},
		{"nothing", "", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := s.isOwnProxy(c.base); got != c.want {
				t.Fatalf("isOwnProxy(%q) = %v, want %v", c.base, got, c.want)
			}
		})
	}
}
