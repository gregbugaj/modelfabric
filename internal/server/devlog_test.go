package server

import (
	"bufio"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gregbugaj/modelfabric/internal/devlog"
)

func TestDevlogIsServedAtTheLevelAsked(t *testing.T) {
	s := tuneServer(t)
	s.dev.Add(devlog.Entry{Level: devlog.Info, Msg: "request received"})
	s.dev.Add(devlog.Entry{Level: devlog.Debug, Msg: "an engine's own line"})
	s.dev.Add(devlog.Entry{Level: devlog.Warn, Msg: "RAM cache full"})
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	for _, c := range []struct {
		name, query string
		status      int
		want        string
	}{
		{"by default: requests, not engine lines", "", 200, "request received|RAM cache full"},
		{"debug: everything", "?level=debug", 200, "request received|an engine's own line|RAM cache full"},
		{"warn: only what needs attention", "?level=warn", 200, "RAM cache full"},
		{"the newest one", "?level=debug&limit=1", 200, "RAM cache full"},
		{"only what came after the reader's last entry", "?level=debug&after=2", 200, "RAM cache full"},
		{"a reader that is up to date gets nothing", "?level=debug&after=3", 200, ""},
		{"after must be a number", "?after=soon", 400, ""},
		{"a level that does not exist is refused, not read as something else", "?level=verbose", 400, ""},
		{"a limit that is not a number", "?limit=lots", 400, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			resp, err := http.Get(ts.URL + "/api/v1/devlog" + c.query)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != c.status {
				t.Fatalf("status %d, want %d", resp.StatusCode, c.status)
			}
			if c.status != 200 {
				return
			}
			var got devlogRecent
			if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
				t.Fatal(err)
			}
			var msgs []string
			for _, e := range got.Entries {
				msgs = append(msgs, e.Msg)
			}
			if strings.Join(msgs, "|") != c.want {
				t.Errorf("got %q, want %q", strings.Join(msgs, "|"), c.want)
			}
			if got.Node != "minion" || got.Capture {
				t.Errorf("node %q capture %v, want minion with capture off", got.Node, got.Capture)
			}
		})
	}
}

func TestDevlogStreamReplaysThenFollows(t *testing.T) {
	s := tuneServer(t)
	s.dev.Add(devlog.Entry{Msg: "before the reader connected"})
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/api/v1/devlog/stream?backlog=1")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content type %q", ct)
	}
	lines := make(chan string, 8)
	go func() {
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			if data, ok := strings.CutPrefix(sc.Text(), "data: "); ok {
				lines <- data
			}
		}
	}()
	next := func() devlog.Entry {
		t.Helper()
		select {
		case l := <-lines:
			var e devlog.Entry
			if err := json.Unmarshal([]byte(l), &e); err != nil {
				t.Fatal(err)
			}
			return e
		case <-time.After(3 * time.Second):
			t.Fatal("no entry arrived")
			return devlog.Entry{}
		}
	}
	if got := next().Msg; got != "before the reader connected" {
		t.Errorf("backlog entry %q", got)
	}
	// Debug is not asked for, so it must not arrive ahead of the next info line.
	s.dev.Add(devlog.Entry{Level: devlog.Debug, Msg: "an engine line"})
	s.dev.Add(devlog.Entry{Msg: "written while following"})
	if got := next().Msg; got != "written while following" {
		t.Errorf("live entry %q, want the info line and not the debug one", got)
	}
}

// Capture off means off: the developer log must lose its bodies with the
// traffic ring, not keep prompts until its own ring rolls over.
func TestSwitchingCaptureOffClearsTheDevlogToo(t *testing.T) {
	s := tuneServer(t)
	s.traffic.setSettings(true, 0)
	s.dev.Add(devlog.Entry{Msg: "request received", Body: `{"messages":[{"content":"private"}]}`})
	s.traffic.setSettings(false, 0)
	for _, e := range s.dev.Recent(0, devlog.Debug) {
		if e.Body != "" {
			t.Errorf("%q kept its body after capture was switched off", e.Msg)
		}
	}
}
