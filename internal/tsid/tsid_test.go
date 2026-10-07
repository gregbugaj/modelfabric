package tsid

import (
	"context"
	"errors"
	"testing"
)

func TestSameOwner(t *testing.T) {
	greg := Identity{UserID: 7, Login: "greg@github"}
	other := Identity{UserID: 9, Login: "sam@github"}
	taggedA := Identity{Tags: []string{"tag:modelfabric"}}
	taggedB := Identity{Tags: []string{"tag:web", "tag:modelfabric"}}
	taggedC := Identity{Tags: []string{"tag:web"}}
	for _, c := range []struct {
		a, b Identity
		want bool
	}{
		{greg, greg, true},
		{greg, other, false},
		{taggedA, taggedB, true},
		{taggedA, taggedC, false},
		{greg, taggedA, false}, // a user and a tagged device are different identities
		{Identity{}, Identity{}, false},
	} {
		if got := SameOwner(c.a, c.b); got != c.want {
			t.Errorf("SameOwner(%+v, %+v) = %v", c.a, c.b, got)
		}
	}
}

func TestResolverParsesTailscaleAndCaches(t *testing.T) {
	calls := 0
	r := New()
	r.Run = func(_ context.Context, args ...string) ([]byte, error) {
		calls++
		switch args[0] {
		case "status":
			return []byte(`{"Self":{"HostName":"xpredator","UserID":13973407768925,"Tags":null},"User":{"13973407768925":{"LoginName":"gregbugaj@github"}}}`), nil
		case "whois":
			if args[2] == "100.100.69.3" {
				return []byte(`{"Node":{"ComputedName":"minion","User":13973407768925,"Tags":null},"UserProfile":{"LoginName":"gregbugaj@github"}}`), nil
			}
			return nil, errors.New("no such peer")
		}
		return nil, errors.New("unexpected")
	}
	self, err := r.Self(context.Background())
	if err != nil || self.Login != "gregbugaj@github" || self.Device != "xpredator" {
		t.Fatalf("self = %+v, %v", self, err)
	}
	peer, err := r.WhoIs(context.Background(), "100.100.69.3")
	if err != nil || !SameOwner(self, peer) || peer.Device != "minion" {
		t.Fatalf("peer = %+v, %v", peer, err)
	}
	r.WhoIs(context.Background(), "100.100.69.3")
	if calls != 2 {
		t.Errorf("lookups should be cached: %d calls", calls)
	}
	if _, err := r.WhoIs(context.Background(), "100.1.1.1"); err == nil {
		t.Error("an unknown address must be an error, never an identity")
	}
}
