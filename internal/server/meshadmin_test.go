package server

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gregbugaj/modelfabric/internal/config"
	"github.com/gregbugaj/modelfabric/internal/mesh"
	"github.com/gregbugaj/modelfabric/internal/router"
	"github.com/gregbugaj/modelfabric/internal/tsid"
)

func adminServer(t *testing.T, policy string) *Server {
	t.Helper()
	m := mesh.New(config.Default(), "minion")
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	s := New(m, router.New(m, log), nil, log, nil)
	r := tsid.New()
	r.Run = func(_ context.Context, args ...string) ([]byte, error) {
		switch {
		case args[0] == "status":
			return []byte(`{"Self":{"HostName":"minion","UserID":7},"User":{"7":{"LoginName":"greg@github"}}}`), nil
		case args[2] == "100.0.0.1": // the owner's other machine
			return []byte(`{"Node":{"ComputedName":"xpredator","User":7},"UserProfile":{"LoginName":"greg@github"}}`), nil
		case args[2] == "100.0.0.2": // someone sharing the tailnet
			return []byte(`{"Node":{"ComputedName":"guest","User":9},"UserProfile":{"LoginName":"sam@github"}}`), nil
		}
		return nil, errors.New("unknown")
	}
	s.SetMeshAdmin(r, policy)
	return s
}

func peerGet(s *Server, from, path string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.RemoteAddr = from + ":40000"
	rec := httptest.NewRecorder()
	s.PeerHandler().ServeHTTP(rec, req)
	return rec
}

func TestMeshManagementIsForTheOwnersDevices(t *testing.T) {
	s := adminServer(t, "")
	// Past the gate (this test server manages no models, so the handler
	// itself answers 503).
	if rec := peerGet(s, "100.0.0.1", "/api/v1/presets"); rec.Code == 403 {
		t.Errorf("owner's device refused: %s", rec.Body)
	}
	if rec := peerGet(s, "100.0.0.2", "/api/v1/presets"); rec.Code != 403 {
		t.Errorf("another user's device: %d", rec.Code)
	}
	if rec := peerGet(s, "100.0.0.3", "/api/v1/presets"); rec.Code != 403 {
		t.Errorf("an unidentified address: %d", rec.Code)
	}
	// The proxy onward and the dashboard are never served over the mesh.
	for _, p := range []string{"/api/v1/nodes/other/presets", "/"} {
		if rec := peerGet(s, "100.0.0.1", p); rec.Code != 403 {
			t.Errorf("%s over the mesh: %d", p, rec.Code)
		}
	}
	off := adminServer(t, "off")
	if rec := peerGet(off, "100.0.0.1", "/api/v1/presets"); rec.Code != 403 {
		t.Errorf("mesh_admin off: %d", rec.Code)
	}
}

// /api/v1/nodes/{node}/<path> is that node's /api/v1/<path>; for this node
// itself it is served here, without a hop.
func TestNodeProxyPathIsRelativeToAPIv1(t *testing.T) {
	s := adminServer(t, "")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/nodes/minion/version-does-not-exist", nil))
	if rec.Code != 404 {
		t.Errorf("unknown path on this node: %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/nodes/nowhere/models", nil))
	if rec.Code != 404 || !strings.Contains(rec.Body.String(), "no live node") {
		t.Errorf("unknown node: %d %s", rec.Code, rec.Body)
	}
}

// Only a running download can be cancelled; its cancel function is called
// once and forgotten when the download ends.
func TestCancelDownload(t *testing.T) {
	s := adminServer(t, "")
	called := 0
	s.cancels.add("op-1", func() { called++ })
	h := s.Handler()
	post := func(id string) int {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/operations/"+id+"/cancel", nil))
		return rec.Code
	}
	if code := post("op-1"); code != 202 || called != 1 {
		t.Fatalf("cancel: %d, called %d", code, called)
	}
	s.cancels.remove("op-1")
	if code := post("op-1"); code != 404 {
		t.Errorf("after the download ended: %d", code)
	}
	if code := post("op-unknown"); code != 404 {
		t.Errorf("unknown operation: %d", code)
	}
}
