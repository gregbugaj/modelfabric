package supervisor

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/gregbugaj/modelfabric/internal/ops"
)

// JIT is disabled by default. TTL applies only to instances configured with
// one; automatic eviction affects only JIT-loaded models. Idle-evicted models
// reload only when another request needs them.

var ErrJITDisabled = errors.New("JIT loading is disabled")

var ErrNotInCatalog = errors.New("model is not in this node's catalog")

const reapInterval = 10 * time.Second

func (i *Instance) lastUsed() time.Time {
	if i.engine != nil {
		if t := i.engine.LastUsed(); !t.IsZero() {
			return t
		}
	}
	return i.StartedAt
}

func (i *Instance) inflight() int64 {
	if i.engine == nil {
		return 0
	}
	return i.engine.Inflight()
}

func (i *Instance) expired(now time.Time) bool {
	return i.TTLSeconds > 0 && i.inflight() == 0 &&
		now.Sub(i.lastUsed()) >= time.Duration(i.TTLSeconds)*time.Second
}

func (s *Supervisor) JITEnabled() bool { return s.cfg.JIT && !s.cfg.Entrypoint }

// EnsureLoaded JIT-loads a model for a request and waits until it is ready.
// ttlSeconds is the request's own "ttl"; zero means the configured default.
func (s *Supervisor) EnsureLoaded(ctx context.Context, ref string, ttlSeconds int) error {
	return s.EnsureLoadedWith(ctx, ref, ttlSeconds, 0)
}

// EnsureLoadedWith is EnsureLoaded for a request that says how much context
// it wants (/api/v1/chat's context_length); zero leaves the model's default.
func (s *Supervisor) EnsureLoadedWith(ctx context.Context, ref string, ttlSeconds, contextLength int) error {
	if !s.cfg.JIT {
		return ErrJITDisabled
	}
	model, err := s.Catalog().Resolve(ref)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrNotInCatalog, err)
	}
	if ttlSeconds <= 0 {
		ttlSeconds = int(s.cfg.JITTTL / time.Second)
	}
	if s.cfg.JITAutoEvict {
		s.evictJIT(model.Key)
	}

	req := LoadRequest{Model: model.Key, TTL: ttlSeconds, origin: "jit"}
	if contextLength > 0 {
		req.ContextLength = &contextLength
	}
	op, _, err := s.Load(req)
	if err != nil {
		return err
	}
	s.log.Info("JIT loading model for a request", "model", model.Key, "ttl_seconds", ttlSeconds)
	for {
		cur, done := s.journal.Wait(op.ID, time.Second)
		if done {
			if cur.State != ops.StateSucceeded {
				return fmt.Errorf("JIT load of %s failed: %s", model.Key, cur.Error)
			}
			return nil
		}
		if err := ctx.Err(); err != nil {
			// The load carries on and stays observable; only this request
			// stops waiting for it.
			return err
		}
	}
}

// evictJIT unloads idle JIT-loaded instances of other models, so on-demand
// loading swaps models rather than stacking them. An instance with requests in
// flight is left alone: eviction must never cut off a response.
func (s *Supervisor) evictJIT(keep string) {
	for _, inst := range s.Instances() {
		if inst.Origin != "jit" || inst.Model == keep {
			continue
		}
		s.mu.RLock()
		live, ok := s.instances[inst.ID]
		busy := ok && live.inflight() > 0
		s.mu.RUnlock()
		if !ok || busy {
			continue
		}
		s.log.Info("evicting JIT-loaded model", "model", inst.Model, "instance", inst.ID, "for", keep)
		if _, err := s.unload(inst.ID, s.cfg.StopTimeout); err != nil {
			s.log.Warn("evict failed", "instance", inst.ID, "err", err)
		}
	}
}

func (s *Supervisor) reapIdleLoop() {
	t := time.NewTicker(reapInterval)
	defer t.Stop()
	for {
		select {
		case <-s.stop:
			return
		case now := <-t.C:
			s.reapIdle(now)
		}
	}
}

func (s *Supervisor) reapIdle(now time.Time) []string {
	var expired []string
	s.mu.RLock()
	for id, inst := range s.instances {
		if inst.expired(now) {
			expired = append(expired, id)
		}
	}
	s.mu.RUnlock()

	for _, id := range expired {
		// A request may have come and gone since the scan; its finish time
		// restarts the clock. Anything that slips past this check is drained,
		// not cut off. Unload also re-checks existence under the lock.
		s.mu.RLock()
		inst, ok := s.instances[id]
		still := ok && inst.expired(now)
		s.mu.RUnlock()
		if !still {
			continue
		}
		if _, err := s.unload(id, s.cfg.StopTimeout); err != nil {
			continue
		}
		s.log.Info("unloaded idle instance: TTL expired", "instance", id)
	}
	return expired
}
