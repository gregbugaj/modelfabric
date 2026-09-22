package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// Every request enters through the front door, whoever routes it after — so
// this is the one in-flight figure ModelFabric always knows. Engine counts cannot
// see a request still queued in Envoy, and under llm-d they
// cannot see one being served either, because Envoy dials the engine directly
// and ModelFabric is never told. The dashboard showed 0 in flight through a whole
// 8000-word request because of exactly that.
func TestFrontDoorCountsInflight(t *testing.T) {
	srv := &Server{}
	var during int64
	release := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		during = srv.accepted.Load()
		wg.Done()
		<-release
	})
	h := srv.frontCounter(inner)

	go h.ServeHTTP(httptest.NewRecorder(),
		httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader("{}")))
	wg.Wait()
	if during != 1 {
		t.Errorf("a request being served should count as one in flight, got %d", during)
	}
	close(release)
}

// A request that runs no model is not inference and must not inflate the
// figure: the dashboard reads it as load.
func TestFrontDoorIgnoresNonInference(t *testing.T) {
	srv := &Server{}
	var during int64
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		during = srv.accepted.Load()
	})
	srv.frontCounter(inner).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/z/mesh", nil))
	if during != 0 {
		t.Errorf("a mesh view is not inference, got %d", during)
	}
}

// And it comes back down, or the node reads as permanently busy.
func TestFrontDoorReleases(t *testing.T) {
	srv := &Server{}
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	srv.frontCounter(inner).ServeHTTP(httptest.NewRecorder(),
		httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader("{}")))
	if got := srv.accepted.Load(); got != 0 {
		t.Errorf("after answering, nothing is in flight; got %d", got)
	}
}
