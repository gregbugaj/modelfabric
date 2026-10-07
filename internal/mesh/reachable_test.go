package mesh

import (
	"testing"
	"time"

	"github.com/gregbugaj/modelfabric/internal/config"
)

// Regression: a peer can serve traffic while Tailscale reports it offline.
// Recently active peers still need an HTTP probe.
func TestReachableTrustsTheDataPlane(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name string
		node tsNode
		want bool
	}{
		{"online", tsNode{Online: true}, true},
		{"control plane lost, data plane alive", tsNode{Online: false, Active: true}, true},
		{"just dropped", tsNode{Online: false, LastSeen: now.Add(-2 * time.Minute)}, true},
		{"gone for days", tsNode{Online: false, LastSeen: now.Add(-48 * time.Hour)}, false},
		// A peer that is online now reports a zero LastSeen, which must not be
		// read as "seen at the epoch" and then as long gone.
		{"never seen, not online", tsNode{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.node.reachable(peerGracePeriod); got != tc.want {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

// Regression: propagate Accepted and Scheduler from the peer's report into
// PeerView so remote front-door load and scheduler ownership remain visible.
func TestPeerViewCarriesWhatThePeerReported(t *testing.T) {
	m := New(config.Default(), "self")
	m.upsert("100.0.0.2", "sites-01", "http://100.0.0.2:1234", &NodeState{
		Node:      "sites-01",
		Inflight:  2,
		Accepted:  7,
		Scheduler: &SchedulerState{Model: "m", Profile: "tuned", Engines: 3},
	})
	pv := m.Peers()
	if len(pv) != 1 {
		t.Fatalf("expected one peer, got %d", len(pv))
	}
	if pv[0].Accepted != 7 {
		t.Errorf("the peer's front-door count should survive: got %d", pv[0].Accepted)
	}
	if pv[0].Scheduler == nil || pv[0].Scheduler.Profile != "tuned" || pv[0].Scheduler.Engines != 3 {
		t.Errorf("the peer's scheduler should survive: %+v", pv[0].Scheduler)
	}
}
