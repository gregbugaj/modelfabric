package server

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gregbugaj/modelfabric/internal/config"
	"github.com/gregbugaj/modelfabric/internal/mesh"
	"github.com/gregbugaj/modelfabric/internal/router"
	"github.com/gregbugaj/modelfabric/internal/runtime"
	"github.com/gregbugaj/modelfabric/internal/tuner"
)

func tuneServer(t *testing.T) *Server {
	t.Helper()
	m := mesh.New(config.Default(), "minion")
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return New(m, router.New(m, log), nil, log, nil)
}

// A second sweep on one node is the failure that produced a table of "did not
// run" for a configuration that had measured fine minutes earlier: two sweeps
// unloading the same engine, each waiting for a slot count the other had just
// replaced. The node refuses it rather than trusting callers to coordinate.
func TestTuneRefusesASecondSweepOnTheSameNode(t *testing.T) {
	s := tuneServer(t)
	if _, _, err := s.claimTune(tuner.Config{Model: "q"}); err != nil {
		t.Fatalf("first sweep refused: %v", err)
	}
	_, _, err := s.claimTune(tuner.Config{Model: "q"})
	if err == nil {
		t.Fatal("a second sweep was allowed on the same node")
	}
	if !strings.Contains(err.Error(), "already running") {
		t.Errorf("the refusal does not say why: %v", err)
	}

	// And once it ends, the node is tunable again — a guard that never
	// released would make one sweep the last one until a restart.
	s.tune.mu.Lock()
	s.tune.running = false
	s.tune.mu.Unlock()
	if _, _, err := s.claimTune(tuner.Config{Model: "q"}); err != nil {
		t.Errorf("a finished sweep still holds the node: %v", err)
	}
}

// Status is readable before anything has run, because the dashboard polls it on
// open. An empty rows array rather than null, for the same reason the model list
// does it: clients iterate it directly.
func TestTuneStatusIsReadableBeforeAnySweep(t *testing.T) {
	s := tuneServer(t)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/tune", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"rows":[]`) {
		t.Errorf(`rows should be [] before a sweep, got: %s`, rec.Body)
	}
	var st tuneStatus
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
		t.Fatal(err)
	}
	if st.Running {
		t.Error("reports a sweep running on a node that has never tuned")
	}
	if st.Node != "minion" {
		t.Errorf("node = %q, want minion: a fleet view keys the table on this", st.Node)
	}
}

// Cancelling nothing is a 409, not a silent success: a UI that shows "stopped"
// for a sweep that was never running hides the sweep that is.
func TestTuneCancelWithNothingRunningSaysSo(t *testing.T) {
	s := tuneServer(t)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/api/v1/tune", nil))
	if rec.Code != http.StatusConflict {
		t.Fatalf("cancel with nothing running: %d, want 409", rec.Code)
	}
}

// A node that manages no models — an entrypoint — cannot tune, and says which
// it is. Without a supervisor the sweep would reload nothing and measure the
// mesh instead of a machine.
func TestTuneOnANodeThatRunsNoModelsIsRefused(t *testing.T) {
	s := tuneServer(t) // built with a nil supervisor
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/tune",
		strings.NewReader(`{"model":"q"}`)))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("tune without a supervisor: %d, want 503\n%s", rec.Code, rec.Body)
	}
}

func TestSpecModeReadsBackWhatWasRunning(t *testing.T) {
	for _, tc := range []struct {
		name string
		spec bool
		kind string
		want string
	}{
		{"off", false, "", "off"},
		{"mtp", true, "draft-mtp", "mtp"},
		{"draft model", true, "draft-simple", "draft"},
	} {
		if got := specModeOf(applied(tc.spec, tc.kind)); got != tc.want {
			t.Errorf("%s: specModeOf = %q, want %q — a sweep that changes speculation "+
				"halfway measures speculation, not slots", tc.name, got, tc.want)
		}
	}
}

func applied(spec bool, kind string) runtime.Applied {
	return runtime.Applied{Speculative: spec, SpecType: kind}
}
