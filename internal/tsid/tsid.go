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

func (r *Resolver) Self(ctx context.Context) (Identity, error) {
	r.mu.Lock()
	c := r.self
	r.mu.Unlock()
	if c != nil && time.Since(c.at) < r.TTL {
		return c.id, c.err
	}
	id, err := r.lookupSelf(ctx)
	r.mu.Lock()
	r.self = &cached{id: id, err: err, at: time.Now()}
	r.mu.Unlock()
	return id, err
}

func (r *Resolver) WhoIs(ctx context.Context, ip string) (Identity, error) {
	r.mu.Lock()
	c := r.peers[ip]
	r.mu.Unlock()
	if c != nil && time.Since(c.at) < r.TTL {
		return c.id, c.err
	}
	id, err := r.lookupPeer(ctx, ip)
	r.mu.Lock()
	r.peers[ip] = &cached{id: id, err: err, at: time.Now()}
	r.mu.Unlock()
	return id, err
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
