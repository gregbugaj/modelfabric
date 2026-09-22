package server

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/gregbugaj/modelfabric/internal/config"
	"github.com/gregbugaj/modelfabric/internal/discovery"
	"github.com/gregbugaj/modelfabric/internal/mesh"
	"github.com/gregbugaj/modelfabric/internal/router"
)

const testKey = "sk-node-key"

// frontFixture: an engine serving model "m" behind ModelFabric's router, and a
// stand-in scheduler (llm-d's Envoy, in production) that records what it was
// sent.
type frontFixture struct {
	srv *Server
	// schedAuth is the Authorization the scheduler received per request. It must
	// always be empty: Envoy is on loopback and needs no credential, and
	// forwarding the caller's key to anything downstream is never right.
	schedAuth []string
	// schedProfile is the scheduling profile the scheduler was told to use per
	// request. llm-d picks its profile from this, so a request carrying an image
	// reaches only an engine that can read one.
	schedProfile []string
	engineHits   int
	// schedUpstream is the address the scheduler says it dialled, as llm-d's
	// Envoy reports it. Empty sends no header, as an older Envoy config does.
	schedUpstream string
}

func newFrontFixture(t *testing.T, scheduled, requireKey bool) *frontFixture {
	t.Helper()
	f := &frontFixture{}
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.engineHits++
		// What llama-server sends: the caller's origin echoed back, with
		// credentials allowed. ModelFabric must never pass it on.
		if o := r.Header.Get("Origin"); o != "" {
			w.Header().Set("Access-Control-Allow-Origin", o)
			w.Header().Set("Access-Control-Allow-Credentials", "true")
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"from":"engine"}`)
	}))
	t.Cleanup(engine.Close)
	sched := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.schedAuth = append(f.schedAuth, r.Header.Get("Authorization"))
		f.schedProfile = append(f.schedProfile, r.Header.Get(discovery.ProfileHeader))
		if f.schedUpstream != "" {
			w.Header().Set("X-Fabric-Upstream", f.schedUpstream)
		}
		io.WriteString(w, `{"from":"scheduler"}`)
	}))
	t.Cleanup(sched.Close)

	m := mesh.New(config.Default(), "self")
	e := mesh.NewEngine("e", engine.URL)
	e.MarkReady("m")
	m.RegisterEngine(e)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	f.srv = New(m, router.New(m, log), nil, log, nil)
	f.srv.SetAuth(func() (string, error) { return testKey, nil }, requireKey)
	u, _ := url.Parse(sched.URL)
	// Gated on the model, as the real one is: llm-d serves one model, and
	// llmdTarget declines anything else — including the empty string a body ModelFabric
	// could not read yields. A stand-in that ignored the model would pass a
	// request the real scheduler refuses, which is how a body too large to peek
	// went unnoticed.
	f.srv.scheduler = func(model string) (*url.URL, bool) { return u, scheduled && model == "m" }
	return f
}

func call(h http.Handler, key, header string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"m","messages":[]}`))
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	if header != "" {
		req.Header.Set(header, "1")
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// When a scheduler owns the model, the front door hands inference to it rather
// than routing itself; local apps need no key (require_api_key off).
func TestFrontDoorHandsOffToTheScheduler(t *testing.T) {
	f := newFrontFixture(t, true, false)
	rec := call(f.srv.FrontHandler(), "", "")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "scheduler") {
		t.Fatalf("got %d %s", rec.Code, rec.Body)
	}
	if f.schedAuth[0] != "" {
		t.Errorf("the scheduler got Authorization %q; the caller's credential must stop "+
			"at ModelFabric, and Envoy on loopback needs none", f.schedAuth[0])
	}
	// Whoever chose the engine is named, so a graph can separate the paths.
	if rec.Header().Get("X-Fabric-Via") != "llm-d" {
		t.Errorf("X-Fabric-Via = %q, want llm-d", rec.Header().Get("X-Fabric-Via"))
	}
}

// A request already forwarded once reaches the router and is never handed on
// again: that marker is the mesh's loop guard.
func TestForwardedRequestsReachTheRouter(t *testing.T) {
	f := newFrontFixture(t, true, false)
	rec := call(f.srv.FrontHandler(), testKey, router.HopHeader)
	if rec.Code != 200 || f.engineHits != 1 || len(f.schedAuth) != 0 {
		t.Errorf("got %d %s (engine %d, scheduler %d)", rec.Code, rec.Body, f.engineHits, len(f.schedAuth))
	}
}

// Without that proof the marker is somebody guessing a header name. Honouring
// it let any caller on this listener skip the scheduler and pick the path its
// request took.
func TestForgedHopMarkerCannotSkipTheScheduler(t *testing.T) {
	f := newFrontFixture(t, true, false)
	rec := call(f.srv.FrontHandler(), "", router.HopHeader)
	if rec.Code != 200 {
		t.Fatalf("got %d %s", rec.Code, rec.Body)
	}
	if f.engineHits != 0 || len(f.schedAuth) != 1 {
		t.Errorf("an unauthenticated marker bypassed the scheduler (engine %d, scheduler %d)",
			f.engineHits, len(f.schedAuth))
	}
}

func TestFrontDoorWithoutASchedulerUsesTheRouter(t *testing.T) {
	f := newFrontFixture(t, false, false)
	if rec := call(f.srv.FrontHandler(), "", ""); rec.Code != 200 || f.engineHits != 1 {
		t.Fatalf("got %d %s", rec.Code, rec.Body)
	}
}

// require_api_key: OpenAI-style Bearer auth, checked by ModelFabric itself —
// including a request that claims to have been forwarded already.
func TestRequireAPIKeyOnTheFrontDoor(t *testing.T) {
	f := newFrontFixture(t, true, true)
	h := f.srv.FrontHandler()
	for _, c := range []struct {
		key, header string
		want        int
	}{
		{"", "", 401},
		{"wrong", "", 401},
		{testKey, "", 200},
		{testKey, router.HopHeader, 200},
	} {
		if rec := call(h, c.key, c.header); rec.Code != c.want {
			t.Errorf("key %q header %q: %d, want %d", c.key, c.header, rec.Code, c.want)
		}
	}
	// Management and the mesh views are not inference; the key does not
	// guard them (loopback is their protection).
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/z/mesh", nil))
	if rec.Code != 200 {
		t.Errorf("/z/mesh: %d", rec.Code)
	}
}

// The public listener: key always required, inference only, internal headers
// stripped, and it routes for itself when nothing else is scheduling.
func TestPublicListener(t *testing.T) {
	f := newFrontFixture(t, true, false)
	h := f.srv.PublicHandler()
	if rec := call(h, "", ""); rec.Code != 401 || rec.Header().Get("WWW-Authenticate") == "" {
		t.Errorf("no key: %d", rec.Code)
	}
	// The hop marker is ModelFabric's own loop guard. A public caller that could set
	// it would be telling this node the request had already been forwarded.
	if rec := call(h, testKey, router.HopHeader); rec.Code != 200 {
		t.Errorf("a public caller setting %s: %d", router.HopHeader, rec.Code)
	}
	for _, path := range []string{"/", "/z/mesh", "/api/v1/models"} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer "+testKey)
		h.ServeHTTP(rec, req)
		if rec.Code != 404 {
			t.Errorf("%s on the public listener: %d, want 404", path, rec.Code)
		}
	}
	// Anthropic clients send x-api-key.
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"m"}`))
	req.Header.Set("X-Api-Key", testKey)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Errorf("x-api-key: %d", rec.Code)
	}

	off := newFrontFixture(t, false, false)
	if rec := call(off.srv.PublicHandler(), testKey, ""); rec.Code != 200 || off.engineHits != 1 {
		t.Errorf("nothing scheduling, so ModelFabric's own router should serve it: %d %s", rec.Code, rec.Body)
	}
}

// newRecordingUpstream is a fake scheduler upstream that records the body it
// was sent and answers with an event stream. Returns its host:port.
func newRecordingUpstream(t *testing.T, got *string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		*got = string(b)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		io.WriteString(w, "data: {\"id\":\"x\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"hi \"}}]}\n\n")
		io.WriteString(w, "data: {\"id\":\"x\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"there\"},\"finish_reason\":\"stop\"}]}\n\n")
		io.WriteString(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	return u.Host
}

// Attribution must survive a scheduler dialling the shim. Both ports name the
// same engine: the shim is how the engine's own node gets into the request
// path, and matching only the engine's port left every scheduled row saying
// "engine unknown".
func TestMatchEngineAcceptsBothEnginePorts(t *testing.T) {
	self := mesh.NodeState{
		Node: "minion", Addr: "100.64.0.2",
		Instances: []mesh.InstanceState{{
			ID: "inst-abc", Address: "100.64.0.2", Port: 18000, MetricsPort: 18001,
		}},
	}
	for _, tc := range []struct{ name, base string }{
		{"engine port", "http://100.64.0.2:18000/v1"},
		{"shim port", "http://100.64.0.2:18001/v1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			node, engine, ok := matchEngine(tc.base, self, nil)
			if !ok || node != "minion" || engine != "inst-abc" {
				t.Fatalf("matchEngine(%s) = %q/%q/%v, want minion/inst-abc/true",
					tc.base, node, engine, ok)
			}
		})
	}
	// A port belonging to neither still identifies the machine — that is the
	// documented fallback, "minion, engine unknown" beating "not recorded" —
	// but it must not be attributed to this engine.
	node, engine, ok := matchEngine("http://100.64.0.2:9999/v1", self, nil)
	if !ok || node != "minion" {
		t.Errorf("unknown port lost the machine: %q/%q/%v", node, engine, ok)
	}
	if engine != "" {
		t.Errorf("unknown port was attributed to engine %q", engine)
	}
}

// Under llm-d the node that served a request is known only from the address
// Envoy reports. The front door used to look for a header that nothing
// sends any more, so every scheduled request was recorded with no
// node. The address is turned into the node's name, and is not passed on: a
// caller is told which machine, not where its engine listens.
func TestScheduledRequestNamesTheNodeThatServedIt(t *testing.T) {
	tests := []struct {
		name, upstream, wantNode string
	}{
		{name: "the address Envoy dialled names the node", upstream: "127.0.0.1:18000", wantNode: "self"},
		{name: "an address nobody in the mesh has names no one", upstream: "10.9.9.9:18000"},
		{name: "no report, from an older Envoy config, names no one"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newFrontFixture(t, true, false)
			f.schedUpstream = tc.upstream
			rec := call(f.srv.FrontHandler(), "", "")
			if rec.Code != 200 || rec.Header().Get("X-Fabric-Via") != "llm-d" {
				t.Fatalf("got %d via %q", rec.Code, rec.Header().Get("X-Fabric-Via"))
			}
			if got := rec.Header().Get("X-Fabric-Node"); got != tc.wantNode {
				t.Fatalf("node %q, want %q", got, tc.wantNode)
			}
			if rec.Header().Get("X-Fabric-Upstream") != "" {
				t.Fatal("the engine's address was passed on to the caller")
			}
		})
	}
}
