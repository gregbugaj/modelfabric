package doctor

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gregbugaj/modelfabric/internal/osproc"
)

func withRecord(t *testing.T, r daemonRecord) string {
	t.Helper()
	dir := t.TempDir()
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "node.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func ownership(t *testing.T, stateDir string, pid int) Check {
	t.Helper()
	d := &doctor{
		addr:        "http://127.0.0.1:1234",
		opts:        Opts{StateDir: stateDir},
		nodeUp:      true,
		nodePID:     pid,
		nodeStarted: time.Now().Add(-time.Minute),
	}
	d.checkNodeOwnership("Processes")
	if len(d.checks) != 1 {
		t.Fatalf("expected one check, got %d", len(d.checks))
	}
	return d.checks[0]
}

// The case this section was written for: a node that answers every request but
// was not started by `mfsh up`, so `mfsh down` reports "no node is running"
// about it. Every other check called that node healthy, which was true and
// useless -- the process had to be found with `ss -ltnp`.
func TestNodeStartedOutsideMfshUpIsReported(t *testing.T) {
	got := ownership(t, t.TempDir(), os.Getpid()) // a state dir with no record
	if got.Status != StatusWarn {
		t.Errorf("status %q, want a warning: an unstoppable node reported as fine is how this hid", got.Status)
	}
	if !strings.Contains(got.Detail, "mfsh down` will not stop it") {
		t.Errorf("the detail does not say what is wrong: %q", got.Detail)
	}
	// The fix line is the whole value here -- it is what had to be worked out
	// by hand, twice.
	if !strings.Contains(got.Fix, "kill -TERM") || !strings.Contains(got.Fix, "mfsh up") {
		t.Errorf("fix should say how to stop and restart it, got %q", got.Fix)
	}
}

// A record naming another process is the same problem wearing a disguise:
// `down` would signal a stranger, or nothing.
func TestRecordNamingADifferentProcessIsReported(t *testing.T) {
	dir := withRecord(t, daemonRecord{PID: os.Getpid() + 100000, BirthID: "x", Listen: "127.0.0.1:1234"})
	got := ownership(t, dir, os.Getpid())
	if got.Status != StatusWarn {
		t.Errorf("status %q, want a warning", got.Status)
	}
	if !strings.Contains(got.Detail, "the record names pid") {
		t.Errorf("detail should name the disagreement, got %q", got.Detail)
	}
}

// A matching record is the normal case, and must not nag.
func TestManagedNodeIsReportedOK(t *testing.T) {
	pid := os.Getpid()
	birth, err := osproc.BirthID(pid)
	if err != nil {
		t.Skipf("no birth id on this platform: %v", err)
	}
	dir := withRecord(t, daemonRecord{PID: pid, BirthID: birth, Listen: "127.0.0.1:1234"})
	got := ownership(t, dir, pid)
	if got.Status != StatusOK {
		t.Fatalf("status %q (%s), want ok for a node started by `mfsh up`", got.Status, got.Detail)
	}
	if !strings.Contains(got.Detail, "mfsh down") {
		t.Errorf("the ok line should still say how to stop it, got %q", got.Detail)
	}
}

// A recorded pid whose process has been replaced must not be claimed as ours:
// pids are reused, and reporting a stranger's process as ModelFabric's is worse than
// reporting nothing.
func TestReusedPIDIsNotClaimed(t *testing.T) {
	pid := os.Getpid()
	dir := withRecord(t, daemonRecord{PID: pid, BirthID: "not-this-process", Listen: "127.0.0.1:1234"})
	got := ownership(t, dir, pid)
	if got.Status != StatusWarn {
		t.Errorf("status %q, want a warning for a reused pid", got.Status)
	}
	if !strings.Contains(got.Detail, "pid was reused") {
		t.Errorf("detail should say the pid was reused, got %q", got.Detail)
	}
}

// Ownership is a local question. Pointed at another machine, it says so rather
// than comparing this machine's record against that machine's node.
func TestOwnershipIsNotGuessedForARemoteNode(t *testing.T) {
	d := &doctor{addr: "http://100.64.0.2:1234", opts: Opts{StateDir: t.TempDir()},
		nodeUp: true, nodePID: 42, nodeStarted: time.Now()}
	d.checkNodeOwnership("Processes")
	if d.checks[0].Status != StatusInfo {
		t.Errorf("status %q, want info: a local record says nothing about a remote node", d.checks[0].Status)
	}
}

func TestPluralDoesNotProduceProcesss(t *testing.T) {
	for _, tc := range []struct {
		n          int
		word, want string
	}{
		{1, "process", "1 process"},
		{3, "process", "3 processes"},
		{2, "model", "2 models"},
		{0, "process", "0 processes"},
	} {
		if got := plural(tc.n, tc.word); got != tc.want {
			t.Errorf("plural(%d, %q) = %q, want %q", tc.n, tc.word, got, tc.want)
		}
	}
}
