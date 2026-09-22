// Package ops is a durable operation journal for long-running work.
//
// Loading a 19GB model can outlast the requesting client. The journal lets a
// reconnecting client observe the result and join compatible in-flight work
// instead of launching a second engine. File-backed storage keeps the node
// independent of a database; restart recovery settles interrupted operations.
package ops

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

type State string

const (
	StateRunning   State = "running"
	StateSucceeded State = "succeeded"
	StateFailed    State = "failed"
	// StateCancelled is an operation stopped on request; not a failure.
	StateCancelled State = "cancelled"
)

func (s State) Terminal() bool {
	return s == StateSucceeded || s == StateFailed || s == StateCancelled
}

// Operation is one unit of long-running work.
type Operation struct {
	ID    string `json:"id"`
	Kind  string `json:"kind"`  // "load" | "unload" | "runtime-get"
	Model string `json:"model"` // model key (or runtime name) this operation concerns
	// DedupeKey identifies a *compatible* operation. A retry with the same key
	// joins this operation instead of spawning a second process.
	DedupeKey string `json:"dedupe_key"`
	State     State  `json:"state"`
	Message   string `json:"message,omitempty"`
	// Fraction is completed work in [0,1] when the operation can measure it
	// (a download), so a UI can draw a real progress bar.
	Fraction float64 `json:"fraction,omitempty"`
	Error    string  `json:"error,omitempty"`
	// InstanceID is set once a load succeeds.
	InstanceID string     `json:"instance_id,omitempty"`
	StartedAt  time.Time  `json:"started_at"`
	EndedAt    *time.Time `json:"ended_at,omitempty"`
	ElapsedMS  int64      `json:"elapsed_ms"`
	// PersistError is set when this operation could not be written to the
	// journal, so a caller can tell "recorded" from "only in memory".
	PersistError string `json:"persist_error,omitempty"`
}

// Journal stores operations in memory and on disk.
type Journal struct {
	dir string

	mu     sync.RWMutex
	byID   map[string]*Operation
	byKey  map[string]string // dedupe key -> id of the running operation
	waits  map[string]chan struct{}
	retain time.Duration
	logger *slog.Logger
}

func NewJournal(dir string) (*Journal, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create operations dir: %w", err)
	}
	j := &Journal{
		dir:    dir,
		byID:   map[string]*Operation{},
		byKey:  map[string]string{},
		waits:  map[string]chan struct{}{},
		retain: 24 * time.Hour,
	}
	j.load()
	j.Prune()
	return j, nil
}

// load reads journalled operations from a previous run. Anything still marked
// running did not survive the restart, so it is settled as failed rather than
// left to look live forever.
func (j *Journal) load() {
	entries, err := os.ReadDir(j.dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		b, err := os.ReadFile(filepath.Join(j.dir, e.Name()))
		if err != nil {
			continue
		}
		var op Operation
		if err := json.Unmarshal(b, &op); err != nil {
			continue
		}
		// The id comes from the file's contents and is later used to build the
		// path this operation is written back to. It has to be the file's own
		// name, or a crafted journal entry chooses where ModelFabric writes.
		if op.ID != strings.TrimSuffix(e.Name(), ".json") {
			continue
		}
		if !op.State.Terminal() {
			op.State = StateFailed
			op.Error = "host restarted while this operation was running"
			now := time.Now().UTC()
			op.EndedAt = &now
			j.persist(&op)
		}
		j.byID[op.ID] = &op
	}
}

// Begin starts an operation, or joins a compatible one already running.
//
// The bool reports whether this call created it: false means an equivalent
// operation was already in flight and no new work should be started.
func (j *Journal) Begin(kind, model, dedupeKey string) (*Operation, bool) {
	j.mu.Lock()
	defer j.mu.Unlock()

	if id, ok := j.byKey[dedupeKey]; ok {
		if existing, ok := j.byID[id]; ok && !existing.State.Terminal() {
			return existing.clone(), false
		}
		delete(j.byKey, dedupeKey)
	}

	op := &Operation{
		ID:        newID(),
		Kind:      kind,
		Model:     model,
		DedupeKey: dedupeKey,
		State:     StateRunning,
		StartedAt: time.Now().UTC(),
	}
	j.byID[op.ID] = op
	j.byKey[dedupeKey] = op.ID
	j.waits[op.ID] = make(chan struct{})
	j.persist(op)
	return op.clone(), true
}

// Progress records a human-readable step without ending the operation.
func (j *Journal) Progress(id, message string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if op, ok := j.byID[id]; ok && !op.State.Terminal() {
		op.Message = message
		j.persist(op)
	}
}

// Report records a step with measured progress. Callers throttle it: each
// report is persisted.
func (j *Journal) Report(id, message string, fraction float64) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if op, ok := j.byID[id]; ok && !op.State.Terminal() {
		op.Message, op.Fraction = message, fraction
		j.persist(op)
	}
}

// Succeed settles an operation successfully.
func (j *Journal) Succeed(id, instanceID string) { j.finish(id, StateSucceeded, instanceID, nil) }

// Fail settles an operation with an error.
func (j *Journal) Fail(id string, err error) { j.finish(id, StateFailed, "", err) }

// Cancelled records an operation stopped on request.
func (j *Journal) Cancelled(id string) { j.finish(id, StateCancelled, "", nil) }

func (j *Journal) finish(id string, state State, instanceID string, failure error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	op, ok := j.byID[id]
	if !ok || op.State.Terminal() {
		return
	}
	now := time.Now().UTC()
	op.State = state
	op.EndedAt = &now
	op.ElapsedMS = now.Sub(op.StartedAt).Milliseconds()
	if instanceID != "" {
		op.InstanceID = instanceID
	}
	if failure != nil {
		op.Error = failure.Error()
	}
	delete(j.byKey, op.DedupeKey)
	j.persist(op)
	if ch, ok := j.waits[id]; ok {
		close(ch)
		delete(j.waits, id)
	}
}

// Wait blocks until the operation reaches a terminal state or the timeout
// elapses. A timeout does not cancel the operation: it stays observable, which
// is what lets a disconnected client reattach instead of retrying the work.
func (j *Journal) Wait(id string, timeout time.Duration) (*Operation, bool) {
	j.mu.RLock()
	op, ok := j.byID[id]
	if !ok {
		j.mu.RUnlock()
		return nil, false
	}
	if op.State.Terminal() {
		out := op.clone()
		j.mu.RUnlock()
		return out, true
	}
	ch := j.waits[id]
	j.mu.RUnlock()

	if ch == nil {
		return j.Get(id)
	}
	select {
	case <-ch:
		return j.Get(id)
	case <-time.After(timeout):
		cur, _ := j.Get(id)
		return cur, false
	}
}

func (j *Journal) Get(id string) (*Operation, bool) {
	j.mu.RLock()
	defer j.mu.RUnlock()
	op, ok := j.byID[id]
	if !ok {
		return nil, false
	}
	return op.clone(), true
}

// List returns operations newest first.
func (j *Journal) List() []Operation {
	j.mu.RLock()
	defer j.mu.RUnlock()
	out := make([]Operation, 0, len(j.byID))
	for _, op := range j.byID {
		out = append(out, *op.clone())
	}
	sort.Slice(out, func(i, k int) bool { return out[i].StartedAt.After(out[k].StartedAt) })
	return out
}

// RunPruner bounds disk usage even when the node runs for weeks without a
// restart or another model operation. Cancellation owns the goroutine's life.
func (j *Journal) RunPruner(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			j.Prune()
		}
	}
}

// Prune removes settled operations older than the retention window.
func (j *Journal) Prune() {
	cutoff := time.Now().Add(-j.retain)
	j.mu.Lock()
	defer j.mu.Unlock()
	for id, op := range j.byID {
		if op.State.Terminal() && op.EndedAt != nil && op.EndedAt.Before(cutoff) {
			if err := os.Remove(j.path(id)); err != nil && !os.IsNotExist(err) {
				j.log().Warn("could not prune operation; will retry", "op", id, "err", err)
				continue
			}
			delete(j.byID, id)
		}
	}
}

func (j *Journal) path(id string) string { return filepath.Join(j.dir, id+".json") }

// persist must be called with the lock held.
//
// The journal is what lets an operation survive a restart, so a write that
// fails silently is worse than no journal at all: the in-memory index reports
// the operation as recorded while nothing reached disk. Failures are reported
// on the operation itself and logged, rather than dropped.
func (j *Journal) persist(op *Operation) {
	if err := j.write(op); err != nil {
		op.PersistError = err.Error()
		j.log().Error("operation not journalled; it will not survive a restart",
			"op", op.ID, "kind", op.Kind, "err", err)
	} else if op.PersistError != "" {
		op.PersistError = ""
	}
}

func (j *Journal) write(op *Operation) error {
	b, err := json.MarshalIndent(op, "", "  ")
	if err != nil {
		return fmt.Errorf("encode: %w", err)
	}
	tmp := j.path(op.ID) + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return fmt.Errorf("write: %w", err)
	}
	if err := os.Rename(tmp, j.path(op.ID)); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("rename: %w", err)
	}
	return nil
}

// SetLogger wires in the node's logger so a journal write that fails is
// reported rather than dropped.
func (j *Journal) SetLogger(l *slog.Logger) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.logger = l
}

// log returns the journal's logger, or a discarding one when it has none.
func (j *Journal) log() *slog.Logger {
	if j.logger != nil {
		return j.logger
	}
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func (o *Operation) clone() *Operation {
	c := *o
	if o.EndedAt != nil {
		t := *o.EndedAt
		c.EndedAt = &t
	}
	return &c
}

func newID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("op-%d", time.Now().UnixNano())
	}
	return "op-" + hex.EncodeToString(b[:])
}
