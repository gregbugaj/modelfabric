package server

import (
	"testing"
	"time"

	"github.com/gregbugaj/modelfabric/internal/router"
)

// recent is what a peer answers when another node's dashboard asks what it has
// served, so its order and its discretion about bodies both matter.
func TestTrafficRecent(t *testing.T) {
	tr := newTraffic(newMetrics())
	tr.setSettings(true, 10)
	for i := range 5 {
		tr.publish(router.Event{
			Time: time.Unix(int64(i), 0), Path: "/v1/chat/completions",
			ReqBody: "prompt", RespBody: "reply",
		})
	}

	t.Run("newest first", func(t *testing.T) {
		got := tr.recent(0, false)
		if len(got) != 5 {
			t.Fatalf("got %d events, want 5", len(got))
		}
		if got[0].Time.Unix() != 4 || got[4].Time.Unix() != 0 {
			t.Errorf("out of order: first %d, last %d", got[0].Time.Unix(), got[4].Time.Unix())
		}
	})

	t.Run("limit takes the newest", func(t *testing.T) {
		got := tr.recent(2, false)
		if len(got) != 2 || got[0].Time.Unix() != 4 || got[1].Time.Unix() != 3 {
			t.Fatalf("got %d events starting at %d, want the two newest", len(got), got[0].Time.Unix())
		}
	})

	t.Run("bodies withheld unless asked for", func(t *testing.T) {
		if e := tr.recent(1, false)[0]; e.ReqBody != "" || e.RespBody != "" {
			t.Errorf("bodies leaked without asking: %q / %q", e.ReqBody, e.RespBody)
		}
		if e := tr.recent(1, true)[0]; e.ReqBody != "prompt" || e.RespBody != "reply" {
			t.Errorf("bodies missing when asked for: %q / %q", e.ReqBody, e.RespBody)
		}
	})

	// The ring is the only copy, so stripping a body for one caller must not
	// take it away from the next.
	t.Run("stripping does not mutate the ring", func(t *testing.T) {
		_ = tr.recent(5, false)
		if e := tr.recent(1, true)[0]; e.ReqBody != "prompt" {
			t.Errorf("ring lost its body after a metadata-only read: %q", e.ReqBody)
		}
	})

	// Capture off clears what was kept: a peer cannot serve what its own
	// operator has switched off.
	t.Run("capture off clears bodies", func(t *testing.T) {
		tr.setSettings(false, 10)
		if e := tr.recent(1, true)[0]; e.ReqBody != "" {
			t.Errorf("body survived capture being turned off: %q", e.ReqBody)
		}
	})
}
