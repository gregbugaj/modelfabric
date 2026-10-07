package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// Every inference listener must tap responses. Cover both handlers because
// public_listen previously bypassed token capture.
func TestEveryInferenceListenerTaps(t *testing.T) {
	for _, tc := range []struct {
		name    string
		handler func(*frontFixture) http.Handler
	}{
		{"FrontHandler", func(f *frontFixture) http.Handler { return f.srv.FrontHandler() }},
		{"PublicHandler", func(f *frontFixture) http.Handler { return f.srv.PublicHandler() }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFrontFixture(t, false, false)
			ch, cancel := f.srv.tokens.Subscribe(16)
			defer cancel()

			if rec := call(tc.handler(f), testKey, ""); rec.Code != http.StatusOK {
				t.Fatalf("status %d, want 200", rec.Code)
			}
			select {
			case e := <-ch:
				if e.Trace == "" {
					t.Error("tapped event carried no trace id")
				}
			default:
				t.Fatalf("%s served inference without tapping it", tc.name)
			}
		})
	}
}

// The tap must not wrap anything that is not inference: the dashboard, the
// mesh views and the health endpoint all stream through the same handler.
func TestTapSkipsNonInferencePaths(t *testing.T) {
	f := newFrontFixture(t, false, false)
	ch, cancel := f.srv.tokens.Subscribe(4)
	defer cancel()

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	w, done := f.srv.tapTokens(httptest.NewRecorder(), req)
	done()
	if w != http.ResponseWriter(nil) && len(ch) != 0 {
		t.Fatalf("/healthz produced %d token events", len(ch))
	}
	select {
	case e := <-ch:
		t.Fatalf("/healthz produced a token event: %+v", e)
	default:
	}
}
