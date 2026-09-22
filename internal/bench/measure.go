package bench

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// sample is one request, measured from both ends: the client's clock, which
// is what an app waiting on it sees, and the engine's own timings, which say
// how the time split between reading the prompt and generating.
type sample struct {
	start, first, end time.Time

	promptTokens int // as the engine counted them, the chat template included
	cachedTokens int // of those, reused from the prompt cache rather than read
	outTokens    int

	// From llama-server's "timings". Zero when the engine reports none (an
	// MLX engine): the report then falls back to the client's clock and says so.
	promptMs, predictMs float64
	engineTimed         bool

	// node is the machine that served it, from X-Fabric-Node: through the
	// front door the router chooses, and the cluster benchmark reports where.
	node string

	err error
}

func (s sample) ttftMs() float64 { return float64(s.first.Sub(s.start).Microseconds()) / 1000 }
func (s sample) e2e() float64    { return s.end.Sub(s.start).Seconds() }

// ppTPS is prompt tokens read per second, counting only those the engine
// actually processed: a cached prefix costs nothing and must not inflate it.
func (s sample) ppTPS() float64 {
	read := s.promptTokens - s.cachedTokens
	if s.engineTimed && s.promptMs > 0 {
		return float64(read) / s.promptMs * 1000
	}
	if t := s.first.Sub(s.start).Seconds(); t > 0 {
		return float64(read) / t
	}
	return 0
}

// tgTPS is tokens generated per second, after the first.
func (s sample) tgTPS() float64 {
	if s.engineTimed && s.predictMs > 0 {
		return float64(s.outTokens) / s.predictMs * 1000
	}
	if t := s.end.Sub(s.first).Seconds(); t > 0 && s.outTokens > 1 {
		return float64(s.outTokens-1) / t
	}
	return 0
}

// tpotMs is time per output token.
func (s sample) tpotMs() float64 {
	if tg := s.tgTPS(); tg > 0 {
		return 1000 / tg
	}
	return 0
}

type client struct {
	base, model string
	http        *http.Client
	// key, when set, is sent as a bearer token: the front door asks for one
	// when require_api_key is on.
	key string
}

func newClient(base, model string) *client {
	return &client{base: strings.TrimRight(base, "/"), model: model, http: &http.Client{Timeout: 30 * time.Minute}}
}

// generate sends one chat request for exactly out tokens and measures it.
// Exactly: ignore_eos keeps the engine generating past a natural end, so
// every run produces the same amount of work whatever the model would have
// said. Temperature 0 and a fixed seed make what it says repeatable too.
func (c *client) generate(ctx context.Context, prompt string, out int) sample {
	body, _ := json.Marshal(map[string]any{
		"model":          c.model,
		"messages":       []map[string]string{{"role": "user", "content": prompt}},
		"max_tokens":     out,
		"n_predict":      out,
		"ignore_eos":     true,
		"temperature":    0,
		"seed":           42,
		"cache_prompt":   true,
		"stream":         true,
		"stream_options": map[string]any{"include_usage": true},
	})
	s := sample{start: time.Now()}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		s.err = err
		return s
	}
	req.Header.Set("Content-Type", "application/json")
	if c.key != "" {
		req.Header.Set("Authorization", "Bearer "+c.key)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		s.err = err
		return s
	}
	defer resp.Body.Close()
	s.node = resp.Header.Get("X-Fabric-Node")
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		s.err = fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(b)))
		return s
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	for sc.Scan() {
		line, ok := strings.CutPrefix(sc.Text(), "data: ")
		if !ok || line == "[DONE]" {
			continue
		}
		var ch struct {
			Choices []struct {
				Delta struct {
					Content          string `json:"content"`
					ReasoningContent string `json:"reasoning_content"`
				} `json:"delta"`
			} `json:"choices"`
			Usage *struct {
				PromptTokens     int `json:"prompt_tokens"`
				CompletionTokens int `json:"completion_tokens"`
				Details          *struct {
					Cached int `json:"cached_tokens"`
				} `json:"prompt_tokens_details"`
			} `json:"usage"`
			Timings *struct {
				PromptN     int     `json:"prompt_n"`
				CacheN      int     `json:"cache_n"`
				PromptMs    float64 `json:"prompt_ms"`
				PredictedN  int     `json:"predicted_n"`
				PredictedMs float64 `json:"predicted_ms"`
			} `json:"timings"`
		}
		if json.Unmarshal([]byte(line), &ch) != nil {
			continue
		}
		// The first token is the first content of either kind: a reasoning
		// model's thinking is generated output too.
		if s.first.IsZero() {
			for _, c := range ch.Choices {
				if c.Delta.Content != "" || c.Delta.ReasoningContent != "" {
					s.first = time.Now()
				}
			}
		}
		if u := ch.Usage; u != nil {
			s.promptTokens, s.outTokens = u.PromptTokens, u.CompletionTokens
			if u.Details != nil {
				s.cachedTokens = u.Details.Cached
			}
		}
		if t := ch.Timings; t != nil && t.PredictedN > 0 {
			s.engineTimed = true
			s.promptMs, s.predictMs = t.PromptMs, t.PredictedMs
			s.outTokens = t.PredictedN
			// prompt_n is what the engine read; cache_n what it reused.
			s.cachedTokens = t.CacheN
			s.promptTokens = t.PromptN + t.CacheN
		}
	}
	s.end = time.Now()
	// Tokens generated but none visible: a reasoning model's first tokens can
	// be markup the engine strips from the stream (Qwen3 opens with <think>),
	// so a one-token request shows nothing although it generated. The engine's
	// own clock then says when the first token came.
	if s.first.IsZero() && s.outTokens > 0 && s.engineTimed {
		s.first = s.start.Add(time.Duration(s.promptMs * float64(time.Millisecond)))
	}
	if err := sc.Err(); err != nil {
		s.err = err
	} else if s.first.IsZero() {
		s.err = fmt.Errorf("the engine answered but generated nothing")
	}
	return s
}

// tokens counts text with the engine's own tokenizer, and cut returns a
// prefix of exactly n of its tokens as text. llama-server serves both; an
// engine that does not gets an estimate, and the report then states the
// prompt length the engine itself counted.
func (c *client) tokenize(ctx context.Context, text string) ([]int, error) {
	var out struct {
		Tokens []int `json:"tokens"`
	}
	err := c.post(ctx, "/tokenize", map[string]any{"content": text, "add_special": false}, &out)
	return out.Tokens, err
}

func (c *client) detokenize(ctx context.Context, toks []int) (string, error) {
	var out struct {
		Content string `json:"content"`
	}
	err := c.post(ctx, "/detokenize", map[string]any{"tokens": toks}, &out)
	return out.Content, err
}

// post retries once: /tokenize and /detokenize change nothing, and a
// kept-alive connection the engine had just closed failed the 4x batch with a
// bare EOF on a request that would have worked on a fresh one.
func (c *client) post(ctx context.Context, path string, in, out any) error {
	err := c.postOnce(ctx, path, in, out)
	if err != nil && ctx.Err() == nil {
		err = c.postOnce(ctx, path, in, out)
	}
	return err
}

func (c *client) postOnce(ctx context.Context, path string, in, out any) error {
	b, _ := json.Marshal(in)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+path, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.key != "" {
		req.Header.Set("Authorization", "Bearer "+c.key)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s %s", path, resp.Status)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}
