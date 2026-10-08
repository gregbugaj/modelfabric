package main

import (
	"strings"
	"testing"
	"time"
)

// Nodes do not deliver together: one node's backlog arrives whole before the
// next node's first line. Merged, the entries must read in the order things
// happened, and two entries one node stamped with the same instant must keep
// the order that node gave them.
func TestDevLogOrderMergesNodesByTime(t *testing.T) {
	at := func(ms int) time.Time { return time.Unix(1700000000, 0).Add(time.Duration(ms) * time.Millisecond) }
	got := devLogOrder([]devEntry{
		{Node: "b", Time: at(30), Msg: "b sent"},
		{Node: "b", Time: at(30), Msg: "b slot chosen"},
		{Node: "a", Time: at(10), Msg: "a received"},
		{Node: "a", Time: at(40), Msg: "a done"},
	})
	var msgs []string
	for _, e := range got {
		msgs = append(msgs, e.Msg)
	}
	want := "a received, b sent, b slot chosen, a done"
	if strings.Join(msgs, ", ") != want {
		t.Fatalf("order = %q, want %q", strings.Join(msgs, ", "), want)
	}
}

func TestDevLogLineNamesTheNodeOnlyWhenMerged(t *testing.T) {
	at := time.Unix(1700000000, 0)
	cases := []struct {
		name, node string
		want       bool
	}{
		{"one node: no node column", "", false},
		{"merged: the node that wrote the line is on it", "minion", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			line := devLogLine(at, "info", "router", c.node, "m", "a1b2c3d4", "sent to minion: it holds 94% of this prompt")
			before, _, _ := strings.Cut(line, "a1b2c3")
			if strings.Contains(before, "minion") != c.want {
				t.Fatalf("line %q: node column present = %v, want %v", line, !c.want, c.want)
			}
		})
	}
}
