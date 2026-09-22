package process

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// These tests drive real subprocesses on purpose: a mocked process proves
// nothing about OS ownership,
// process groups or PID reuse.
//
// The synthetic child is this same test binary re-executed with MFSH_FAKE_MODE
// set, which avoids a separate build step while still being a real process.

func TestMain(m *testing.M) {
	if mode := os.Getenv("MFSH_FAKE_MODE"); mode != "" {
		fakeRuntime(mode)
		return
	}
	os.Exit(m.Run())
}

// fakeRuntime implements the synthetic child behaviours under test:
// delayed readiness, exiting early, ignoring TERM, and forking a worker.
func fakeRuntime(mode string) {
	port := os.Getenv("MFSH_FAKE_PORT")
	model := os.Getenv("MFSH_FAKE_MODEL")

	switch mode {
	case "exit-early":
		// Exits successfully having never served anything.
		os.Exit(0)

	case "ignore-term":
		signal.Ignore(syscall.SIGTERM)

	case "fork":
		// Spawn a worker that also holds the port, then keep running. Both must
		// die when the group is signalled.
		cmd := exec.Command(os.Args[0])
		cmd.Env = append(os.Environ(), "MFSH_FAKE_MODE=idle")
		_ = cmd.Start()
	}

	if mode == "idle" {
		select {} // hold forever; only the process group can end this
	}

	if delay := os.Getenv("MFSH_FAKE_DELAY"); delay != "" {
		d, _ := time.ParseDuration(delay)
		time.Sleep(d)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"object": "list",
			"data":   []map[string]string{{"id": model}},
		})
	})
	srv := &http.Server{Addr: "127.0.0.1:" + port, Handler: mux}
	_ = srv.ListenAndServe()
	select {}
}

// ---- helpers ----

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	return port
}

// httpProbe is the real readiness check: the endpoint must report the model we
// expect, not merely answer.
func httpProbe(ctx context.Context, spec LaunchSpec) error {
	endpoint, model := spec.Endpoint, spec.Model
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"/v1/models", nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %s", resp.Status)
	}
	var body struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return err
	}
	for _, d := range body.Data {
		if d.ID == model {
			return nil
		}
	}
	return fmt.Errorf("endpoint does not serve %q", model)
}

func newLauncher(t *testing.T) *Launcher {
	t.Helper()
	l, err := NewLauncher(t.TempDir(), httpProbe)
	if err != nil {
		t.Fatalf("NewLauncher: %v", err)
	}
	return l
}

func fakeSpec(t *testing.T, mode string, port int, env ...string) LaunchSpec {
	t.Helper()
	return LaunchSpec{
		DeploymentID:   "dep-" + mode,
		Generation:     1,
		Argv:           []string{os.Args[0]},
		Env:            append([]string{"MFSH_FAKE_MODE=" + mode, "MFSH_FAKE_PORT=" + strconv.Itoa(port), "MFSH_FAKE_MODEL=test-model"}, env...),
		Cwd:            t.TempDir(),
		Endpoint:       fmt.Sprintf("http://127.0.0.1:%d", port),
		Model:          "test-model",
		StartupTimeout: 10 * time.Second,
		StopTimeout:    5 * time.Second,
	}
}

// ---- acceptance cases ----

func TestStartsAndReachesReady(t *testing.T) {
	l := newLauncher(t)
	port := freePort(t)
	h, err := l.Start(context.Background(), fakeSpec(t, "serve", port), "inst-1")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer l.Stop(context.Background(), h, 5*time.Second)

	if got := l.Inspect(context.Background(), h); got != StateReady {
		t.Fatalf("Inspect = %q, want ready", got)
	}
	if h.BirthID == "" {
		t.Fatal("handle has no birth identity; PID reuse could not be detected")
	}
}

// "Shell exits successfully before server readiness -> Startup fails."
func TestExitBeforeReadinessFailsLaunch(t *testing.T) {
	l := newLauncher(t)
	h, err := l.Start(context.Background(), fakeSpec(t, "exit-early", freePort(t)), "inst-1")
	if err == nil {
		l.Stop(context.Background(), h, time.Second)
		t.Fatal("expected launch failure when the engine exits before readiness")
	}
	if !errors.Is(err, ErrLaunch) {
		t.Fatalf("error = %v, want ErrLaunch", err)
	}
}

// "Port is occupied by an unrelated server -> No false readiness; unrelated
// server untouched."
func TestUnrelatedServerOnPortDoesNotSatisfyReadiness(t *testing.T) {
	port := freePort(t)
	// An unrelated server that answers, but serves a different model.
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]string{{"id": "somebody-elses-model"}},
		})
	})
	srv := &http.Server{Handler: mux}
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go srv.Serve(ln)
	defer srv.Close()

	l := newLauncher(t)
	spec := fakeSpec(t, "idle", port) // our engine never serves the model
	spec.StartupTimeout = 2 * time.Second
	h, err := l.Start(context.Background(), spec, "inst-1")
	if err == nil {
		l.Stop(context.Background(), h, time.Second)
		t.Fatal("an unrelated server on the port must not satisfy readiness")
	}

	// And the unrelated server must still be running.
	if err := httpProbe(context.Background(), LaunchSpec{
		Endpoint: fmt.Sprintf("http://127.0.0.1:%d", port), Model: "somebody-elses-model",
	}); err != nil {
		t.Fatalf("unrelated server was disturbed: %v", err)
	}
}

// "Old handle points to a reused PID -> Stop is refused."
func TestStopRefusesReusedPID(t *testing.T) {
	l := newLauncher(t)
	port := freePort(t)
	h, err := l.Start(context.Background(), fakeSpec(t, "serve", port), "inst-1")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer l.Stop(context.Background(), h, 5*time.Second)

	// Same live PID, wrong birth time: this is what PID reuse looks like.
	stale := *h
	stale.BirthID = h.BirthID + "999"

	if err := l.Stop(context.Background(), &stale, time.Second); err != nil {
		t.Fatalf("Stop on a reused PID should be a safe no-op, got %v", err)
	}
	// The real process must be untouched.
	if got := l.Inspect(context.Background(), h); got != StateReady {
		t.Fatalf("real process was killed via a stale handle; state = %q", got)
	}
}

// "Child ignores TERM or forks -> Owned process group terminates within policy."
func TestStopEscalatesPastIgnoredTERM(t *testing.T) {
	l := newLauncher(t)
	port := freePort(t)
	h, err := l.Start(context.Background(), fakeSpec(t, "ignore-term", port), "inst-1")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	start := time.Now()
	if err := l.Stop(context.Background(), h, 1*time.Second); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if took := time.Since(start); took > 10*time.Second {
		t.Fatalf("stop took %s; escalation is too slow", took)
	}
	if got := l.Inspect(context.Background(), h); got != StateStopped {
		t.Fatalf("Inspect = %q, want stopped", got)
	}
}

func TestStopKillsForkedWorkers(t *testing.T) {
	l := newLauncher(t)
	port := freePort(t)
	h, err := l.Start(context.Background(), fakeSpec(t, "fork", port), "inst-1")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	pgid, err := syscall.Getpgid(h.PID)
	if err != nil {
		t.Fatalf("Getpgid: %v", err)
	}
	if err := l.Stop(context.Background(), h, 2*time.Second); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	// Signal 0 probes for existence: the whole group must be gone.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(-pgid, 0); err != nil {
			return // group is gone
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("forked worker survived; the process group was not terminated")
}

// "Two starts for one deployment/generation -> At most one owned process."
func TestSecondStartIsRefused(t *testing.T) {
	l := newLauncher(t)
	port := freePort(t)
	spec := fakeSpec(t, "serve", port)
	h, err := l.Start(context.Background(), spec, "inst-1")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer l.Stop(context.Background(), h, 5*time.Second)

	second := spec
	second.Endpoint = fmt.Sprintf("http://127.0.0.1:%d", freePort(t))
	if _, err := l.Start(context.Background(), second, "inst-2"); !errors.Is(err, ErrOwnership) {
		t.Fatalf("second Start error = %v, want ErrOwnership", err)
	}
}

// "Host fails after spawn before complete record -> No duplicate spawn; report
// unknown."
func TestUnresolvedLaunchWindowBlocksRestart(t *testing.T) {
	dir := t.TempDir()
	l, err := NewLauncher(dir, httpProbe)
	if err != nil {
		t.Fatalf("NewLauncher: %v", err)
	}
	// Simulate a crash between persisting intent and recording identity.
	if err := l.writeRecord(record{
		DeploymentID: "dep-serve", Generation: 1, InstanceID: "inst-1", Pending: true,
	}); err != nil {
		t.Fatalf("writeRecord: %v", err)
	}

	if _, state := l.Recover("dep-serve"); state != StateUnknown {
		t.Fatalf("Recover state = %q, want unknown", state)
	}
	if _, err := l.Start(context.Background(), fakeSpec(t, "serve", freePort(t)), "inst-2"); !errors.Is(err, ErrOwnership) {
		t.Fatalf("Start over an unresolved window = %v, want ErrOwnership", err)
	}
	// Only an explicit operator action clears it.
	if err := l.ClearUnresolved("dep-serve"); err != nil {
		t.Fatalf("ClearUnresolved: %v", err)
	}
	h, err := l.Start(context.Background(), fakeSpec(t, "serve", freePort(t)), "inst-3")
	if err != nil {
		t.Fatalf("Start after clearing: %v", err)
	}
	l.Stop(context.Background(), h, 5*time.Second)
}

// "Invalid input digest -> No process is created."
func TestInvalidArgvCreatesNoProcess(t *testing.T) {
	l := newLauncher(t)
	spec := fakeSpec(t, "serve", freePort(t))
	spec.Argv = nil
	if _, err := l.Start(context.Background(), spec, "inst-1"); !errors.Is(err, ErrLaunch) {
		t.Fatalf("empty argv error = %v, want ErrLaunch", err)
	}

	spec2 := fakeSpec(t, "serve", freePort(t))
	spec2.Argv = []string{"/nonexistent/engine-binary"}
	if _, err := l.Start(context.Background(), spec2, "inst-2"); !errors.Is(err, ErrLaunch) {
		t.Fatalf("missing binary error = %v, want ErrLaunch", err)
	}
	// A failed launch must leave no record that would block the next attempt.
	if _, err := l.readRecord(spec2.DeploymentID); err == nil {
		t.Fatal("failed launch left an ownership record behind")
	}
}

func TestStopIsIdempotent(t *testing.T) {
	l := newLauncher(t)
	h, err := l.Start(context.Background(), fakeSpec(t, "serve", freePort(t)), "inst-1")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := l.Stop(context.Background(), h, 5*time.Second); err != nil {
		t.Fatalf("first Stop: %v", err)
	}
	if err := l.Stop(context.Background(), h, 5*time.Second); err != nil {
		t.Fatalf("second Stop should be a no-op, got %v", err)
	}
}

func TestBirthIDDistinguishesProcesses(t *testing.T) {
	// Our own PID has a stable birth ID.
	a, err := birthID(os.Getpid())
	if err != nil {
		t.Fatalf("birthID: %v", err)
	}
	b, err := birthID(os.Getpid())
	if err != nil {
		t.Fatalf("birthID: %v", err)
	}
	if a != b {
		t.Fatalf("birthID is not stable: %q vs %q", a, b)
	}
	if _, err := birthID(1 << 30); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("birthID for a dead PID = %v, want ErrNotExist", err)
	}
}

// An adopted process is one we did not spawn, so there is no cmd.Wait to tell
// us when it exits. Stop must poll /proc instead of waiting on a channel that
// can never close — otherwise every unload after a crash reports a false
// "survived SIGKILL" while actually having stopped the process.
func TestStopWorksOnAdoptedProcess(t *testing.T) {
	dir := t.TempDir()
	l, err := NewLauncher(dir, httpProbe)
	if err != nil {
		t.Fatalf("NewLauncher: %v", err)
	}
	port := freePort(t)
	h, err := l.Start(context.Background(), fakeSpec(t, "serve", port), "inst-1")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	// A fresh Launcher over the same state dir is what a host restart looks
	// like: the engine is still running, but it is no longer our child.
	restarted, err := NewLauncher(dir, httpProbe)
	if err != nil {
		t.Fatalf("NewLauncher: %v", err)
	}
	adopted, state := restarted.Recover(h.DeploymentID)
	if adopted == nil {
		t.Fatalf("Recover did not adopt the running engine (state %q)", state)
	}
	if adopted.PID != h.PID {
		t.Fatalf("adopted pid %d, want %d", adopted.PID, h.PID)
	}

	if err := restarted.Stop(context.Background(), adopted, 3*time.Second); err != nil {
		t.Fatalf("Stop on an adopted process: %v", err)
	}
	if err := syscall.Kill(adopted.PID, 0); err == nil {
		t.Fatal("adopted process is still running after Stop")
	}
}

// Recovery must not adopt a PID that now belongs to something else.
func TestRecoverRejectsReusedPID(t *testing.T) {
	dir := t.TempDir()
	l, err := NewLauncher(dir, httpProbe)
	if err != nil {
		t.Fatalf("NewLauncher: %v", err)
	}
	// A record pointing at a live PID with the wrong birth time.
	if err := l.writeRecord(record{
		DeploymentID: "dep", Generation: 1, InstanceID: "inst-1",
		PID: os.Getpid(), BirthID: "definitely-not-the-real-birth-time",
		Endpoint: "http://127.0.0.1:1", Model: "m",
	}); err != nil {
		t.Fatalf("writeRecord: %v", err)
	}
	h, state := l.Recover("dep")
	if h != nil || state != StateStopped {
		t.Fatalf("Recover adopted a reused PID: handle=%v state=%q", h, state)
	}
}

// An engine killed from outside — the OOM killer — must be reported, with the
// signal, so the host can withdraw it instead of routing to a dead port.
func TestWaitReportsExternalKill(t *testing.T) {
	l := newLauncher(t)
	h, err := l.Start(context.Background(), fakeSpec(t, "serve", freePort(t)), "inst-1")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer l.Stop(context.Background(), h, 5*time.Second)

	got := make(chan string, 1)
	go func() { got <- l.Wait(context.Background(), h) }()
	_ = syscall.Kill(h.PID, syscall.SIGKILL)
	select {
	case how := <-got:
		if !strings.Contains(how, "killed") {
			t.Fatalf("Wait = %q, want it to name the kill", how)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Wait did not return after the process was killed")
	}
}

func TestWaitEndsWithContext(t *testing.T) {
	l := newLauncher(t)
	h, err := l.Start(context.Background(), fakeSpec(t, "serve", freePort(t)), "inst-1")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer l.Stop(context.Background(), h, 5*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if how := l.Wait(ctx, h); how != "" {
		t.Fatalf("Wait = %q for a live process, want \"\"", how)
	}
}

// Two deployment ids that sanitize to the same string shared one record file,
// so one could overwrite the other's ownership record — and the launcher's
// one-process-per-deployment guarantee rests on that record.
func TestRecordPathsDoNotCollideAfterSanitizing(t *testing.T) {
	l := &Launcher{stateDir: t.TempDir()}
	a, b := l.recordPath("qwen/qwen3-0.6b"), l.recordPath("qwen_qwen3-0_6b")
	if a == b {
		t.Fatalf("two deployments share the record %s", a)
	}
	// The readable part survives, so the directory is still greppable.
	if !strings.Contains(filepath.Base(a), "qwen_qwen3-0_6b") {
		t.Errorf("record name lost the deployment id: %s", filepath.Base(a))
	}
	// A record written by an older build is still found.
	legacy := l.legacyRecordPath("qwen/qwen3-0.6b")
	if err := os.WriteFile(legacy, []byte(`{"deployment_id":"qwen/qwen3-0.6b","pid":1}`), 0o644); err != nil {
		t.Fatal(err)
	}
	rec, err := l.readRecord("qwen/qwen3-0.6b")
	if err != nil || rec.DeploymentID != "qwen/qwen3-0.6b" {
		t.Fatalf("legacy record not read: %v %+v", err, rec)
	}
}

// One process per deployment is the launcher's core guarantee, and it rests on
// the record. Only a *pending* record was refused, so a launcher that had not
// recovered yet — a fresh one after a node restart — would start a second
// engine beside a live one.
func TestStartRefusesWhenTheRecordedProcessIsStillRunning(t *testing.T) {
	dir := t.TempDir()
	l, err := NewLauncher(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	// This test process stands in for a previously launched engine: it is
	// certainly alive, and its birth id is whatever the OS reports.
	birth, err := birthID(os.Getpid())
	if err != nil {
		t.Skipf("no birth id on this platform: %v", err)
	}
	if err := l.writeRecord(record{DeploymentID: "m", PID: os.Getpid(), BirthID: birth}); err != nil {
		t.Fatal(err)
	}
	_, err = l.Start(context.Background(), LaunchSpec{DeploymentID: "m", Argv: []string{"/bin/true"}}, "inst-1")
	if !errors.Is(err, ErrOwnership) {
		t.Fatalf("Start returned %v; want an ownership refusal", err)
	}

	// A record for a process that is gone is not an obstacle.
	if err := l.writeRecord(record{DeploymentID: "gone", PID: 1 << 30, BirthID: "stale"}); err != nil {
		t.Fatal(err)
	}
	if l.recordedProcessAlive(&record{PID: 1 << 30, BirthID: "stale"}) {
		t.Error("a dead pid was reported as alive")
	}
}
