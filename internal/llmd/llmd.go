// Package llmd runs llm-d's scheduler in front of one model's engines:
//
//	front door ──► Envoy (127.0.0.1:8090) ──ext_proc──► EPP ──picks──► llama-server on any node
//
// Envoy forwards; the EPP decides which engine serves each request, from
// queue depth and prefix-cache locality it scores itself. ModelFabric's part is
// running both, and keeping the EPP's endpoint list equal to the engines that
// serve the model.
//
// One model only, as llm-d is designed: an inference pool is one model's
// replicas, and the EPP assumes every endpoint it is given serves the model
// requested. Given engines for two models it could answer a 27B request from
// a 0.6B engine; llama-server serves whatever it has loaded, whatever the
// request says. So the endpoint list is filtered to the one model, and every
// other model keeps its direct route.
//
// Both run as native processes supervised by the node, with no containers. Envoy is its official static release binary; the
// EPP, which llm-d publishes only as an image, is the static binary pulled out
// of that image over the registry's HTTP API. Both are pinned by digest.
package llmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gregbugaj/modelfabric/internal/discovery"
	"github.com/gregbugaj/modelfabric/internal/oci"
	"github.com/gregbugaj/modelfabric/internal/osproc"
	"github.com/gregbugaj/modelfabric/internal/rtpkg"
)

const (
	EPPVersion   = "v0.10.0"
	EnvoyVersion = "1.33.14"
)

// eppImage is llm-d's EPP image for EPPVersion, pinned by index digest.
var eppImage = oci.Ref{
	Registry:   "ghcr.io",
	Repository: "llm-d/llm-d-router-endpoint-picker",
	Digest:     "sha256:2e516fa1310da7be59b82beb1445362139597d6d553ef04d546716abe3aaaa70",
}

// envoyBinaries are Envoy's official release binaries, pinned by SHA-256.
var envoyBinaries = map[string]struct{ name, sum string }{
	"amd64": {"envoy-1.33.14-linux-x86_64", "e0d635131e34a7a1cdb4051dc9535ebf8a922687306db5bf6f9d7ee543e08ada"},
	"arm64": {"envoy-1.33.14-linux-aarch_64", "6d02f3fbb1cfa552c67605d44345e2e44eed07dae9edea4239f269e9823bf9b2"},
}

const ModelLabel = "modelfabric.sh/model"

const NodeLabel = "modelfabric.sh/node"

type Config struct {
	Dir       string // ~/.modelfabric/llmd: generated configs, endpoint list, pidfiles
	Tools     string // ~/.modelfabric/tools: where the binaries live
	LogDir    string // engine-style logs: llmd-epp.log, llmd-envoy.log
	Listen    string // Envoy's host:port, loopback by default
	EPPPort   int    // ext_proc gRPC; health is EPPPort+1
	Metrics   int    // EPP Prometheus metrics
	AdminPort int    // Envoy admin, loopback only
	Endpoints func() []discovery.Endpoint
	// Self is this node's name. Another node's engine advertised on its own
	// loopback cannot be scheduled from here: the EPP would dial this
	// machine's port of the same number instead.
	Self func() string
}

func (c Config) EPPPath() string {
	return filepath.Join(c.Tools, "llmd", "epp-"+EPPVersion, "epp")
}
func (c Config) EnvoyPath() string {
	return filepath.Join(c.Tools, "llmd", "envoy-"+EnvoyVersion, "envoy")
}

func usableBinary(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && !fi.IsDir() && fi.Mode()&0o111 != 0 && fi.Size() > 0
}

func (c Config) Installed() bool {
	for _, p := range []string{c.EPPPath(), c.EnvoyPath()} {
		if fi, err := os.Stat(p); err != nil || fi.Mode()&0o111 == 0 {
			return false
		}
	}
	return true
}

// Install fetches both binaries, each verified against its pin.
func (c Config) Install(ctx context.Context, progress func(what string, done, total int64)) error {
	if goruntime.GOOS != "linux" {
		return fmt.Errorf("llm-d binaries are fetched for Linux; this is %s", goruntime.GOOS)
	}
	// Use Installed's executable check so an incomplete or non-executable
	// file does not permanently prevent reinstallation.
	if !usableBinary(c.EPPPath()) {
		if err := oci.ExtractFile(ctx, eppImage, goruntime.GOARCH, "app/epp", c.EPPPath(),
			func(done, total int64) { progress("EPP", done, total) }); err != nil {
			return fmt.Errorf("fetch the EPP from its image: %w", err)
		}
	}
	if !usableBinary(c.EnvoyPath()) {
		bin, ok := envoyBinaries[goruntime.GOARCH]
		if !ok {
			return fmt.Errorf("no pinned Envoy for %s", goruntime.GOARCH)
		}
		asset := rtpkg.Asset{
			Name:   bin.name,
			URL:    "https://github.com/envoyproxy/envoy/releases/download/v" + EnvoyVersion + "/" + bin.name,
			SHA256: bin.sum,
		}
		if err := os.MkdirAll(filepath.Dir(c.EnvoyPath()), 0o755); err != nil {
			return err
		}
		tmp := c.EnvoyPath() + ".download"
		if err := rtpkg.Download(ctx, asset, tmp, func(_ string, done, total int64) { progress("Envoy", done, total) }); err != nil {
			return fmt.Errorf("fetch Envoy: %w", err)
		}
		if err := os.Chmod(tmp, 0o755); err != nil {
			return err
		}
		if err := os.Rename(tmp, c.EnvoyPath()); err != nil {
			return err
		}
	}
	return nil
}

type Status struct {
	State       string  `json:"state"` // disabled | starting | running | restarting
	Model       string  `json:"model,omitempty"`
	Profile     string  `json:"profile,omitempty"`
	PeakPrefill float64 `json:"peak_prefill_tok_s,omitempty"` // calibration used
	Calibrated  bool    `json:"calibrated"`                   // measured, not the default
	// PrefillSource is where PeakPrefill came from: "engines" (measured at
	// start), "saved" (an earlier run's measurement) or "default".
	PrefillSource string `json:"prefill_source,omitempty"`
	// MeasuredPrefill is the rate measured while running; it is saved and
	// used from the next enable, since applying it means restarting the EPP.
	MeasuredPrefill float64 `json:"measured_prefill_tok_s,omitempty"`
	PrefixCache     bool    `json:"prefix_cache"`
	// RoomFilter drops engines with no free slot before any other decision;
	// it uses an Alpha llm-d plugin, so the EPP runs with experimental
	// plugins allowed (see discovery.roomFilter).
	RoomFilter bool      `json:"room_filter"`
	KVCeiling  float64   `json:"kv_ceiling,omitempty"`
	KVScorer   int       `json:"kv_scorer,omitempty"`
	URL        string    `json:"url,omitempty"`
	Endpoints  []string  `json:"endpoints,omitempty"`
	Since      time.Time `json:"since,omitempty"`
	Restarts   int       `json:"restarts"`
	Error      string    `json:"error,omitempty"`
	Installed  bool      `json:"installed"`
}

type LLMD struct {
	cfg Config
	log *slog.Logger

	// opMu serializes Start/Stop. Start calls Stop and then installs new
	// state, so two concurrent Starts could both stop, then both launch.
	opMu   sync.Mutex
	mu     sync.Mutex
	status Status
	cancel context.CancelFunc
	done   chan struct{}
}

// Timings; variables so tests can shorten them.
var (
	readyTimeout     = 2 * time.Minute
	pollEvery        = 2 * time.Second
	recalibrateEvery = time.Minute
	stopTimeout      = 15 * time.Second
	maxBackoff       = time.Minute
)

func New(cfg Config, log *slog.Logger) *LLMD {
	return &LLMD{cfg: cfg, log: log, status: Status{State: "disabled"}}
}

func (l *LLMD) Config() Config { return l.cfg }

// URL is the OpenAI base the front door sends the model to.
func (l *LLMD) URL() string { return "http://" + l.cfg.Listen + "/v1" }

func (l *LLMD) Status() Status {
	l.mu.Lock()
	defer l.mu.Unlock()
	s := l.status
	// Endpoints is a slice, so a shallow copy handed the caller the same
	// backing array this keeps mutating.
	s.Endpoints = append([]string(nil), l.status.Endpoints...)
	s.Installed = l.cfg.Installed()
	return s
}

func (l *LLMD) set(f func(*Status)) {
	l.mu.Lock()
	defer l.mu.Unlock()
	f(&l.status)
}

type Options struct {
	// Profile names a scheduling profile (discovery.Profiles); empty is
	// load-aware.
	Profile     string
	PrefixCache bool
	RoomFilter  bool
	// KVCeiling refuses engines whose KV cache is fuller than this share, and
	// KVScorer weights llm-d's kv-cache-utilization-scorer. Both read the
	// gauge ModelFabric synthesizes; see discovery.EPPOptions for why neither is a
	// load-balancing knob on llama.cpp.
	KVCeiling float64
	KVScorer  int
}

// Start runs llm-d for one model until Stop. Starting again (another model or
// options) restarts it.
func (l *LLMD) Start(model string, o Options) {
	l.opMu.Lock()
	defer l.opMu.Unlock()
	l.stop()
	if o.Profile == "" {
		o.Profile = discovery.ProfileLoadAware
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	l.mu.Lock()
	l.cancel, l.done = cancel, done
	l.status = Status{State: "starting", Model: model, Profile: o.Profile, PrefixCache: o.PrefixCache,
		KVCeiling: o.KVCeiling, KVScorer: o.KVScorer,
		RoomFilter: o.RoomFilter, URL: l.URL()}
	l.mu.Unlock()
	// run closes the channel it was handed, so a restart cannot have the old
	// goroutine close the new one's channel and panic the next exit.
	go l.run(ctx, model, o, done)
}

func (l *LLMD) Stop() {
	l.opMu.Lock()
	defer l.opMu.Unlock()
	l.stop()
	l.set(func(s *Status) { *s = Status{State: "disabled"} })
}

// stop ends a running llm-d. The caller holds opMu.
func (l *LLMD) stop() {
	l.mu.Lock()
	cancel, done := l.cancel, l.done
	l.mu.Unlock()
	if cancel == nil {
		return
	}
	cancel()
	<-done
	l.mu.Lock()
	l.cancel, l.done = nil, nil
	l.mu.Unlock()
}

func (l *LLMD) run(ctx context.Context, model string, o Options, done chan struct{}) {
	defer close(done)
	backoff := time.Second
	for {
		started := time.Now()
		err := l.serve(ctx, model, o)
		if ctx.Err() != nil {
			return
		}
		if time.Since(started) > 5*time.Minute {
			backoff = time.Second
		}
		l.log.Warn("llm-d stopped; restarting", "err", err, "in", backoff)
		l.set(func(s *Status) { s.State, s.Error = "restarting", err.Error(); s.Restarts++ })
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, maxBackoff)
	}
}

func (l *LLMD) serve(ctx context.Context, model string, o Options) error {
	profile := o.Profile
	if !l.cfg.Installed() {
		return errors.New("llm-d is not installed; run `mfsh llmd install`")
	}
	for _, dir := range []string{l.cfg.Dir, l.cfg.LogDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	host, port, err := net.SplitHostPort(l.cfg.Listen)
	if err != nil {
		return fmt.Errorf("llm-d listen %q: %w", l.cfg.Listen, err)
	}
	listenPort, _ := strconv.Atoi(port)
	endpointsPath := filepath.Join(l.cfg.Dir, "endpoints.yaml")
	eppConfig := filepath.Join(l.cfg.Dir, "epp.yaml")
	envoyConfig := filepath.Join(l.cfg.Dir, "envoy.yaml")
	peak, measured := l.calibratePrefill(model)
	source := "engines"
	if !measured {
		source = "default"
		if saved := l.savedPrefill(model); saved > 0 {
			peak, measured, source = saved, true, "saved"
		}
	}
	l.set(func(s *Status) { s.PeakPrefill, s.Calibrated, s.PrefillSource = peak, measured, source })
	if err := os.WriteFile(eppConfig, discovery.EPPConfig(discovery.EPPOptions{
		EndpointsPath: endpointsPath, Profile: profile, PrefixCache: o.PrefixCache,
		PeakPrefillThroughput: peak, RoomFilter: o.RoomFilter, Slots: l.uniformSlots(model),
		KVCeiling: o.KVCeiling, KVScorer: o.KVScorer,
	}), 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(envoyConfig, discovery.EnvoyConfig(discovery.EnvoyOptions{
		ListenAddress: host, ListenPort: listenPort, EPPPort: l.cfg.EPPPort, AdminPort: l.cfg.AdminPort,
	}), 0o644); err != nil {
		return err
	}
	if _, err := l.syncEndpoints(model, endpointsPath); err != nil {
		return err
	}

	l.reapOrphans()
	for _, addr := range []string{l.cfg.Listen, net.JoinHostPort("127.0.0.1", strconv.Itoa(l.cfg.EPPPort))} {
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			return fmt.Errorf("llm-d port %s is in use; set llmd_listen or free it", addr)
		}
		ln.Close()
	}

	args := []string{"--config-file", eppConfig, "--pool-name", "mfsh", "--pool-namespace", "mfsh",
		"--grpc-port", strconv.Itoa(l.cfg.EPPPort), "--grpc-health-port", strconv.Itoa(l.cfg.EPPPort + 1),
		"--metrics-port", strconv.Itoa(l.cfg.Metrics)}
	if p, _ := discovery.ProfileByName(profile); p.Experimental || o.RoomFilter {
		// Only when an Alpha plugin is in use; a profile that says so, or
		// the room filter (utilization-filter is Alpha in v0.10.0).
		args = append(args, "--allow-experimental-plugins")
	}
	epp, err := l.start("epp", l.cfg.EPPPath(), args...)
	if err != nil {
		return fmt.Errorf("start EPP: %w", err)
	}
	defer epp.stop()
	// --disable-hot-restart: no shared-memory region, so this Envoy cannot
	// collide with any other Envoy on the machine.
	envoy, err := l.start("envoy", l.cfg.EnvoyPath(), "-c", envoyConfig, "--disable-hot-restart", "--log-level", "warn")
	if err != nil {
		return fmt.Errorf("start Envoy: %w", err)
	}
	defer envoy.stop()
	l.log.Info("llm-d starting", "model", model, "profile", profile, "peak_prefill", peak, "calibrated", measured,
		"listen", l.cfg.Listen,
		"epp_pid", epp.cmd.Process.Pid, "envoy_pid", envoy.cmd.Process.Pid)

	if err := waitReady(ctx, "http://"+l.cfg.Listen+"/", epp, envoy); err != nil {
		return err
	}
	l.set(func(s *Status) { s.State, s.Since, s.Error = "running", time.Now(), "" })
	l.log.Info("llm-d ready", "model", model, "url", l.URL())

	tick := time.NewTicker(pollEvery)
	defer tick.Stop()
	// Engines are usually freshly loaded when llm-d starts, so the start-up
	// calibration mostly sees no traffic. Re-measure as traffic accrues and
	// save it for the next start.
	recal := time.NewTicker(recalibrateEvery)
	defer recal.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-epp.exited:
			return epp.failure()
		case <-envoy.exited:
			return envoy.failure()
		case <-recal.C:
			if rate, ok := l.calibratePrefill(model); ok {
				l.set(func(s *Status) { s.MeasuredPrefill = rate })
				if err := l.savePrefill(model, rate); err != nil {
					l.log.Warn("llm-d prefill calibration not saved", "err", err)
				}
			}
		case <-tick.C:
			if _, err := l.syncEndpoints(model, endpointsPath); err != nil {
				l.log.Warn("llm-d endpoint list not updated", "err", err)
			}
		}
	}
}

// calibratePrefill measures how fast this model's engines prefill, from
// llama.cpp's own counters (prompt_tokens_total / prompt_seconds_total), for
// the affinity filter's TTFT estimate. llm-d's default is calibrated for a
// 32B model on two H100s; several times faster than a workstation GPU under
// llama.cpp, which would make every queue look short. Too little history
// yields the conservative default.
func (l *LLMD) calibratePrefill(model string) (float64, bool) {
	if l.cfg.Endpoints == nil {
		return discovery.DefaultPeakPrefill, false
	}
	client := &http.Client{Timeout: 2 * time.Second}
	var tokens, seconds float64
	for _, e := range l.cfg.Endpoints() {
		if e.Labels[ModelLabel] != model {
			continue
		}
		resp, err := client.Get("http://" + net.JoinHostPort(e.Address, strconv.Itoa(e.Port)) + "/metrics")
		if err != nil {
			continue
		}
		t, s := promCounters(resp.Body)
		resp.Body.Close()
		tokens, seconds = tokens+t, seconds+s
	}
	if tokens < 20000 || seconds <= 0 {
		return discovery.DefaultPeakPrefill, false
	}
	return float64(int(tokens / seconds)), true
}

// uniformSlots is the slot count every engine serving model shares, or zero
// when they differ or any is unknown; the EPP's in-flight cap is one number
// for the pool, so a mixed fleet gets only the per-engine queue condition.
func (l *LLMD) uniformSlots(model string) int {
	if l.cfg.Endpoints == nil {
		return 0
	}
	slots := 0
	for _, e := range l.cfg.Endpoints() {
		if e.Labels[ModelLabel] != model {
			continue
		}
		n, err := strconv.Atoi(e.Labels[discovery.SlotsLabel])
		if err != nil || n <= 0 || (slots != 0 && n != slots) {
			return 0
		}
		slots = n
	}
	return slots
}

func (l *LLMD) calibrationPath() string { return filepath.Join(l.cfg.Dir, "prefill-calibration.json") }

func (l *LLMD) savedPrefill(model string) float64 {
	var saved map[string]float64
	if b, err := os.ReadFile(l.calibrationPath()); err == nil {
		_ = json.Unmarshal(b, &saved)
	}
	return saved[model]
}

func (l *LLMD) savePrefill(model string, rate float64) error {
	saved := map[string]float64{}
	if b, err := os.ReadFile(l.calibrationPath()); err == nil {
		_ = json.Unmarshal(b, &saved)
	}
	saved[model] = rate
	b, err := json.MarshalIndent(saved, "", "  ")
	if err != nil {
		return err
	}
	tmp := l.calibrationPath() + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, l.calibrationPath())
}

func promCounters(r interface{ Read([]byte) (int, error) }) (tokens, seconds float64) {
	b := make([]byte, 0, 64<<10)
	buf := make([]byte, 16<<10)
	for len(b) < 1<<20 {
		n, err := r.Read(buf)
		b = append(b, buf[:n]...)
		if err != nil {
			break
		}
	}
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) != 2 {
			continue
		}
		v, err := strconv.ParseFloat(f[1], 64)
		if err != nil {
			continue
		}
		switch f[0] {
		case "llamacpp:prompt_tokens_total":
			tokens = v
		case "llamacpp:prompt_seconds_total":
			seconds = v
		}
	}
	return tokens, seconds
}

// syncEndpoints writes the model's engines to the EPP's endpoint list. Only
// engines serving this model are included; the EPP assumes they all do.
func (l *LLMD) syncEndpoints(model, path string) (bool, error) {
	var keep []discovery.Endpoint
	var names []string
	if l.cfg.Endpoints != nil {
		for _, e := range l.cfg.Endpoints() {
			if e.Labels[ModelLabel] != model {
				continue
			}
			if e.Unreachable() && l.cfg.Self != nil && e.Labels[NodeLabel] != l.cfg.Self() {
				continue
			}
			// The EPP dials engines itself and sends the model's own name, so
			// an engine that answers to a different id cannot be scheduled by
			// llm-d at all. ModelFabric still routes to it.
			if e.NeedsRewrite() {
				continue
			}
			keep = append(keep, e)
			names = append(names, net.JoinHostPort(e.Address, strconv.Itoa(e.Port)))
		}
	}
	l.set(func(s *Status) { s.Endpoints = names })
	return discovery.WriteFile(path, keep)
}

/* ---------- processes ---------- */

type proc struct {
	name    string
	cmd     *exec.Cmd
	exited  chan struct{} // closed on exit; err is valid after
	err     error
	logPath string
	pidPath string
}

func (l *LLMD) start(name, bin string, args ...string) (*proc, error) {
	p := &proc{
		name:    name,
		exited:  make(chan struct{}),
		logPath: filepath.Join(l.cfg.LogDir, "llmd-"+name+".log"),
		pidPath: filepath.Join(l.cfg.Dir, name+".pid"),
	}
	logf, err := os.OpenFile(p.logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	fmt.Fprintf(logf, "\n--- ModelFabric starting %s, %s ---\n", name, time.Now().Format(time.RFC3339))
	p.cmd = exec.Command(bin, args...)
	p.cmd.Dir = l.cfg.Dir
	p.cmd.Stdout, p.cmd.Stderr = logf, logf
	// Own process group, stopped with the node if it dies (these are cheap
	// to start again, unlike engines, which are adopted).
	p.cmd.SysProcAttr = osproc.DieWithParent()
	if err := p.cmd.Start(); err != nil {
		logf.Close()
		return nil, err
	}
	_ = os.WriteFile(p.pidPath, []byte(strconv.Itoa(p.cmd.Process.Pid)+"\n"), 0o644)
	go func() {
		p.err = p.cmd.Wait()
		logf.Close()
		close(p.exited)
	}()
	return p, nil
}

func (p *proc) stop() {
	select {
	case <-p.exited:
	default:
		_ = syscall.Kill(-p.cmd.Process.Pid, syscall.SIGTERM)
		select {
		case <-p.exited:
		case <-time.After(stopTimeout):
			_ = syscall.Kill(-p.cmd.Process.Pid, syscall.SIGKILL)
			<-p.exited
		}
	}
	_ = os.Remove(p.pidPath)
}

func (p *proc) failure() error {
	b, _ := os.ReadFile(p.logPath)
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	if len(lines) > 4 {
		lines = lines[len(lines)-4:]
	}
	return fmt.Errorf("%s exited (%v): %s — see %s", p.name, p.err, strings.Join(lines, " | "), p.logPath)
}

// reapOrphans stops an EPP or Envoy left by a node that was killed outright
// ; only when its command line runs ModelFabric's own binary, never another
// process that reused the pid.
func (l *LLMD) reapOrphans() {
	for name, bin := range map[string]string{"epp": l.cfg.EPPPath(), "envoy": l.cfg.EnvoyPath()} {
		pidPath := filepath.Join(l.cfg.Dir, name+".pid")
		b, err := os.ReadFile(pidPath)
		if err != nil {
			continue
		}
		pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
		argv, rerr := osproc.Argv(pid)
		if err == nil && pid > 1 && rerr == nil && len(argv) > 0 && argv[0] == bin {
			l.log.Warn("stopping an llm-d process left by a previous node", "process", name, "pid", pid)
			// The group first (ModelFabric starts each as a group leader), and the
			// process itself in case it is not one.
			_ = syscall.Kill(-pid, syscall.SIGTERM)
			_ = syscall.Kill(pid, syscall.SIGTERM)
			for i := 0; i < 50 && syscall.Kill(pid, 0) == nil; i++ {
				time.Sleep(100 * time.Millisecond)
			}
			_ = syscall.Kill(-pid, syscall.SIGKILL)
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
		_ = os.Remove(pidPath)
	}
}

// waitReady waits for Envoy to answer at all: any HTTP response means the
// listener is up (with no endpoints the EPP answers 503, which is still up).
func waitReady(ctx context.Context, url string, procs ...*proc) error {
	deadline := time.Now().Add(readyTimeout)
	client := &http.Client{Timeout: 2 * time.Second}
	for time.Now().Before(deadline) {
		for _, p := range procs {
			select {
			case <-p.exited:
				return p.failure()
			default:
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(300 * time.Millisecond):
		}
		if resp, err := client.Get(url); err == nil {
			resp.Body.Close()
			return nil
		}
	}
	return fmt.Errorf("Envoy did not answer on %s within %s", url, readyTimeout)
}
