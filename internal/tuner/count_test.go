package tuner

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Engines do not agree on what to call a thinking token, and a sweep that reads
// only one of the names measures a thinking model at zero.
//
// Both shapes below are captured from live engines. The mlx-lm one arrives after
// a run of `: keepalive N/54` comment frames, which are not data lines and must
// not be counted either.
const (
	llamaFrames = `data: {"choices":[{"index":0,"delta":{"reasoning_content":"We"}}]}

data: {"choices":[{"index":0,"delta":{"reasoning_content":" need"}}]}

data: {"choices":[{"index":0,"delta":{"content":"4"}}]}

data: [DONE]

`
	mlxFrames = `: keepalive 1/54

: keepalive 2/54

data: {"choices": [{"index": 0, "finish_reason": null, "delta": {"role": "assistant", "reasoning": "We"}}]}

data: {"choices": [{"index": 0, "finish_reason": null, "delta": {"role": "assistant", "reasoning": " need"}}]}

data: {"choices": [{"index": 0, "finish_reason": null, "delta": {"role": "assistant", "content": "4"}}]}

data: [DONE]

`
)

func streamingServer(t *testing.T, frames string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/chat/completions") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, frames)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestTokensAreCountedWhateverTheEngineCallsThem(t *testing.T) {
	for _, tc := range []struct {
		engine string
		frames string
	}{
		{"llama.cpp", llamaFrames},
		{"mlx-lm", mlxFrames},
	} {
		t.Run(tc.engine, func(t *testing.T) {
			base := streamingServer(t, tc.frames)
			n, ttft, err := streamOnce(context.Background(), base, "m", "hello", 300)
			if err != nil {
				t.Fatalf("streamOnce: %v", err)
			}
			// Two thinking tokens and one of reply. The keepalive comments and
			// [DONE] are not tokens.
			if n != 3 {
				t.Errorf("counted %d tokens, want 3", n)
			}
			if ttft <= 0 {
				t.Error("no time to first token was recorded")
			}
		})
	}
}

type serverEngine struct{ base string }

func (s serverEngine) Reload(context.Context, string, int, int) error { return nil }
func (s serverEngine) Target(context.Context, string) (string, string, error) {
	return s.base, "m", nil
}
func (s serverEngine) Inflight(context.Context, string) (int, error)     { return 0, nil }
func (s serverEngine) Current(context.Context, string) (int, int, error) { return 1, 4096, nil }

// Regression: unrecognized token fields produced a zero rate and a slot
// recommendation despite successful generation. Treat an uncountable reply
// as a measurement failure.
func TestARowThatProducedNoTokensIsNotAMeasurement(t *testing.T) {
	withMemory(t, 36*gb, 20*gb)
	// Well-formed frames whose token lives under a name nothing reads: the
	// stand-in for the next engine to invent a field.
	base := streamingServer(t, `data: {"choices":[{"index":0,"delta":{"thinking":"We"}}]}

data: [DONE]

`)
	row := measure(context.Background(), serverEngine{base}, Config{Model: "m", Context: 4096, Output: 300}, 1)
	if row.Fit {
		t.Error("a row that produced no tokens was reported as a fit")
	}
	if row.Aggregate != 0 || row.DecodeTokS != 0 {
		t.Errorf("rates published from nothing: %+v", row)
	}
	if !strings.Contains(row.Error, "without a token") {
		t.Errorf("the row should say it could not count, got %q", row.Error)
	}
}

func TestNoSlotCountIsRecommendedFromARowWithNoRate(t *testing.T) {
	// Exactly what the Mac produced: one row, no error from the request itself,
	// no tokens.
	n, why := recommend([]Row{{Slots: 1, Error: "1 of 1 requests finished without a token this sweep could count"}})
	if n != 0 {
		t.Errorf("recommended %d slots on the strength of a row with no rate", n)
	}
	if !strings.Contains(why, "nothing completed") {
		t.Errorf("the reason should say nothing completed, got %q", why)
	}
}
