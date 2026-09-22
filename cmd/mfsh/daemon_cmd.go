package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/gregbugaj/modelfabric/internal/config"
	"github.com/gregbugaj/modelfabric/internal/osproc"
)

// `mfsh up` / `mfsh down` manage a background node, so `mfsh status` on a fresh
// shell has something to talk to. The pid file records the process birth time
// alongside the pid for the same reason the supervisor does: a pid on its own
// is not an identity once pids get reused.

type daemonRecord struct {
	PID     int    `json:"pid"`
	BirthID string `json:"birth_id"`
	Listen  string `json:"listen"`
	Started string `json:"started"`
}

func daemonRecordPath() string { return filepath.Join(defaultStateDir(), "node.json") }

func readDaemonRecord() (*daemonRecord, error) { return readRecord(daemonRecordPath()) }

// listenRecordPath is where a running node says which address it holds,
// however it was started. node.json is `mfsh up`'s alone — `mfsh down` stops
// what it names — so a foreground `mfsh serve` must not write there; without
// a record of its own, a node on -port 3000 was invisible to every command.
func listenRecordPath() string { return filepath.Join(defaultStateDir(), "listen.json") }

// writeListenRecord records this process as listening on listen, and returns
// what removes the record again — only while it is still this process's, so a
// node exiting late cannot delete the record of the one that replaced it.
func writeListenRecord(listen string) (func(), error) {
	pid := os.Getpid()
	birth, err := osproc.BirthID(pid)
	if err != nil {
		return func() {}, err
	}
	b, _ := json.MarshalIndent(daemonRecord{
		PID: pid, BirthID: birth, Listen: listen, Started: time.Now().UTC().Format(time.RFC3339),
	}, "", "  ")
	if err := os.MkdirAll(filepath.Dir(listenRecordPath()), 0o755); err != nil {
		return func() {}, err
	}
	if err := os.WriteFile(listenRecordPath(), b, 0o644); err != nil {
		return func() {}, err
	}
	return func() {
		if r, err := readRecord(listenRecordPath()); err == nil && r.PID == pid && r.BirthID == birth {
			_ = os.Remove(listenRecordPath())
		}
	}, nil
}

// runningListen is the address of a live local node: the one that recorded
// itself, else the one `mfsh up` started (a node from a build that predates
// listen.json). A record whose process is gone says nothing.
func runningListen() string {
	for _, path := range []string{listenRecordPath(), daemonRecordPath()} {
		if r, err := readRecord(path); err == nil && daemonAlive(r) && r.Listen != "" {
			return r.Listen
		}
	}
	return ""
}

func readRecord(path string) (*daemonRecord, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var r daemonRecord
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

// daemonAlive reports whether the recorded process is still the one we started.
func daemonAlive(r *daemonRecord) bool {
	if r == nil || r.PID <= 0 {
		return false
	}
	birth, err := osproc.BirthID(r.PID)
	if err != nil {
		return false
	}
	return birth == r.BirthID && !osproc.Zombie(r.PID)
}

// ensureNode starts the local node if it is not already running.
//
// `lms ls` prints "Waking up LM Studio service..." and just works; making the
// user read a dial error and then run a second command is the CLI doing less
// than it could. Only the local default address is auto-started — a remote
// -addr belongs to someone else and we must not try to manage it.
func ensureNode(addr string) error {
	if addr != defaultAddr {
		return nil
	}
	if waitHealthy(addr, 300*time.Millisecond) == nil {
		return nil
	}
	if r, err := readDaemonRecord(); err == nil && daemonAlive(r) {
		// Recorded as running but not answering yet — it is probably still
		// coming up, so give it a moment rather than starting a second one.
		return waitHealthy(addr, 10*time.Second)
	}
	fmt.Fprintln(os.Stderr, "Waking up ModelFabric node...")
	if err := startNode(defaultConfigPath(), "", false); err != nil {
		return err
	}
	return nil
}

func upCmd(args []string) error {
	fs := flag.NewFlagSet("up", flag.ExitOnError)
	cfgPath := fs.String("config", defaultConfigPath(), "path to config file")
	listen := fs.String("listen", "", "override listen address")
	port := fs.Int("port", 0, "override only the listen port, keeping the configured address")
	verbose := fs.Bool("v", false, "verbose logging")
	if err := fs.Parse(args); err != nil {
		return err
	}
	// Folded into -listen here, so the node, the daemon record and every
	// command that reads it all see the one address.
	if *port != 0 {
		addr, err := withPort(resolveListen(*cfgPath, *listen), *port)
		if err != nil {
			return err
		}
		*listen = addr
	}

	// Checking for a node and starting one is a read-modify-write on the
	// daemon record and the port. Two `mfsh up` at once — or an auto-starting
	// command racing one — could both see nothing running and both spawn, and
	// the loser would then rewrite or remove the winner's record.
	unlock, err := lockDaemonStart()
	if err != nil {
		return err
	}
	defer unlock()

	if r, err := readDaemonRecord(); err == nil && daemonAlive(r) {
		fmt.Printf("already running (pid %d, %s)\n", r.PID, r.Listen)
		return nil
	}
	// A node we have no record of (started by hand, or by an older build
	// before its state moved) still owns the port: starting another would
	// fail to bind while the old one answered the health check for it.
	base := httpBase(resolveListen(*cfgPath, *listen))
	if waitHealthy(base, 300*time.Millisecond) == nil {
		return fmt.Errorf("a node is already answering on %s but it is not the one `mfsh up` started;\n"+
			"stop it first (`ss -ltnp` shows its pid), then run `mfsh up` again", base)
	}
	if err := startNode(*cfgPath, *listen, *verbose); err != nil {
		return err
	}
	r, _ := readDaemonRecord()
	fmt.Printf("ModelFabric node up (pid %d) on %s\n", r.PID, httpBase(r.Listen))
	fmt.Printf("logs: %s\n", nodeLogPath())
	return nil
}

// startNode spawns a detached node and waits for it to answer.
func startNode(cfgPath, listen string, verbose bool) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	// Move old state before opening the log there: no node is running yet.
	migrateLegacyState(slog.New(slog.NewTextHandler(io.Discard, nil)))
	logPath := nodeLogPath()
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		return err
	}
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer logFile.Close()

	// The default path is left for serve to work out, not written into the
	// node's command line: frozen there, a default from one build outlives
	// it, and deploy.sh carries the node's flags into every restart.
	argv := []string{"serve"}
	if cfgPath != defaultConfigPath() {
		argv = append(argv, "-config", cfgPath)
	}
	if listen != "" {
		argv = append(argv, "-listen", listen)
	}
	if verbose {
		argv = append(argv, "-v")
	}

	cmd := exec.Command(self, argv...)
	cmd.Stdout, cmd.Stderr = logFile, logFile
	// Setsid detaches the node from this terminal, so closing the shell (or a
	// Ctrl-C meant for something else) does not take the node with it.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start node: %w", err)
	}

	birth, err := osproc.BirthID(cmd.Process.Pid)
	if err != nil {
		_ = cmd.Process.Kill()
		return fmt.Errorf("capture node identity: %w", err)
	}
	addr := resolveListen(cfgPath, listen)
	rec := daemonRecord{
		PID: cmd.Process.Pid, BirthID: birth,
		Listen: addr, Started: time.Now().UTC().Format(time.RFC3339),
	}
	b, _ := json.MarshalIndent(rec, "", "  ")
	// The log directory is not always inside the state directory, so this can
	// be the first thing written there: without the MkdirAll the record fails
	// and `mfsh up` reports a start that did happen as a failure.
	if err := os.MkdirAll(filepath.Dir(daemonRecordPath()), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(daemonRecordPath(), b, 0o644); err != nil {
		return err
	}
	// Release the child: it is detached and owns its own lifetime now.
	_ = cmd.Process.Release()

	base := httpBase(addr)
	if err := waitHealthy(base, 20*time.Second); err != nil {
		// The record was written and the child released before startup was
		// known to have worked. Leaving both behind meant a node that came up
		// but never answered still looked "already running" to the next
		// `mfsh up`, with nothing to stop.
		if daemonAlive(&rec) {
			if pr, ferr := os.FindProcess(rec.PID); ferr == nil {
				_ = pr.Signal(syscall.SIGTERM)
			}
		}
		_ = os.Remove(daemonRecordPath())
		return fmt.Errorf("node started (pid %d) but did not become healthy: %w\nit has been stopped; logs: %s",
			rec.PID, err, logPath)
	}
	// Healthy is only ours if our process is still alive: one that exited
	// (a port someone else holds, a bad config) leaves whatever else is
	// listening there to answer. A bind failure takes a moment to surface.
	time.Sleep(500 * time.Millisecond)
	if !daemonAlive(&rec) {
		_ = os.Remove(daemonRecordPath())
		return fmt.Errorf("node (pid %d) exited during startup while something else answers on %s\nlogs: %s",
			rec.PID, base, logPath)
	}
	return nil
}

// lockDaemonStart takes an exclusive lock for the duration of an `mfsh up`.
// The lock file lives beside the daemon record and is never removed: unlinking
// it would let a second process lock a file the first no longer holds.
func lockDaemonStart() (func(), error) {
	dir := defaultStateDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, "up.lock"), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, fmt.Errorf("lock %s: %w", f.Name(), err)
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, nil
}

// resolveListen is the address a node started with these flags listens on:
// the flag, else the config's listen, else the default serve uses. Never "",
// which callers turned into defaultAddr — the address of whatever node is
// running now (listen.json). Another node sharing the state directory, on
// 127.0.0.1:18700, then became where `mfsh up` thought this one belonged, and
// it refused to start on :1234 because 18700 was taken.
func resolveListen(cfgPath, listen string) string {
	if listen != "" {
		return listen
	}
	if addr, _ := loadListen(cfgPath); addr != "" {
		return addr
	}
	return config.Default().Listen
}

func downCmd(args []string) error {
	fs := flag.NewFlagSet("down", flag.ExitOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	r, err := readDaemonRecord()
	if err != nil {
		fmt.Println("no node is running")
		return nil
	}
	if !daemonAlive(r) {
		_ = os.Remove(daemonRecordPath())
		fmt.Println("no node is running")
		return nil
	}

	// SIGTERM so the node unloads its models and drains cleanly.
	if err := syscall.Kill(r.PID, syscall.SIGTERM); err != nil {
		return fmt.Errorf("signal node: %w", err)
	}
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if !daemonAlive(r) {
			_ = os.Remove(daemonRecordPath())
			fmt.Printf("stopped node (pid %d)\n", r.PID)
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("node %d did not stop within 60s; it may still be unloading models", r.PID)
}

func waitHealthy(base string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var last error
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		// A malformed listen address makes this fail and return a nil request,
		// which Do dereferences: the health check would panic rather than
		// report an unreachable node.
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/healthz", nil)
		if err != nil {
			cancel()
			return fmt.Errorf("health check address %q: %w", base, err)
		}
		resp, err := http.DefaultClient.Do(req)
		cancel()
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
			last = fmt.Errorf("status %s", resp.Status)
		} else {
			last = err
		}
		time.Sleep(250 * time.Millisecond)
	}
	return last
}

func httpBase(listen string) string {
	if listen == "" {
		return defaultAddr
	}
	return loopbackBase(listen)
}

// loopbackBase is listen as a URL this machine can dial.
func loopbackBase(listen string) string {
	if strings.HasPrefix(listen, "http://") || strings.HasPrefix(listen, "https://") {
		return listen
	}
	// Every wildcard form is reachable on loopback, and the comment already
	// claimed as much — but only the bare ":1234" was translated, so a health
	// check against "0.0.0.0:1234" or "[::]:1234" dialled an address that
	// cannot be connected to on some systems.
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return "http://" + listen
	}
	switch host {
	case "", "0.0.0.0", "::", "[::]":
		return "http://" + net.JoinHostPort("127.0.0.1", port)
	}
	return "http://" + listen
}

func loadListen(cfgPath string) (string, error) {
	b, err := os.ReadFile(cfgPath)
	if err != nil {
		return "", err
	}
	var c struct {
		Listen string `json:"listen"`
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return "", err
	}
	return c.Listen, nil
}

// withPort is listen with its port replaced, as `lms server start --port`
// does: the bind address stays whatever the config chose, so -port never
// widens a loopback-only listener. Only the port in listen is changed —
// mesh_port, which every node must agree on, is not.
func withPort(listen string, port int) (string, error) {
	if port < 1 || port > 65535 {
		return "", fmt.Errorf("-port %d is not a port; use 1-65535", port)
	}
	host := "127.0.0.1"
	if listen != "" {
		h, _, err := net.SplitHostPort(listen)
		if err != nil {
			return "", fmt.Errorf("listen address %q: %w; set -listen host:port instead", listen, err)
		}
		host = h
	}
	return net.JoinHostPort(host, fmt.Sprint(port)), nil
}

// pickAddr chooses the node the CLI talks to: MFSH_ADDR, else the address a
// live local node recorded (see runningListen), else the config's listen.
func pickAddr(env, running, cfgListen string) string {
	switch {
	case env != "":
		return env
	case running != "":
		return loopbackBase(running)
	case cfgListen != "":
		return loopbackBase(cfgListen)
	}
	return "http://127.0.0.1:1234"
}
