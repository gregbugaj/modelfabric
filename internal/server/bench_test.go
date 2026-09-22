package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gregbugaj/modelfabric/internal/bench"
	"github.com/gregbugaj/modelfabric/internal/tuner"
)

// A benchmark and a tuning sweep both reload the engine. Run together, each
// would measure a load the other had just replaced, as two overlapping sweeps
// once did on minion: every row "did not run".
func TestBenchAndTuneExcludeEachOther(t *testing.T) {
	tests := []struct {
		name    string
		first   func(*Server) error
		second  func(*Server) error
		wantErr string
	}{
		{name: "a benchmark refuses while a sweep runs",
			first:   func(s *Server) error { _, _, err := s.claimTune(tuner.Config{Model: "m"}); return err },
			second:  func(s *Server) error { _, err := s.claimBench(); return err },
			wantErr: "slot-tuning sweep is running"},
		{name: "a sweep refuses while a benchmark runs",
			first:   func(s *Server) error { _, err := s.claimBench(); return err },
			second:  func(s *Server) error { _, _, err := s.claimTune(tuner.Config{Model: "m"}); return err },
			wantErr: "benchmark is running"},
		{name: "a second benchmark refuses",
			first:   func(s *Server) error { _, err := s.claimBench(); return err },
			second:  func(s *Server) error { _, err := s.claimBench(); return err },
			wantErr: "already running"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newFrontFixture(t, false, false)
			if err := tc.first(f.srv); err != nil {
				t.Fatal(err)
			}
			err := tc.second(f.srv)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want %q", err, tc.wantErr)
			}
		})
	}
}

// The command in a report has to repeat the run exactly, so every setting is
// written out even where it is the default.
func TestBenchCommandIsComplete(t *testing.T) {
	c := bench.Config{Model: "qwen/qwen3.8-27b"}
	c.Fill()
	got := benchCommand("minion", c)
	want := "mfsh bench -model qwen/qwen3.8-27b -prompts prose -pp 1024,4096,8192,16384,32768 -tg 128 -batch 2,4,8 -batch-pp 1024 -reps 1 -node minion"
	if got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
}

// The cluster benchmark goes through this node's own front door, so it can
// only start where there is one and something in the mesh to measure.
func TestClusterBenchRefuses(t *testing.T) {
	tests := []struct {
		name   string
		front  string
		body   string
		want   int
		reason string
	}{
		{name: "no model", front: "127.0.0.1:1", body: `{}`, want: 400, reason: "name the model"},
		{name: "an unknown prompt set", front: "127.0.0.1:1", body: `{"model":"m","prompts":"poetry"}`, want: 400},
		{name: "a concurrency of 0", front: "127.0.0.1:1", body: `{"model":"m","concurrency":[0]}`, want: 400, reason: "out of range"},
		{name: "a model nobody holds", front: "127.0.0.1:1", body: `{"model":"other"}`, want: 409, reason: "no node in the mesh holds other"},
		{name: "a node with no front door", body: `{"model":"m"}`, want: 409, reason: "no front door"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newFrontFixture(t, false, false)
			f.srv.SetFrontDoor(tc.front, "")
			rec := httptest.NewRecorder()
			f.srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/bench/cluster", strings.NewReader(tc.body)))
			if rec.Code != tc.want || !strings.Contains(rec.Body.String(), tc.reason) {
				t.Fatalf("got %d %s, want %d naming %q", rec.Code, rec.Body, tc.want, tc.reason)
			}
		})
	}
}

// What the run measured is recorded before it starts: the door, who routes
// the model, and who holds it. A run that then fails (here the fixture's
// engine does not stream) keeps that record and says why, and does not stay
// "running".
func TestClusterBenchRecordsWhatItMeasured(t *testing.T) {
	f := newFrontFixture(t, false, false)
	front := httptest.NewServer(f.srv.FrontHandler())
	t.Cleanup(front.Close)
	f.srv.SetFrontDoor(strings.TrimPrefix(front.URL, "http://"), "")
	h := f.srv.Handler()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/bench/cluster", strings.NewReader(`{"model":"m","concurrency":[1,2]}`)))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("start: %d %s", rec.Code, rec.Body)
	}
	var st clusterStatus
	for deadline := time.Now().Add(10 * time.Second); ; {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/bench/cluster", nil))
		st = clusterStatus{}
		json.Unmarshal(rec.Body.Bytes(), &st)
		if !st.Running {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the run did not finish")
		}
		time.Sleep(50 * time.Millisecond)
	}
	r := st.Report
	if r == nil || r.Entry != "self" || r.Routing != "ModelFabric router" {
		t.Fatalf("report %+v", r)
	}
	if len(r.Holders) != 1 || r.Holders[0].Node != "self" || r.Holders[0].Engines != 1 {
		t.Fatalf("holders %+v", r.Holders)
	}
	if !strings.Contains(r.Command, "mfsh bench -cluster -model m") || !strings.Contains(r.Command, "-concurrency 1,2") {
		t.Fatalf("command %q", r.Command)
	}
	if st.Error == "" {
		t.Fatal("a run against an engine that does not stream reported no error")
	}
}

func TestFrontBaseDialsAWildcardOnLoopback(t *testing.T) {
	tests := []struct{ listen, want string }{
		{"0.0.0.0:1234", "http://127.0.0.1:1234"},
		{":1234", "http://127.0.0.1:1234"},
		{"[::]:1234", "http://127.0.0.1:1234"},
		{"127.0.0.1:3000", "http://127.0.0.1:3000"},
	}
	for _, tc := range tests {
		s := &Server{frontListen: tc.listen}
		if got, err := s.frontBase(); err != nil || got != tc.want {
			t.Errorf("%s: %q %v, want %q", tc.listen, got, err, tc.want)
		}
	}
}
