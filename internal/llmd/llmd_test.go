package llmd

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gregbugaj/modelfabric/internal/discovery"
)

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func ep(name, addr string, port int, model string) discovery.Endpoint {
	return discovery.Endpoint{Name: name, Address: addr, Port: port,
		Labels: map[string]string{ModelLabel: model, discovery.EngineTypeLabel: discovery.EngineTypeLlamaCPP}}
}

// The EPP assumes every endpoint serves the requested model, and llama-server
// answers with whatever it has loaded — so an engine of another model in the
// list would silently answer with the wrong model. Only the model's engines
// may be written.
func TestEndpointListHoldsOnlyTheModel(t *testing.T) {
	dir := t.TempDir()
	l := New(Config{Dir: dir, Endpoints: func() []discovery.Endpoint {
		return []discovery.Endpoint{
			ep("a", "127.0.0.1", 18000, "qwen/qwen3.8-27b"),
			ep("b", "100.100.69.3", 18000, "qwen/qwen3.8-27b"),
			ep("c", "127.0.0.1", 18001, "qwen/qwen3-0.6b"),
		}
	}}, quiet())
	path := filepath.Join(dir, "endpoints.yaml")
	if _, err := l.syncEndpoints("qwen/qwen3.8-27b", path); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	out := string(b)
	if !strings.Contains(out, "100.100.69.3") || strings.Contains(out, "qwen3-0.6b") || strings.Contains(out, "18001") {
		t.Fatalf("endpoint list is not exactly the model's engines:\n%s", out)
	}
	if got := l.Status().Endpoints; len(got) != 2 {
		t.Fatalf("status endpoints = %v", got)
	}

	// A model with no engines yields an empty, valid list — the EPP then
	// answers 503 rather than routing anywhere.
	if _, err := l.syncEndpoints("nobody/serves-this", path); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(path); !strings.Contains(string(b), "endpoints:\n  []") {
		t.Fatalf("endpoints listed for a model nobody serves:\n%s", b)
	}
}

func TestInstalledNeedsBothExecutables(t *testing.T) {
	c := Config{Tools: t.TempDir()}
	if c.Installed() {
		t.Fatal("installed with nothing present")
	}
	for _, p := range []string{c.EPPPath(), c.EnvoyPath()} {
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte("x"), 0o644)
	}
	if c.Installed() {
		t.Fatal("non-executable files counted as installed")
	}
	os.Chmod(c.EPPPath(), 0o755)
	os.Chmod(c.EnvoyPath(), 0o755)
	if !c.Installed() {
		t.Fatal("both executables present but not installed")
	}
}

// A pidfile naming a process that is not ModelFabric's own binary — a reused pid —
// must never lead to a kill.
func TestReapOrphansOnlyStopsOurBinaries(t *testing.T) {
	dir := t.TempDir()
	c := Config{Dir: dir, Tools: t.TempDir()}
	l := New(c, quiet())

	bystander := exec.Command("sleep", "30")
	if err := bystander.Start(); err != nil {
		t.Fatal(err)
	}
	defer bystander.Process.Kill()
	os.WriteFile(filepath.Join(dir, "epp.pid"), []byte(fmt.Sprint(bystander.Process.Pid)), 0o644)
	l.reapOrphans()
	time.Sleep(200 * time.Millisecond)
	if _, err := os.Stat(fmt.Sprintf("/proc/%d", bystander.Process.Pid)); err != nil {
		t.Fatal("killed a process that is not ModelFabric's")
	}
	if _, err := os.Stat(filepath.Join(dir, "epp.pid")); err == nil {
		t.Fatal("stale pidfile kept")
	}

	// Our own binary (a copy of sleep at the EPP path) is stopped.
	os.MkdirAll(filepath.Dir(c.EPPPath()), 0o755)
	sleepBin, _ := exec.LookPath("sleep")
	b, _ := os.ReadFile(sleepBin)
	os.WriteFile(c.EPPPath(), b, 0o755)
	ours := exec.Command(c.EPPPath(), "30")
	ours.SysProcAttr = nil
	if err := ours.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { ours.Wait(); close(done) }()
	// Until the child's exec completes, /proc shows the parent's command
	// line, which the reaper rightly refuses to touch. A real orphan has been
	// running for a while; wait for this one to become itself.
	for i := 0; i < 100; i++ {
		if b, _ := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", ours.Process.Pid)); strings.HasPrefix(string(b), c.EPPPath()+"\x00") {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	os.WriteFile(filepath.Join(dir, "epp.pid"), []byte(fmt.Sprint(ours.Process.Pid)), 0o644)
	l.reapOrphans()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		ours.Process.Kill()
		t.Fatal("orphaned EPP not stopped")
	}
}

// Engines are usually fresh when llm-d starts, so a measurement saved during
// an earlier run must be what the next start uses — per model.
func TestPrefillCalibrationIsSavedPerModel(t *testing.T) {
	l := New(Config{Dir: t.TempDir()}, quiet())
	if got := l.savedPrefill("m"); got != 0 {
		t.Fatalf("nothing saved yet, got %v", got)
	}
	if err := l.savePrefill("m", 1234); err != nil {
		t.Fatal(err)
	}
	if err := l.savePrefill("other", 99); err != nil {
		t.Fatal(err)
	}
	if got := l.savedPrefill("m"); got != 1234 {
		t.Fatalf("saved rate = %v, want 1234", got)
	}
}

func TestCalibrationNeedsTraffic(t *testing.T) {
	srv := fakeMetrics(t, "llamacpp:prompt_tokens_total 500\nllamacpp:prompt_seconds_total 1\n")
	l := New(Config{Dir: t.TempDir(), Endpoints: func() []discovery.Endpoint { return []discovery.Endpoint{srv} }}, quiet())
	if _, ok := l.calibratePrefill("m"); ok {
		t.Fatal("500 tokens is too little history to calibrate from")
	}
	srv2 := fakeMetrics(t, "llamacpp:prompt_tokens_total 90000\nllamacpp:prompt_seconds_total 30\n")
	l = New(Config{Dir: t.TempDir(), Endpoints: func() []discovery.Endpoint { return []discovery.Endpoint{srv2} }}, quiet())
	if rate, ok := l.calibratePrefill("m"); !ok || rate != 3000 {
		t.Fatalf("calibratePrefill = %v, %v; want 3000, true", rate, ok)
	}
}

func fakeMetrics(t *testing.T, body string) discovery.Endpoint {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, body) }))
	t.Cleanup(s.Close)
	u, _ := url.Parse(s.URL)
	port, _ := strconv.Atoi(u.Port())
	return ep("e", u.Hostname(), port, "m")
}

func TestUniformSlots(t *testing.T) {
	withSlots := func(n string) discovery.Endpoint {
		e := ep("x", "127.0.0.1", 1, "m")
		if n != "" {
			e.Labels[discovery.SlotsLabel] = n
		}
		return e
	}
	for _, c := range []struct {
		slots []string
		want  int
	}{
		{[]string{"2", "2"}, 2},
		{[]string{"2", "4"}, 0}, // a mixed fleet: no single cap
		{[]string{"2", ""}, 0},  // unknown capacity somewhere
		{nil, 0},
	} {
		eps := []discovery.Endpoint{ep("other", "127.0.0.1", 9, "not-m")}
		for _, s := range c.slots {
			eps = append(eps, withSlots(s))
		}
		l := New(Config{Dir: t.TempDir(), Endpoints: func() []discovery.Endpoint { return eps }}, quiet())
		if got := l.uniformSlots("m"); got != c.want {
			t.Errorf("slots %v: uniformSlots = %d, want %d", c.slots, got, c.want)
		}
	}
}

// Another node's engine on its own loopback cannot be scheduled from here —
// the EPP would dial this machine's port of the same number. This node's own
// loopback engines, and anything on a routable address, stay.
func TestEndpointListDropsOtherNodesLoopback(t *testing.T) {
	dir := t.TempDir()
	on := func(e discovery.Endpoint, node string) discovery.Endpoint { e.Labels[NodeLabel] = node; return e }
	l := New(Config{Dir: dir, Self: func() string { return "droplet" }, Endpoints: func() []discovery.Endpoint {
		return []discovery.Endpoint{
			on(ep("mine", "127.0.0.1", 18000, "m"), "droplet"),
			on(ep("theirs-loopback", "127.0.0.1", 18001, "m"), "xpredator"),
			on(ep("theirs-tailnet", "100.100.69.3", 18000, "m"), "minion"),
		}
	}}, quiet())
	if _, err := l.syncEndpoints("m", filepath.Join(dir, "e.yaml")); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(l.Status().Endpoints, " ")
	if got != "127.0.0.1:18000 100.100.69.3:18000" {
		t.Fatalf("endpoints = %q", got)
	}
}
