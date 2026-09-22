package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// Every listener that serves inference must tap, and each one is a separate
// handler. The tap first went only into FrontHandler, which left
// public_listen — the listener every caller from outside the machine uses —
// showing nothing at all. A third listener added later would be blind the
// same way, so both are asserted here by name.
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
			// The fixture's engine answers with plain JSON rather than an event
			// stream, so there are no deltas — but a watcher must still be told
			// the request happened and ended, or its view of a busy node is a
			// blank panel.
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
