package server

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// Peers may probe and forward inference; they may not manage this node.
func TestPeerAllowlist(t *testing.T) {
	allowed := [][2]string{
		{"GET", "/z/state"}, {"GET", "/z/mesh"}, {"GET", "/healthz"},
		{"GET", "/v1/models"}, {"GET", "/v1/models/qwen/qwen3-0.6b"}, {"GET", "/z/endpoints.yaml"},
		{"POST", "/v1/chat/completions"}, {"POST", "/v1/responses"}, {"POST", "/v1/messages"},
		{"POST", "/v1/embeddings"}, {"POST", "/v1/audio/transcriptions"},
	}
	denied := [][2]string{
		{"POST", "/api/v1/models/load"}, {"POST", "/api/v1/models/unload"},
		{"POST", "/api/v1/models/repin"}, {"POST", "/api/v1/models/recover"},
		{"POST", "/api/v1/runtimes/select"}, {"POST", "/api/v1/models/rescan"},
		{"POST", "/z/preferred"}, {"GET", "/api/v1/models"}, {"GET", "/api/v0/models"},
		{"GET", "/"}, {"GET", "/index.html"}, {"GET", "/z/log/stream"},
		{"DELETE", "/v1/models/x"}, {"GET", "/v1/chat/completions"},
		// The live token tap carries what the model is writing. It is served
		// on a node's own loopback listener only, and this is what enforces
		// that: allowing it here would send reply text across the tailnet to
		// whichever peer asked.
		{"GET", "/z/log/tokens"},
		// Tuning reloads this node's engine once per slot count, so it is
		// management, not a probe: any tailnet member could otherwise take a
		// GPU out of service for half an hour by asking politely. It stays
		// reachable for the owner's own devices, which is how the dashboard
		// tunes a fleet, but that goes through the same-owner check rather
		// than this allowlist.
		{"POST", "/api/v1/tune"}, {"GET", "/api/v1/tune"}, {"DELETE", "/api/v1/tune"},
	}
	for _, c := range allowed {
		if !peerAllowed(c[0], c[1]) {
			t.Errorf("%s %s should be reachable by peers", c[0], c[1])
		}
	}
	for _, c := range denied {
		if peerAllowed(c[0], c[1]) {
			t.Errorf("%s %s must not be reachable by peers", c[0], c[1])
		}
	}
}

// mesh_admin is a setting an operator uses to close a door. Matching only the
// exact string "off" meant a typo left it open: "of", "OFF" or a value from a
// newer version all fell through to same-owner management.
func TestMeshAdminFailsClosedOnAnUnknownValue(t *testing.T) {
	for _, policy := range []string{"of", "OFF", "none", "disabled", "yes"} {
		s := &Server{meshAdmin: policy}
		ok, why := s.sameOwner(httptest.NewRequest("GET", "/api/v1/models", nil))
		if ok {
			t.Errorf("mesh_admin %q allowed management", policy)
		}
		if !strings.Contains(why, policy) {
			t.Errorf("the refusal for %q should name the value: %q", policy, why)
		}
	}
	// The values ModelFabric defines still behave as documented.
	if ok, _ := (&Server{meshAdmin: "off"}).sameOwner(httptest.NewRequest("GET", "/x", nil)); ok {
		t.Error(`"off" allowed management`)
	}
	// "same-owner" with no resolver still refuses, but for the stated reason.
	if ok, why := (&Server{meshAdmin: "same-owner"}).sameOwner(httptest.NewRequest("GET", "/x", nil)); ok || !strings.Contains(why, "mesh_admin") {
		t.Errorf(`"same-owner" without a resolver: ok=%v why=%q`, ok, why)
	}
}

// /metrics is not in the open allowlist: a scraper reaching it over the
// tailnet goes through the same-owner check, like management. It names every
// model and how much traffic each took, which is not for every tailnet member.
func TestMetricsIsNotOpenToEveryPeer(t *testing.T) {
	if peerAllowed("GET", "/metrics") {
		t.Error("/metrics must not be in the unauthenticated peer allowlist")
	}
	// mesh_admin off closes it along with the rest of management.
	s := &Server{meshAdmin: "off"}
	if ok, _ := s.sameOwner(httptest.NewRequest("GET", "/metrics", nil)); ok {
		t.Error("mesh_admin off must refuse /metrics over the mesh")
	}
	// On loopback it is served, like the dashboard: the mesh policy applies to
	// the tailnet listener only. A node with no mesh still answers, with the
	// counters and without the gauges.
	open := &Server{metrics: newMetrics()}
	rec := httptest.NewRecorder()
	open.handleMetrics(rec, httptest.NewRequest("GET", "/metrics", nil))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "modelfabric_requests_total") {
		t.Errorf("loopback scrape got %d: %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("Content-Type %q: Prometheus expects the text exposition type", ct)
	}
}
