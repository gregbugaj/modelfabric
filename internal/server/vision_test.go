package server

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gregbugaj/modelfabric/internal/config"
	"github.com/gregbugaj/modelfabric/internal/mesh"
	"github.com/gregbugaj/modelfabric/internal/supervisor"
)

// The vision endpoints are how the dashboard reads and sets a node's slot
// count for models that take images, on this node and — through the peer
// proxy — on every other. A second ModelFabric cannot be started alongside a live
// one to try them by hand: it binds the mesh port. So they are exercised here.
func visionServer(t *testing.T) *Server {
	t.Helper()
	m := mesh.New(config.Default(), "testnode")
	sup := supervisor.New(supervisor.Config{DataDir: t.TempDir()}, nil, nil, nil, m, nil)
	return &Server{sup: sup, m: m}
}

func TestVisionDefaultsEndpointReportsTheSafeDefault(t *testing.T) {
	s := visionServer(t)
	rec := httptest.NewRecorder()
	s.handleGetVisionDefaults(rec, httptest.NewRequest("GET", "/api/v1/vision-defaults", nil))
	if rec.Code != 200 {
		t.Fatalf("GET: %d %s", rec.Code, rec.Body.String())
	}
	var got struct {
		Node     string `json:"node"`
		Settings struct {
			Parallel *int    `json:"parallel"`
			SpecMode *string `json:"spec_mode"`
		} `json:"settings"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Settings.Parallel == nil || *got.Settings.Parallel != 1 {
		t.Errorf("a node that has saved nothing should report one slot: %s", rec.Body)
	}
	// Not off: a vision load keeps its MTP head now (see
	// supervisor.DefaultVisionSettings).
	if got.Settings.SpecMode != nil {
		t.Errorf("speculation should be left to the runtime: %s", rec.Body)
	}
	if got.Node != "testnode" {
		t.Errorf("the reply should name the node it speaks for: %q", got.Node)
	}
}

func TestVisionDefaultsEndpointSaves(t *testing.T) {
	s := visionServer(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("PUT", "/api/v1/vision-defaults", strings.NewReader(`{"parallel":4}`))
	s.handlePutVisionDefaults(rec, req)
	if rec.Code != 200 {
		t.Fatalf("PUT: %d %s", rec.Code, rec.Body.String())
	}
	// The reply is the saved state, so the dashboard renders what is stored
	// rather than what it hoped it sent.
	if !strings.Contains(rec.Body.String(), `"parallel":4`) {
		t.Errorf("PUT should echo the saved settings: %s", rec.Body)
	}
	v, err := s.sup.VisionDefaults()
	if err != nil {
		t.Fatal(err)
	}
	if v.Parallel == nil || *v.Parallel != 4 {
		t.Errorf("not persisted: %+v", v.Parallel)
	}
}

// A bad value is refused rather than stored: the dashboard sends whatever is
// typed into the slot box.
func TestVisionDefaultsEndpointRejectsNonsense(t *testing.T) {
	s := visionServer(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("PUT", "/api/v1/vision-defaults", strings.NewReader(`{"parallel":-5}`))
	s.handlePutVisionDefaults(rec, req)
	if rec.Code != 400 {
		t.Errorf("a negative slot count should be refused, got %d %s", rec.Code, rec.Body.String())
	}
}

// Routing-only nodes expose these routes too; both used to dereference a nil
// supervisor instead of returning the same actionable error as model defaults.
func TestVisionDefaultsWithoutSupervisorReturnsUnavailable(t *testing.T) {
	for _, method := range []string{"GET", "PUT"} {
		t.Run(method, func(t *testing.T) {
			s := &Server{}
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(method, "/api/v1/vision-defaults", strings.NewReader(`{}`))
			if method == "GET" {
				s.handleGetVisionDefaults(rec, req)
			} else {
				s.handlePutVisionDefaults(rec, req)
			}
			if rec.Code != 503 || !strings.Contains(rec.Body.String(), "does not manage models") {
				t.Fatalf("got %d %s, want actionable 503", rec.Code, rec.Body)
			}
		})
	}
}
