package tsid

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
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

const whoisMinion = `{"Node":{"ComputedName":"minion","User":7,"Tags":null},"UserProfile":{"LoginName":"owner@example"}}`

// One lookup that was cancelled or timed out used to refuse a device every
// management request for a minute (2026-10-08: "Tailscale does not identify
// 100.64.0.2: signal: killed" on a node that had just restarted).
func TestAFailedLookupIsNotRememberedAsAnAnswer(t *testing.T) {
	fail := true
	calls := 0
	r := New()
	r.Run = func(context.Context, ...string) ([]byte, error) {
		calls++
		if fail {
			return nil, errors.New("signal: killed")
		}
		return []byte(whoisMinion), nil
	}
	if _, err := r.WhoIs(context.Background(), "100.64.0.2"); err == nil {
		t.Fatal("the lookup failed and must say so")
	}
	// At once: not asked again, so a tailscaled that is down is not hammered.
	r.WhoIs(context.Background(), "100.64.0.2")
	if calls != 1 {
		t.Errorf("a failure is remembered briefly: %d lookups, want 1", calls)
	}
	// A few seconds on, tailscale is answering, and the device is let in
	// without waiting out the minute an answer is kept for.
	fail = false
	r.mu.Lock()
	r.peers["100.64.0.2"].at = time.Now().Add(-errTTL - time.Second)
	r.mu.Unlock()
	id, err := r.WhoIs(context.Background(), "100.64.0.2")
	if err != nil || id.Device != "minion" {
		t.Errorf("after the failure aged out: %+v, %v", id, err)
	}
}

// The request that starts a lookup may go away (a closed browser tab). The
// answer is for every request in the next minute and must not be "canceled".
func TestALookupOutlivesTheRequestThatStartedIt(t *testing.T) {
	r := New()
	r.Run = func(ctx context.Context, _ ...string) ([]byte, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return []byte(whoisMinion), nil
	}
	gone, cancel := context.WithCancel(context.Background())
	cancel()
	if id, err := r.WhoIs(gone, "100.64.0.2"); err != nil || id.Device != "minion" {
		t.Errorf("a cancelled caller poisoned the lookup: %+v, %v", id, err)
	}
}

func TestRequestsArrivingTogetherShareOneLookup(t *testing.T) {
	var calls atomic.Int32
	release := make(chan struct{})
	r := New()
	r.Run = func(context.Context, ...string) ([]byte, error) {
		calls.Add(1)
		<-release
		return []byte(whoisMinion), nil
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if id, err := r.WhoIs(context.Background(), "100.64.0.2"); err != nil || id.Device != "minion" {
				t.Errorf("got %+v, %v", id, err)
			}
		}()
	}
	// Let them all arrive before the one lookup answers.
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()
	if n := calls.Load(); n != 1 {
		t.Errorf("%d lookups for eight requests at once, want 1", n)
	}
}
