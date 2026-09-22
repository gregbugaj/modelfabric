package main

import (
	"strings"
	"testing"
	"time"
)

// The footer's headroom line, which exists because a conversation that runs out
// of context fails as an error with the conversation lost, and until the mesh
// published a context length there was nothing here to warn with. The only tasks
// a 2026-09-24 benchmark run failed to solve died at ~130,900 tokens of 131,072.
func TestFooterShowsHeadroomAgainstTheNextTurn(t *testing.T) {
	s := &chatSession{}
	s.last = &chatTurn{
		at: time.Now(), node: "minion", engine: "inst-1",
		total: time.Second, promptTokens: 20_000, completionTokens: 1_000,
		contextLen: 65536,
	}
	out := capture(t, s.printStats)
	// 21,000 of 65,536 is what the *next* prompt carries: everything already
	// said plus the reply just received.
	if !strings.Contains(out, "21,000 of 65,536 tokens") {
		t.Errorf("headroom should count the next turn's prompt:\n%s", out)
	}
	if !strings.Contains(out, "32%") {
		t.Errorf("headroom should state the share used:\n%s", out)
	}
}

// Nothing is shown when the limit is unknown. mlx-lm serves no /props, and a
// percentage against a guessed limit is the kind of number that gets believed.
func TestFooterShowsNoHeadroomWhenTheLimitIsUnknown(t *testing.T) {
	s := &chatSession{}
	s.last = &chatTurn{
		at: time.Now(), total: time.Second, promptTokens: 20_000, contextLen: 0,
	}
	out := capture(t, s.printStats)
	if strings.Contains(out, "headroom") {
		t.Errorf("no context length means no headroom line:\n%s", out)
	}
}

// Close to the limit it has to be impossible to miss, and it has to say what to
// do: the next turn is likely to be refused, and the refusal loses the thread.
func TestFooterWarnsBeforeTheContextRunsOut(t *testing.T) {
	s := &chatSession{}
	s.last = &chatTurn{
		at: time.Now(), total: time.Second,
		promptTokens: 51_000, completionTokens: 0, contextLen: 65536, // ~78%
	}
	near := capture(t, s.printStats)
	if !strings.Contains(near, "headroom") {
		t.Fatalf("expected a headroom line:\n%s", near)
	}

	s.last = &chatTurn{
		at: time.Now(), total: time.Second,
		promptTokens: 60_000, completionTokens: 0, contextLen: 65536, // ~92%
	}
	full := capture(t, s.printStats)
	if !strings.Contains(full, "/clear") {
		t.Errorf("past 90%% the line should say what to do about it:\n%s", full)
	}
}

// An engine running a different context than it was loaded with makes every
// figure above it measured against the wrong limit, so it is stated rather than
// left to be inferred.
func TestFooterSurfacesAContextDisagreement(t *testing.T) {
	s := &chatSession{}
	s.last = &chatTurn{
		at: time.Now(), total: time.Second, promptTokens: 100, contextLen: 32768,
		contextNote: "loaded for 65536 tokens per request but the engine reports 131072 across 4 slots: it is running 32768",
	}
	out := capture(t, s.printStats)
	if !strings.Contains(out, "it is running 32768") {
		t.Errorf("the disagreement should be shown:\n%s", out)
	}
}

func TestCommaGroupsThousands(t *testing.T) {
	for in, want := range map[int]string{
		0: "0", 999: "999", 1000: "1,000", 65536: "65,536",
		131072: "131,072", 130900: "130,900", -1500: "-1,500",
	} {
		if got := comma(in); got != want {
			t.Errorf("comma(%d) = %q, want %q", in, got, want)
		}
	}
}
