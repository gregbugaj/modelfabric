package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// Listener counts must include requests queued or served through Envoy;
// engine counters omit those requests.
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

func TestFrontDoorReleases(t *testing.T) {
	srv := &Server{}
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	srv.frontCounter(inner).ServeHTTP(httptest.NewRecorder(),
		httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader("{}")))
	if got := srv.accepted.Load(); got != 0 {
		t.Errorf("after answering, nothing is in flight; got %d", got)
	}
}
