package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gregbugaj/modelfabric/internal/llmd"
	"github.com/gregbugaj/modelfabric/internal/mesh"
	"github.com/gregbugaj/modelfabric/internal/router"
)

// Checking only matchEngine missed the response/event plumbing: Activity needs
// the same engine identity as the caller, including when Envoy dials its shim.
func TestScheduledAttributionReachesResponseAndActivity(t *testing.T) {
	for _, listener := range []struct {
		name    string
		handler func(*Server) http.Handler
	}{
		{"front", (*Server).FrontHandler},
		{"public", (*Server).PublicHandler},
	} {
		t.Run(listener.name, func(t *testing.T) {
			for _, tc := range []struct {
				name, upstream, wantNode, wantEngine, wantUpstream string
			}{
				{"engine port identifies the instance", "127.0.0.1:18000", "self", "instance-a", "http://127.0.0.1:18000"},
				{"shim port identifies the same instance", "127.0.0.1:18001", "self", "instance-a", "http://127.0.0.1:18001"},
				{"an upstream URL preserves its path", "http://127.0.0.1:18001/v1", "self", "instance-a", "http://127.0.0.1:18001/v1"},
				{"unknown port identifies only the node", "127.0.0.1:18999", "self", "", "http://127.0.0.1:18999"},
				{"unknown address identifies nobody", "192.0.2.1:18000", "", "", "http://192.0.2.1:18000"},
				{"absent report identifies nobody", "", "", "", ""},
				{"own front proxy identifies nobody", "127.0.0.1:1234", "", "", "http://127.0.0.1:1234"},
				{"own Envoy proxy identifies nobody", "127.0.0.1:8080", "", "", "http://127.0.0.1:8080"},
			} {
				t.Run(tc.name, func(t *testing.T) {
					f := newFrontFixture(t, true, false)
					f.schedUpstream = tc.upstream
					f.srv.frontListen = "127.0.0.1:1234"
					f.srv.llmd = llmd.New(llmd.Config{Listen: "127.0.0.1:8080"}, f.srv.log)
					f.srv.m.SetInstanceProvider(func() []mesh.InstanceState {
						return []mesh.InstanceState{{ID: "instance-a", Address: "127.0.0.1", Port: 18000, MetricsPort: 18001}}
					})
					rec := call(listener.handler(f.srv), testKey, "")
					if rec.Code != http.StatusOK {
						t.Fatalf("inference: status %d, body %s", rec.Code, rec.Body)
					}
					for header, want := range map[string]string{
						router.NodeHeader:   tc.wantNode,
						router.EngineHeader: tc.wantEngine,
						"X-Fabric-Via":      "llm-d",
						upstreamHeader:      "",
					} {
						if got := rec.Header().Get(header); got != want {
							t.Errorf("%s = %q, want %q", header, got, want)
						}
					}
					if _, present := rec.Header()[http.CanonicalHeaderKey(upstreamHeader)]; present {
						t.Error("upstream address header was not removed")
					}

					// Activity reads this endpoint even for traffic on the public
					// listener; public inference must publish into the same ring.
					activity := httptest.NewRecorder()
					f.srv.FrontHandler().ServeHTTP(activity, httptest.NewRequest(http.MethodGet, "/api/v1/traffic/recent", nil))
					if activity.Code != http.StatusOK {
						t.Fatalf("activity: status %d, body %s", activity.Code, activity.Body)
					}
					var recent trafficRecent
					if err := json.Unmarshal(activity.Body.Bytes(), &recent); err != nil {
						t.Fatal(err)
					}
					if len(recent.Events) != 1 {
						t.Fatalf("activity has %d events, want exactly one", len(recent.Events))
					}
					e := recent.Events[0]
					if e.Node != tc.wantNode || e.Engine != tc.wantEngine || e.Upstream != tc.wantUpstream || e.Via != "llm-d" {
						t.Errorf("activity attribution = node %q, engine %q, upstream %q, via %q; want %q, %q, %q, llm-d",
							e.Node, e.Engine, e.Upstream, e.Via, tc.wantNode, tc.wantEngine, tc.wantUpstream)
					}
					if e.Status != http.StatusOK || e.Model != "m" || e.Path != "/v1/chat/completions" {
						t.Errorf("activity recorded the wrong request: %+v", e)
					}
				})
			}
		})
	}
}
