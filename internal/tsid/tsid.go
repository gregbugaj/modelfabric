// Package tsid answers "who is on the other end of this tailnet connection?"
// using Tailscale's own record of every device, so ModelFabric can let the owner's
// devices manage each other without a password of its own.
//
// Tailscale has already authenticated every tailnet connection to a device,
// and every device belongs to a user or carries tags. Two devices have the
// same owner when both are untagged and belong to the same user, or both are
// tagged and share a tag (tags replace the user as a device's identity).
// Anything else; including any failure to find out; is not the same owner.
package tsid

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/gregbugaj/modelfabric/internal/tscli"
)

// Identity is a device's owner as Tailscale records it.
type Identity struct {
	UserID int64    `json:"user_id,omitempty"`
	Login  string   `json:"login,omitempty"`
	Tags   []string `json:"tags,omitempty"`
	Device string   `json:"device,omitempty"`
}

func SameOwner(a, b Identity) bool {
	switch {
	case len(a.Tags) > 0 && len(b.Tags) > 0:
		for _, t := range a.Tags {
			if slices.Contains(b.Tags, t) {
				return true
			}
		}
		return false
	case len(a.Tags) == 0 && len(b.Tags) == 0:
		return a.UserID != 0 && a.UserID == b.UserID
	}
	return false
}

// Resolver looks identities up through the tailscale CLI, caching them: a
// dashboard polls, and whois runs once per request otherwise.
type Resolver struct {
	// Run executes the tailscale CLI; replaced in tests.
	Run func(ctx context.Context, args ...string) ([]byte, error)
	TTL time.Duration

	mu    sync.Mutex
	self  *cached
	peers map[string]*cached
	// flight is the lookups running now, by key, each with a channel closed
	// when it ends.
	flight map[string]chan struct{}
}

type cached struct {
	id  Identity
	err error
	at  time.Time
}

func New() *Resolver {
	return &Resolver{
		Run: func(ctx context.Context, args ...string) ([]byte, error) {
			return tscli.Run(ctx, args...)
		},
		TTL:   time.Minute,
		peers: map[string]*cached{},
	}
}

// errTTL is how long a failed lookup is remembered. An answer is good for
// TTL: who owns a tailnet address does not change from one minute to the
// next. A failure is not an answer. It used to be kept as long, so one lookup
// that was cancelled or timed out refused that device every management
// request for a minute: on 2026-10-08 two nodes dropped out of another's
// dashboard with "Tailscale does not identify 100.x: signal: killed" and
// "context canceled", each from a single bad lookup just after a restart.
// Long enough that a tailscaled that is down is not asked on every request.
const errTTL = 3 * time.Second

func (r *Resolver) fresh(c *cached) bool {
	if c == nil {
		return false
	}
	if c.err != nil {
		return time.Since(c.at) < errTTL
	}
	return time.Since(c.at) < r.TTL
}

// once runs one lookup for key however many callers ask at the same time,
// and gives them all its result. Several requests arriving together after a
// restart each used to start their own `tailscale`, and the slowest were
// killed at the deadline.
func (r *Resolver) once(key string, get func() *cached, set func(*cached), lookup func(context.Context) (Identity, error)) (Identity, error) {
	r.mu.Lock()
	if c := get(); r.fresh(c) {
		r.mu.Unlock()
		return c.id, c.err
	}
	if wait, running := r.flight[key]; running {
		r.mu.Unlock()
		<-wait
		r.mu.Lock()
		c := get()
		r.mu.Unlock()
		if c == nil {
			return Identity{}, errors.New("identity lookup did not finish")
		}
		return c.id, c.err
	}
	done := make(chan struct{})
	if r.flight == nil {
		r.flight = map[string]chan struct{}{}
	}
	r.flight[key] = done
	r.mu.Unlock()

	// Not the caller's context: the answer is for everyone who asks in the
	// next minute, and the request that happened to start the lookup going
	// away (a closed browser tab) must not turn into a refusal for the rest.
	id, err := lookup(context.Background())

	r.mu.Lock()
	set(&cached{id: id, err: err, at: time.Now()})
	delete(r.flight, key)
	r.mu.Unlock()
	close(done)
	return id, err
}

// Self is this node's own identity. ctx is accepted for the callers that
// have one; the lookup runs to its own deadline (see once).
func (r *Resolver) Self(_ context.Context) (Identity, error) {
	return r.once("self",
		func() *cached { return r.self },
		func(c *cached) { r.self = c },
		r.lookupSelf)
}

// WhoIs is the identity Tailscale gives for a tailnet address.
func (r *Resolver) WhoIs(_ context.Context, ip string) (Identity, error) {
	return r.once("peer:"+ip,
		func() *cached { return r.peers[ip] },
		func(c *cached) { r.peers[ip] = c },
		func(ctx context.Context) (Identity, error) { return r.lookupPeer(ctx, ip) })
}

func (r *Resolver) lookupSelf(ctx context.Context) (Identity, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	out, err := r.Run(ctx, "status", "--json")
	if err != nil {
		return Identity{}, err
	}
	var st struct {
		Self struct {
			HostName string
			UserID   int64
			Tags     []string
		}
		User map[string]struct{ LoginName string }
	}
	if err := json.Unmarshal(out, &st); err != nil {
		return Identity{}, err
	}
	id := Identity{UserID: st.Self.UserID, Tags: st.Self.Tags, Device: st.Self.HostName}
	id.Login = st.User[strconv.FormatInt(st.Self.UserID, 10)].LoginName
	if id.UserID == 0 && len(id.Tags) == 0 {
		return id, errors.New("tailscale reports no owner for this device")
	}
	return id, nil
}

func (r *Resolver) lookupPeer(ctx context.Context, ip string) (Identity, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	out, err := r.Run(ctx, "whois", "--json", ip)
	if err != nil {
		return Identity{}, err
	}
	var w struct {
		Node struct {
			ComputedName string
			User         int64
			Tags         []string
		}
		UserProfile struct{ LoginName string }
	}
	if err := json.Unmarshal(out, &w); err != nil {
		return Identity{}, err
	}
	id := Identity{UserID: w.Node.User, Login: w.UserProfile.LoginName, Tags: w.Node.Tags, Device: w.Node.ComputedName}
	if id.UserID == 0 && len(id.Tags) == 0 {
		return id, errors.New("tailscale does not know " + ip)
	}
	return id, nil
}
