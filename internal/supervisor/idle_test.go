package supervisor

import (
	"testing"
	"time"

	"github.com/gregbugaj/modelfabric/internal/config"
	"github.com/gregbugaj/modelfabric/internal/engineshim"
	"github.com/gregbugaj/modelfabric/internal/mesh"
	"net/http"
	"net/http/httptest"
)

// An instance expires only with a TTL, nothing in flight, and no use for that
// long — and use is measured from the last request, not from the load.
func TestInstanceExpiry(t *testing.T) {
	m := mesh.New(config.Default(), "self")
	eng := mesh.NewEngine("i1", "http://127.0.0.1:1")
	eng.MarkReady("m")
	m.RegisterEngine(eng)
	inst := &Instance{ID: "i1", Model: "m", TTLSeconds: 60, StartedAt: time.Now().Add(-time.Hour), engine: eng}

	// Never used: measured from the load, long ago.
	if !inst.expired(time.Now()) {
		t.Fatal("an unused instance past its TTL did not expire")
	}

	release := m.Candidates("m", true)[0].Acquire()
	if inst.expired(time.Now().Add(time.Hour)) {
		t.Fatal("an instance with a request in flight expired")
	}
	release()

	now := time.Now()
	if inst.expired(now.Add(30 * time.Second)) {
		t.Fatal("expired 30s after use with a 60s TTL")
	}
	if !inst.expired(now.Add(61 * time.Second)) {
		t.Fatal("did not expire 61s after use with a 60s TTL")
	}

	manual := &Instance{ID: "i2", StartedAt: time.Now().Add(-24 * time.Hour)}
	if manual.expired(time.Now()) {
		t.Fatal("an instance without a TTL expired")
	}
}

// Tearing a shim down must not need the instance lock. It did once: freeing
// the port took s.mu, and both call sites held it, so every unload deadlocked
// the node — engines stayed resident, `mfsh ps` hung, and the process would
// not shut down. The port is freed by closing the listener now, so this holds
// the lock while stopping a shim and expects it to finish regardless.
func TestStopShimDoesNotNeedTheInstanceLock(t *testing.T) {
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	defer engine.Close()
	sh, err := engineshim.New(engine.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	port, err := sh.Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &Supervisor{reserved: map[int]bool{}}
	inst := &Instance{ID: "inst-1", ShimPort: port, shim: sh}

	done := make(chan struct{})
	s.mu.Lock()
	go func() {
		s.stopShim(inst)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		s.mu.Unlock()
		t.Fatal("stopShim blocked while the instance lock was held; unloads would deadlock")
	}
	s.mu.Unlock()
	if inst.shim != nil {
		t.Error("the shim was not cleared")
	}
}

// An engine that refuses to stop must not be forgotten. unload removes the
// instance before it knows the stop worked — deliberately, so nothing routes
// to it — so a Stop failure ("process survived SIGKILL") used to leave a live
// process holding its VRAM that `mfsh ps` did not list and no unload could
// reach.
func TestAStuckEngineStaysListedButUnroutable(t *testing.T) {
	s := &Supervisor{
		instances: map[string]*Instance{},
		byModel:   map[string]string{},
	}
	inst := &Instance{ID: "i1", Model: "m"}
	s.retainStuck("i1", inst)
	if s.instances["i1"] != inst {
		t.Fatal("a stuck instance must stay listed so it can be seen and retried")
	}
	if _, routable := s.byModel["m"]; routable {
		t.Fatal("a stuck instance must not be routable again")
	}
	// A replacement that already took the id wins; the corpse never displaces it.
	fresh := &Instance{ID: "i1", Model: "m"}
	s.instances["i1"] = fresh
	s.retainStuck("i1", inst)
	if s.instances["i1"] != fresh {
		t.Fatal("retaining a stuck instance overwrote a newer one with the same id")
	}
}
