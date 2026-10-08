package devlog

import (
	"fmt"
	"testing"
)

func TestLogKeepsTheLatestInOrderAndFiltersByLevel(t *testing.T) {
	l := New("minion", 3)
	for i, level := range []string{Info, Debug, Warn, Info, Error} {
		l.Add(Entry{Level: level, Msg: fmt.Sprint(i)})
	}
	for _, c := range []struct {
		name  string
		limit int
		level string
		want  string
	}{
		{"all that are held: the last three, oldest first", 0, Debug, "2 3 4"},
		{"info leaves out nothing here but would leave out debug", 0, Info, "2 3 4"},
		{"warn and worse", 0, Warn, "2 4"},
		{"only the newest", 1, Debug, "4"},
		{"a level nobody knows reads as info", 0, "loud", "2 3 4"},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := ""
			for _, e := range l.Recent(c.limit, c.level) {
				if e.Node != "minion" {
					t.Errorf("entry %s is from %q, want the log's node", e.Msg, e.Node)
				}
				got += " " + e.Msg
			}
			if got != " "+c.want {
				t.Errorf("got%s, want %s", got, c.want)
			}
		})
	}
	if seq := l.Recent(1, Debug)[0].Seq; seq != 5 {
		t.Errorf("the fifth entry has seq %d, want 5: numbering must not restart when the ring rolls", seq)
	}
}

// Prompts are recorded only while capture is on, and switching it off has to
// take what was already recorded with it. The traffic ring does; this must too.
func TestDropBodiesForgetsWhatWasCaptured(t *testing.T) {
	l := New("n", 0)
	l.Add(Entry{Msg: "request received", Body: `{"messages":[{"content":"a secret"}]}`, Truncated: true})
	l.Add(Entry{Msg: "sent to minion"})
	l.DropBodies()
	for _, e := range l.Recent(0, Debug) {
		if e.Body != "" || e.Truncated {
			t.Errorf("%q still holds a body after capture was switched off", e.Msg)
		}
	}
	if got := l.Recent(0, Debug)[0].Msg; got != "request received" {
		t.Errorf("the entry itself must stay, got %q", got)
	}
}

func TestSubscribersGetNewEntriesAndASlowOneDoesNotBlock(t *testing.T) {
	l := New("n", 0)
	l.Add(Entry{Msg: "before"})
	ch, backlog, cancel := l.Subscribe()
	if len(backlog) != 1 || backlog[0].Msg != "before" {
		t.Fatalf("backlog %v, want the one entry already held", backlog)
	}
	// More than the subscriber's buffer, with nobody reading: Add must return.
	for i := 0; i < subBuffer+50; i++ {
		l.Add(Entry{Msg: "x"})
	}
	if got := (<-ch).Msg; got != "x" {
		t.Errorf("first live entry %q, want x", got)
	}
	cancel()
	l.Add(Entry{Msg: "after cancel"}) // must not panic or block

	// A nil log is what a router or supervisor has before one is attached.
	var none *Log
	none.Add(Entry{Msg: "dropped"})
	none.DropBodies()
	if none.Recent(0, Debug) != nil {
		t.Error("a nil log holds nothing")
	}
}
