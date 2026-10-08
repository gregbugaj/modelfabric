// Package process supervises one native engine process through readiness, stop
// and recovery.
//
// Process ownership and recovery depend on these rules:
//
//   - A PID alone never authorizes adoption or termination. PIDs are reused, so
//     every handle also carries the process's birth time; a mismatch means the
//     PID now belongs to somebody else and we refuse to touch it.
//   - argv comes from trusted runtime configuration, never from an inference
//     request, and is executed directly; never through a shell.
//   - Readiness is bound to the expected endpoint *and* model. An unrelated
//     server already listening on the port must not satisfy our launch.
//   - Launch intent is persisted before the spawn, so a crash between fork and
//     record leaves an auditable `unknown` rather than a silent duplicate.
package process

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gregbugaj/modelfabric/internal/osproc"
)

type State string

const (
	StateStarting State = "starting"
	StateReady    State = "ready"
	StateStopped  State = "stopped"
	StateFailed   State = "failed"
	// StateUnknown means we cannot prove what happened; typically a crash
	// between persisting launch intent and recording process identity. It
	// deliberately blocks another launch for the same deployment.
	StateUnknown State = "unknown"
)

var (
	ErrLaunch    = errors.New("launch failed")
	ErrOwnership = errors.New("ownership check failed")
)

type LaunchSpec struct {
	DeploymentID string   `json:"deployment_id"`
	Generation   int      `json:"generation"`
	Argv         []string `json:"argv"`
	Env          []string `json:"env,omitempty"`
	Cwd          string   `json:"cwd"`
	Endpoint     string   `json:"endpoint"`
	Model        string   `json:"model"`
	// Engine is the runtime family ("llama.cpp", "mlx"), and ServedModel the id
	// that engine answers to for Model; its own /v1/models id, which is not
	// always the catalog key. Readiness is checked against these, which is what
	// stops a stray server on the port from satisfying us.
	Engine                 string        `json:"engine,omitempty"`
	ServedModel            string        `json:"served_model,omitempty"`
	StartupTimeout         time.Duration `json:"startup_timeout"`
	StopTimeout            time.Duration `json:"stop_timeout"`
	StdoutPath, StderrPath string        `json:"-"`

	// Meta is opaque JSON persisted with the ownership record. The launcher
	// never interprets it; it exists so a caller can rebuild its own view of
	// an adopted process after a restart.
	Meta []byte `json:"meta,omitempty"`
}

// Handle identifies an owned process. Generation plus BirthID together are what
// make stop safe.
type Handle struct {
	DeploymentID string `json:"deployment_id"`
	Generation   int    `json:"generation"`
	InstanceID   string `json:"instance_id"`
	PID          int    `json:"pid"`
	// BirthID is the kernel's start time for this PID. Together with the PID it
	// is a stable identity that survives PID reuse.
	BirthID   string `json:"birth_id"`
	StartedAt string `json:"process_started_at"`
	Endpoint  string `json:"endpoint"`
	Model     string `json:"model"`
	// Engine and ServedModel are the spec's, kept so an adopted process can
	// still be probed the way its own engine requires.
	Engine      string `json:"engine,omitempty"`
	ServedModel string `json:"served_model,omitempty"`
}

// ReadinessProbe reports whether a launched engine is serving its model. It
// takes the whole spec because how to ask differs by engine.
type ReadinessProbe func(ctx context.Context, spec LaunchSpec) error

type Launcher struct {
	stateDir string
	probe    ReadinessProbe

	mu      sync.Mutex
	running map[string]*child // keyed by deployment ID
}

type child struct {
	cmd    *exec.Cmd
	handle Handle
	done   chan struct{}
	// exitErr is written once, before done is closed.
	exitErr error
	stdout  *os.File
	stderr  *os.File
}

func NewLauncher(stateDir string, probe ReadinessProbe) (*Launcher, error) {
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return nil, fmt.Errorf("create state dir: %w", err)
	}
	return &Launcher{stateDir: stateDir, probe: probe, running: map[string]*child{}}, nil
}

// record is the on-disk ownership record. It is written before the spawn with
// Pending set, then rewritten once the process identity is known.
type record struct {
	DeploymentID string    `json:"deployment_id"`
	Generation   int       `json:"generation"`
	InstanceID   string    `json:"instance_id"`
	Pending      bool      `json:"pending"`
	PID          int       `json:"pid,omitempty"`
	BirthID      string    `json:"birth_id,omitempty"`
	Endpoint     string    `json:"endpoint"`
	Model        string    `json:"model"`
	Engine       string    `json:"engine,omitempty"`
	ServedModel  string    `json:"served_model,omitempty"`
	Argv         []string  `json:"argv"`
	Meta         []byte    `json:"meta,omitempty"`
	WrittenAt    time.Time `json:"written_at"`
}

// recordPath names a deployment's record file. The readable part is sanitized,
// which is lossy; "a/b" and "a_b" both became "a_b" and shared one file, so
// one deployment could overwrite another's ownership record and break the
// one-process-per-deployment guarantee. A digest of the full id disambiguates.
func (l *Launcher) recordPath(deploymentID string) string {
	sum := sha256.Sum256([]byte(deploymentID))
	return filepath.Join(l.stateDir, sanitize(deploymentID)+"-"+hex.EncodeToString(sum[:4])+".json")
}

// legacyRecordPath is the pre-digest name. Records written by an older build
// are still read, so an upgrade does not lose track of a running engine and
// spawn a second one beside it.
func (l *Launcher) legacyRecordPath(deploymentID string) string {
	return filepath.Join(l.stateDir, sanitize(deploymentID)+".json")
}

func sanitize(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			return r
		default:
			return '_'
		}
	}, s)
}

func (l *Launcher) writeRecord(r record) error {
	r.WrittenAt = time.Now().UTC()
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	path := l.recordPath(r.DeploymentID)
	tmp := path + ".tmp"
	// Flush both the file and directory entry before spawning. Rename alone
	// provides atomic replacement but does not make launch intent durable.
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	if dir, err := os.Open(l.stateDir); err == nil {
		_ = dir.Sync()
		dir.Close()
	}
	// An old record for the same deployment would otherwise linger and be
	// read by a future build that still knows the legacy name.
	if legacy := l.legacyRecordPath(r.DeploymentID); legacy != path {
		_ = os.Remove(legacy)
	}
	return nil
}

func (l *Launcher) readRecord(deploymentID string) (*record, error) {
	b, err := os.ReadFile(l.recordPath(deploymentID))
	if os.IsNotExist(err) {
		b, err = os.ReadFile(l.legacyRecordPath(deploymentID))
	}
	if err != nil {
		return nil, err
	}
	var r record
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

// PIDOf is the process recorded for a deployment, zero when none is. Start
// records it as soon as the process exists and returns only when the engine
// is ready, so this is how anything watching a start in progress finds the
// process to watch.
func (l *Launcher) PIDOf(deploymentID string) int {
	r, err := l.readRecord(deploymentID)
	if err != nil || r == nil {
		return 0
	}
	return r.PID
}

// Start launches the process and waits for readiness.
//
// At most one owned process exists per deployment: a second Start while one is
// running is refused rather than racing it.
func (l *Launcher) Start(ctx context.Context, spec LaunchSpec, instanceID string) (*Handle, error) {
	if len(spec.Argv) == 0 {
		return nil, fmt.Errorf("%w: empty argv", ErrLaunch)
	}

	l.mu.Lock()
	if _, busy := l.running[spec.DeploymentID]; busy {
		l.mu.Unlock()
		return nil, fmt.Errorf("%w: deployment %s already has an owned process",
			ErrOwnership, spec.DeploymentID)
	}
	// Require recovery before spawning when a record is pending or its process
	// is still alive; otherwise a restart can launch a duplicate engine.
	if prev, err := l.readRecord(spec.DeploymentID); err == nil {
		switch {
		case prev.Pending:
			l.mu.Unlock()
			return nil, fmt.Errorf("%w: deployment %s has an unresolved launch window; state is unknown",
				ErrOwnership, spec.DeploymentID)
		case l.recordedProcessAlive(prev):
			l.mu.Unlock()
			return nil, fmt.Errorf("%w: deployment %s is still running as pid %d; recover or stop it first",
				ErrOwnership, spec.DeploymentID, prev.PID)
		}
	}
	// Reserve the slot before releasing the lock so no concurrent Start slips in.
	l.running[spec.DeploymentID] = nil
	l.mu.Unlock()

	handle, err := l.start(ctx, spec, instanceID)
	if err != nil {
		l.mu.Lock()
		if c := l.running[spec.DeploymentID]; c == nil {
			delete(l.running, spec.DeploymentID)
		}
		l.mu.Unlock()
	}
	return handle, err
}

func (l *Launcher) start(ctx context.Context, spec LaunchSpec, instanceID string) (*Handle, error) {
	// Persist intent BEFORE the spawn. If we die immediately after fork, this
	// record is what stops a restart from launching a duplicate.
	if err := l.writeRecord(record{
		DeploymentID: spec.DeploymentID,
		Generation:   spec.Generation,
		InstanceID:   instanceID,
		Pending:      true,
		Endpoint:     spec.Endpoint,
		Model:        spec.Model,
		Engine:       spec.Engine,
		ServedModel:  spec.ServedModel,
		Argv:         spec.Argv,
		Meta:         spec.Meta,
	}); err != nil {
		return nil, fmt.Errorf("%w: persist launch intent: %v", ErrLaunch, err)
	}

	cmd := exec.Command(spec.Argv[0], spec.Argv[1:]...)
	cmd.Dir = spec.Cwd
	if len(spec.Env) > 0 {
		cmd.Env = append(os.Environ(), spec.Env...)
	}
	// Setpgid puts the engine in its own process group so we can signal the
	// whole tree; engines routinely fork workers that would otherwise survive.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	c := &child{done: make(chan struct{})}
	if spec.StdoutPath != "" {
		if f, err := os.OpenFile(spec.StdoutPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644); err == nil {
			cmd.Stdout, c.stdout = f, f
		}
	}
	if spec.StderrPath != "" {
		if f, err := os.OpenFile(spec.StderrPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644); err == nil {
			cmd.Stderr, c.stderr = f, f
		}
	}

	if err := cmd.Start(); err != nil {
		_ = l.clearRecord(spec.DeploymentID)
		c.closeLogs()
		return nil, fmt.Errorf("%w: %v", ErrLaunch, err)
	}

	pid := cmd.Process.Pid
	birth, err := birthID(pid)
	if err != nil {
		// We could not capture identity, so we cannot safely own this process.
		// Kill what we just started and leave no pending record behind.
		_ = syscall.Kill(-pid, syscall.SIGKILL)
		_ = cmd.Wait()
		_ = l.clearRecord(spec.DeploymentID)
		c.closeLogs()
		return nil, fmt.Errorf("%w: capture process identity: %v", ErrLaunch, err)
	}

	handle := Handle{
		DeploymentID: spec.DeploymentID,
		Generation:   spec.Generation,
		InstanceID:   instanceID,
		PID:          pid,
		BirthID:      birth,
		StartedAt:    time.Now().UTC().Format(time.RFC3339Nano),
		Endpoint:     spec.Endpoint,
		Model:        spec.Model,
		Engine:       spec.Engine,
		ServedModel:  spec.ServedModel,
	}

	c.cmd, c.handle = cmd, handle
	l.mu.Lock()
	l.running[spec.DeploymentID] = c
	l.mu.Unlock()

	go func() {
		c.exitErr = cmd.Wait()
		c.closeLogs()
		close(c.done)
	}()

	if err := l.writeRecord(record{
		DeploymentID: spec.DeploymentID,
		Generation:   spec.Generation,
		InstanceID:   instanceID,
		Pending:      false,
		PID:          pid,
		BirthID:      birth,
		Endpoint:     spec.Endpoint,
		Model:        spec.Model,
		Engine:       spec.Engine,
		ServedModel:  spec.ServedModel,
		Argv:         spec.Argv,
		Meta:         spec.Meta,
	}); err != nil {
		_ = l.Stop(context.Background(), &handle, spec.StopTimeout)
		return nil, fmt.Errorf("%w: persist ownership record: %v", ErrLaunch, err)
	}

	if err := l.awaitReady(ctx, c, spec); err != nil {
		_ = l.Stop(context.Background(), &handle, spec.StopTimeout)
		return nil, err
	}
	return &handle, nil
}

func (l *Launcher) awaitReady(ctx context.Context, c *child, spec LaunchSpec) error {
	timeout := spec.StartupTimeout
	if timeout <= 0 {
		timeout = 120 * time.Second
	}
	deadline := time.After(timeout)
	tick := time.NewTicker(300 * time.Millisecond)
	defer tick.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-c.done:
			// Exit before readiness is a failed launch even with status zero.
			// Include recognized engine log failures in the error.
			return explain(fmt.Errorf("%w: engine exited before readiness (%v)", ErrLaunch, c.exitErr),
				spec.StdoutPath)
		case <-deadline:
			// A timeout has a reason in the log too when the engine is stuck
			// rather than merely slow.
			return explain(fmt.Errorf("%w: engine not ready within %s", ErrLaunch, timeout),
				spec.StdoutPath)
		case <-tick.C:
			probeCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
			err := l.probe(probeCtx, spec)
			cancel()
			if err == nil {
				return nil
			}
		}
	}
}

// Inspect reports the state of a handle, verifying ownership first.
func (l *Launcher) Inspect(ctx context.Context, h *Handle) State {
	if h == nil {
		return StateUnknown
	}
	switch alive, err := l.owns(h); {
	case err != nil:
		return StateUnknown
	case !alive:
		return StateStopped
	}
	if l.probe == nil {
		return StateStarting
	}
	probeCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if err := l.probe(probeCtx, LaunchSpec{Endpoint: h.Endpoint, Model: h.Model,
		Engine: h.Engine, ServedModel: h.ServedModel}); err != nil {
		return StateStarting
	}
	return StateReady
}

// owns reports whether the PID in the handle is still the process we started.
// A live PID whose birth time differs belongs to somebody else.
// recordedProcessAlive reports whether the process a record names is still
// running. PIDs are reused, so the recorded birth id has to match too.
func (l *Launcher) recordedProcessAlive(r *record) bool {
	if r == nil || r.PID <= 0 {
		return false
	}
	birth, err := birthID(r.PID)
	if err != nil {
		return false
	}
	return r.BirthID == "" || birth == r.BirthID
}

func (l *Launcher) owns(h *Handle) (bool, error) {
	birth, err := birthID(h.PID)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if birth != h.BirthID {
		return false, nil // PID reused by an unrelated process
	}
	return true, nil
}

// Stop terminates the owned process group under a bounded-stop policy: SIGTERM
// to the group, then SIGKILL after the deadline.
//
// This is interruption, not graceful drain. llama.cpp exposes no quiescence
// signal, so we do not claim in-flight requests are preserved; callers that
// need that must settle traffic before calling Stop.
func (l *Launcher) Stop(ctx context.Context, h *Handle, timeout time.Duration) error {
	if h == nil {
		return fmt.Errorf("%w: nil handle", ErrOwnership)
	}
	owned, err := l.owns(h)
	if err != nil {
		return fmt.Errorf("%w: verify ownership: %v", ErrOwnership, err)
	}
	if !owned {
		// Either already gone or the PID was reused. Refusing is the whole
		// point: stopping a stranger's process would be worse than a no-op.
		l.forget(h)
		return nil
	}

	l.mu.Lock()
	c := l.running[h.DeploymentID]
	l.mu.Unlock()
	if c != nil && c.handle.InstanceID != h.InstanceID {
		return fmt.Errorf("%w: handle is for instance %s but %s is running (stale generation)",
			ErrOwnership, h.InstanceID, c.handle.InstanceID)
	}

	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	// Negative PID signals the whole process group, catching forked workers.
	_ = syscall.Kill(-h.PID, syscall.SIGTERM)
	if l.waitExit(h, c, timeout, ctx) {
		l.forget(h)
		return nil
	}

	_ = syscall.Kill(-h.PID, syscall.SIGKILL)
	if !l.waitExit(h, c, 5*time.Second, ctx) {
		return fmt.Errorf("%w: process %d survived SIGKILL", ErrOwnership, h.PID)
	}
	l.forget(h)
	return nil
}

// waitExit reports whether the process is gone within d.
//
// The two cases need different evidence. For a process we spawned, c.done
// closes once cmd.Wait reaps it; polling /proc would see a zombie and wrongly
// conclude it is still alive. For an adopted process we are not the parent, so
// there is no Wait to close anything and /proc is the only truth.
func (l *Launcher) waitExit(h *Handle, c *child, d time.Duration, ctx context.Context) bool {
	if c != nil && c.cmd != nil {
		select {
		case <-c.done:
			return true
		case <-time.After(d):
			return false
		case <-ctx.Done():
			return false
		}
	}
	return waitGone(h, l, d)
}

// Wait blocks until the process behind h exits or ctx ends, and describes how
// it ended ("" when ctx ended first). A spawned child is reaped by cmd.Wait,
// so its exit status is known; an adopted process is not our child, so it is
// polled and its status is not.
func (l *Launcher) Wait(ctx context.Context, h *Handle) string {
	l.mu.Lock()
	c := l.running[h.DeploymentID]
	l.mu.Unlock()
	if c != nil && c.cmd != nil && c.handle.InstanceID == h.InstanceID {
		select {
		case <-c.done:
			if c.exitErr == nil {
				return "exited with status 0"
			}
			return c.exitErr.Error()
		case <-ctx.Done():
			return ""
		}
	}
	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return ""
		case <-tick.C:
			if owned, err := l.owns(h); err == nil && !owned {
				return "exited (adopted process; status unknown)"
			}
		}
	}
}

func waitGone(h *Handle, l *Launcher, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if owned, err := l.owns(h); err == nil && !owned {
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return false
}

func (l *Launcher) forget(h *Handle) {
	l.mu.Lock()
	delete(l.running, h.DeploymentID)
	l.mu.Unlock()
	_ = l.clearRecord(h.DeploymentID)
}

func (l *Launcher) clearRecord(deploymentID string) error {
	if legacy := l.legacyRecordPath(deploymentID); legacy != l.recordPath(deploymentID) {
		_ = os.Remove(legacy)
	}
	err := os.Remove(l.recordPath(deploymentID))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// Recover inspects the persisted record for a deployment after a host restart.
//
// A record with proven identity is adopted. A pending record is an unresolved
// launch window: we report StateUnknown and keep the record, which blocks
// another launch until an operator settles it.
func (l *Launcher) Recover(deploymentID string) (*Handle, State) {
	r, err := l.readRecord(deploymentID)
	if err != nil {
		return nil, StateStopped
	}
	if r.Pending || r.PID == 0 || r.BirthID == "" {
		return nil, StateUnknown
	}
	h := &Handle{
		DeploymentID: r.DeploymentID,
		Generation:   r.Generation,
		InstanceID:   r.InstanceID,
		PID:          r.PID,
		BirthID:      r.BirthID,
		Endpoint:     r.Endpoint,
		Model:        r.Model,
		Engine:       r.Engine,
		ServedModel:  r.ServedModel,
	}
	owned, err := l.owns(h)
	if err != nil {
		return nil, StateUnknown
	}
	if !owned {
		_ = l.clearRecord(deploymentID)
		return nil, StateStopped
	}
	// Adopted: re-register so Stop can find it. cmd is nil because this process
	// is not our child any more, so Stop falls back to polling for exit.
	l.mu.Lock()
	l.running[deploymentID] = &child{handle: *h, done: make(chan struct{})}
	l.mu.Unlock()
	return h, StateStarting
}

type Recovered struct {
	Handle *Handle
	State  State
	Meta   []byte
}

// RecoverAll inspects every persisted ownership record.
//
// Called once at startup: a node that was killed rather than stopped leaves
// engines running and holding GPU memory. Adopting them is what lets the host
// manage or stop them instead of leaking them.
func (l *Launcher) RecoverAll() []Recovered {
	entries, err := os.ReadDir(l.stateDir)
	if err != nil {
		return nil
	}
	var out []Recovered
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		b, err := os.ReadFile(filepath.Join(l.stateDir, e.Name()))
		if err != nil {
			continue
		}
		var r record
		if err := json.Unmarshal(b, &r); err != nil {
			continue
		}
		h, state := l.Recover(r.DeploymentID)
		out = append(out, Recovered{Handle: h, State: state, Meta: r.Meta})
	}
	return out
}

// ClearUnresolved discards an unresolved launch window after an operator has
// confirmed no orphan is running. Explicit, never automatic.
func (l *Launcher) ClearUnresolved(deploymentID string) error {
	return l.clearRecord(deploymentID)
}

func (c *child) closeLogs() {
	if c.stdout != nil {
		_ = c.stdout.Close()
	}
	if c.stderr != nil {
		_ = c.stderr.Close()
	}
}

// birthID returns a stable identity for a running PID: its start time, which
// the OS reports differently on each platform (see internal/osproc).
//
// Reusing a PID requires wrapping the whole PID space, and the replacement
// would have a different start time, so (pid, birthID) is unique in practice.
func birthID(pid int) (string, error) { return osproc.BirthID(pid) }
