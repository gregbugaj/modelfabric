package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gregbugaj/modelfabric/internal/discovery"
)

const imageBody = `{"model":"m","messages":[{"role":"user","content":[` +
	`{"type":"text","text":"what is in this image?"},` +
	`{"type":"image_url","image_url":{"url":"data:image/png;base64,iVBORw0KGgoAAAANSUhEUg"}}]}]}`

const textBody = `{"model":"m","messages":[{"role":"user","content":"what day is it?"}]}`

func post(t *testing.T, h http.Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// Set llm-d's profile from image detection so its vision label filter excludes
// engines loaded without projectors, avoiding mtmd chunk failures.
func TestTheSchedulerIsToldWhenARequestCarriesAnImage(t *testing.T) {
	f := newFrontFixture(t, true, false)
	h := f.srv.FrontHandler()

	if rec := post(t, h, imageBody); rec.Code != 200 {
		t.Fatalf("image request: %d %s", rec.Code, rec.Body)
	}
	if rec := post(t, h, textBody); rec.Code != 200 {
		t.Fatalf("text request: %d %s", rec.Code, rec.Body)
	}
	want := []string{discovery.VisionProfile, "default"}
	if len(f.schedProfile) != 2 || f.schedProfile[0] != want[0] || f.schedProfile[1] != want[1] {
		t.Errorf("the scheduler was told %q, want %q", f.schedProfile, want)
	}
}

// Set a known profile on every request and override caller-supplied values.
// EPP rejects unknown profiles with ResourceExhausted.
func TestACallerCannotChooseItsOwnSchedulingProfile(t *testing.T) {
	f := newFrontFixture(t, true, false)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(textBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(discovery.ProfileHeader, "something-of-its-own")
	rec := httptest.NewRecorder()
	f.srv.FrontHandler().ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if len(f.schedProfile) != 1 || f.schedProfile[0] != "default" {
		t.Errorf("the caller's profile reached the scheduler: %q", f.schedProfile)
	}
}

// Large bodies must retain model-based scheduler selection. The former
// 64 KiB cap bypassed llm-d for long conversations.
func TestALongConversationStillReachesTheScheduler(t *testing.T) {
	f := newFrontFixture(t, true, false)
	h := f.srv.FrontHandler()

	long := strings.Repeat("the quick brown fox jumps over the lazy dog. ", (360<<10)/45)
	body := `{"model":"m","messages":[{"role":"user","content":"` + long + `"}]}`
	if len(body) < 300<<10 {
		t.Fatalf("this test needs a body past the old cap, got %d bytes", len(body))
	}
	rec := post(t, h, body)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "scheduler") {
		t.Fatalf("a %d-byte conversation did not reach the scheduler: %d %s",
			len(body), rec.Code, rec.Body)
	}
	if f.engineHits != 0 {
		t.Errorf("it went to ModelFabric's own router instead (%d engine hits)", f.engineHits)
	}
	if len(f.schedProfile) != 1 || f.schedProfile[0] != "default" {
		t.Errorf("profile sent: %q, want default for a text conversation", f.schedProfile)
	}
}
