package router

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gregbugaj/modelfabric/internal/devlog"
)

// A request is three entries in the developer log, in order, sharing its
// trace: received, sent with the reason, finished with what it cost.
func TestARequestIsLoggedAsItHappens(t *testing.T) {
	const answer = `{"choices":[{"message":{"content":"hi"}}],"usage":{"prompt_tokens":2100,"completion_tokens":48,` +
		`"prompt_tokens_details":{"cached_tokens":2000}},"timings":{"prompt_n":100,"cache_n":2000,"prompt_ms":120.5,` +
		`"predicted_per_second":43.2,"draft_n":40,"draft_n_accepted":30}}`
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, answer)
	}))
	defer up.Close()
	// Long enough to be matched on: a prompt of a few words is placed by load alone.
	secret := "the user's private prompt " + strings.Repeat("with a long history ", 400)
	body := `{"model":"m","messages":[{"role":"system","content":"s"},{"role":"user","content":"` + secret + `"}]}`

	for _, c := range []struct {
		name    string
		capture bool
	}{
		{"capture off: sizes and counts, nothing a request says", false},
		{"capture on: the request and the answer are in the log", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := routerWith(t, up)
			r.EnablePrefixAffinity()
			r.Dev = devlog.New("entry", 0)
			r.Bodies = func() BodyLog { return BodyLog{Enabled: c.capture, Max: 4096} }
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
			r.Forward(rec, req, "/v1/chat/completions")
			if rec.Code != http.StatusOK {
				t.Fatalf("status %d: %s", rec.Code, rec.Body)
			}
			got := r.Dev.Recent(0, devlog.Debug)
			if len(got) != 3 {
				t.Fatalf("got %d entries, want received, sent, finished: %+v", len(got), got)
			}
			trace := rec.Header().Get(TraceHeader)
			for i, want := range []string{
				"request received: POST /v1/chat/completions, 2 messages, ",
				"sent to ",
				"finished: 200 in ",
			} {
				if !strings.HasPrefix(got[i].Msg, want) {
					t.Errorf("entry %d is %q, want it to start %q", i, got[i].Msg, want)
				}
				if got[i].Trace != trace || trace == "" {
					t.Errorf("entry %d has trace %q, want the request's %q", i, got[i].Trace, trace)
				}
				if got[i].Level != devlog.Info || got[i].Model != "m" {
					t.Errorf("entry %d: level %s model %q, want info and m", i, got[i].Level, got[i].Model)
				}
			}
			if !strings.Contains(got[1].Msg, "no engine holds this prompt") {
				t.Errorf("a first request is placed cold, and the log must say so: %q", got[1].Msg)
			}
			for _, part := range []string{"2100 prompt tokens, 2000 from cache (95%)", "48 generated at 43.2 tok/s", "30 of 40 drafted tokens accepted"} {
				if !strings.Contains(got[2].Msg, part) {
					t.Errorf("finished line %q is missing %q", got[2].Msg, part)
				}
			}
			for i, e := range got {
				has := strings.Contains(e.Body, "the user's private prompt") || strings.Contains(e.Body, `"content":"hi"`)
				wantBody := c.capture && i != 1
				if has != wantBody {
					t.Errorf("entry %d (%s): body present %v, want %v", i, e.Msg, has, wantBody)
				}
				if strings.Contains(e.Msg, "private prompt") {
					t.Errorf("entry %d put the prompt in its message", i)
				}
			}
		})
	}
}

// The second request of a conversation goes back to the engine that has it,
// and the log gives that as the reason.
func TestTheLogSaysWhyAnEngineWasChosen(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, `{"usage":{"prompt_tokens":900,"completion_tokens":5}}`)
	}))
	defer up.Close()
	r := routerWith(t, up)
	r.EnablePrefixAffinity()
	r.Dev = devlog.New("entry", 0)
	turn := `{"model":"m","messages":[{"role":"user","content":"` + strings.Repeat("a long conversation ", 400) + `"}]}`
	for i := 0; i < 2; i++ {
		r.Forward(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(turn)), "/v1/chat/completions")
	}
	var sent []devlog.Entry
	for _, e := range r.Dev.Recent(0, devlog.Info) {
		if strings.HasPrefix(e.Msg, "sent to ") {
			sent = append(sent, e)
		}
	}
	if len(sent) != 2 {
		t.Fatalf("got %d placements, want 2", len(sent))
	}
	if !strings.Contains(sent[1].Msg, "it holds 100% of this prompt") {
		t.Errorf("the repeat should be sent back for what the engine holds, got %q", sent[1].Msg)
	}
}

func TestAFailureIsLoggedWithItsReason(t *testing.T) {
	r := routerWith(t)
	r.Dev = devlog.New("entry", 0)
	rec := httptest.NewRecorder()
	r.Forward(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"nobody-serves-this"}`)), "/v1/chat/completions")
	got := r.Dev.Recent(0, devlog.Debug)
	last := got[len(got)-1]
	if last.Level != devlog.Warn || !strings.Contains(last.Msg, "failed with 404") || !strings.Contains(last.Msg, "no node serves this model") {
		t.Errorf("a request for an unknown model: %s %q, want a warning naming the 404 and why", last.Level, last.Msg)
	}
}
