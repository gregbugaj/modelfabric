package bench

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fakeLlama is a llama-server: one token per word, a two-token chat template,
// and a prompt cache holding the last prompt each request saw.
type fakeLlama struct {
	mu       sync.Mutex
	lastSeen string
	prompts  []int
}

func words(s string) []string { return strings.Fields(s) }

func (f *fakeLlama) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /tokenize", func(w http.ResponseWriter, r *http.Request) {
		var in struct{ Content string }
		json.NewDecoder(r.Body).Decode(&in)
		toks := make([]int, len(words(in.Content)))
		for i := range toks {
			toks[i] = i
		}
		json.NewEncoder(w).Encode(map[string]any{"tokens": toks})
	})
	mux.HandleFunc("POST /detokenize", func(w http.ResponseWriter, r *http.Request) {
		var in struct{ Tokens []int }
		json.NewDecoder(r.Body).Decode(&in)
		json.NewEncoder(w).Encode(map[string]any{"content": strings.Repeat("w ", len(in.Tokens))})
	})
	mux.HandleFunc("POST /v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Messages  []struct{ Content string }
			MaxTokens int `json:"max_tokens"`
		}
		json.NewDecoder(r.Body).Decode(&in)
		text := in.Messages[0].Content
		n := len(words(text)) + 2 // the template's two tokens
		f.mu.Lock()
		cached := 0
		if text == f.lastSeen {
			cached = n - 1
		}
		f.lastSeen = text
		f.prompts = append(f.prompts, n)
		f.mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		for i := 0; i < in.MaxTokens; i++ {
			fmt.Fprintf(w, "data: %s\n\n", `{"choices":[{"delta":{"content":"x"}}]}`)
		}
		timings := map[string]any{"prompt_n": n - cached, "cache_n": cached, "prompt_ms": 10.0, "predicted_n": in.MaxTokens, "predicted_ms": 100.0}
		b, _ := json.Marshal(map[string]any{"choices": []any{}, "timings": timings})
		fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", b)
	})
	return mux
}

type fakeEngine struct {
	base     string
	reloads  [][2]int
	mu       sync.Mutex
	restored bool
}

func (e *fakeEngine) Reload(_ context.Context, _ string, ctx, slots int) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.reloads = append(e.reloads, [2]int{ctx, slots})
	return nil
}
func (e *fakeEngine) Target(context.Context, string) (string, string, error) { return e.base, "m", nil }
func (e *fakeEngine) Inflight(context.Context, string) (int, error)          { return 0, nil }
func (e *fakeEngine) Current(context.Context, string) (int, int, error)      { return 4, 8192, nil }
func (e *fakeEngine) Loaded(context.Context, string) (int, string, map[string]any, error) {
	return 0, "fake", map[string]any{}, nil
}

func setup(t *testing.T) (*fakeLlama, *fakeEngine) {
	t.Helper()
	f := &fakeLlama{}
	srv := httptest.NewServer(f.handler())
	t.Cleanup(srv.Close)
	return f, &fakeEngine{base: srv.URL}
}

// "pp1024" has to mean 1024 prompt tokens as the engine counts them, chat
// template included, or two engines' rows would be measuring different work.
func TestPromptsAreTheLengthAskedFor(t *testing.T) {
	f, e := setup(t)
	rep, err := Run(context.Background(), e, nil, Config{Model: "m", PP: []int{512, 2048}, Batch: []int{}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for i, want := range []int{512, 2048} {
		if got := rep.Single[i].PromptTokens; got != want {
			t.Errorf("%s: %d prompt tokens, want %d", rep.Single[i].Test, got, want)
		}
	}
	if len(f.prompts) == 0 {
		t.Fatal("no requests reached the engine")
	}
}

// Tokens read from the cache cost nothing; counting them made a same-prompt
// batch look several times faster at reading prompts than the GPU is.
func TestCachedTokensDoNotCountAsRead(t *testing.T) {
	cached := sample{promptTokens: 1000, cachedTokens: 999, promptMs: 10, engineTimed: true}
	fresh := sample{promptTokens: 1000, cachedTokens: 0, promptMs: 10, engineTimed: true}
	if cached.ppTPS() >= fresh.ppTPS() {
		t.Fatalf("pp TPS with the prompt cached (%v) is not below reading it (%v)", cached.ppTPS(), fresh.ppTPS())
	}
}

func TestEngineIsRestored(t *testing.T) {
	tests := []struct {
		name   string
		cancel bool
	}{
		{name: "after a full run"},
		{name: "after a cancelled run", cancel: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, e := setup(t)
			ctx, cancel := context.WithCancel(context.Background())
			progress := func(r Report) {
				if tc.cancel && len(r.Single) > 0 {
					cancel()
				}
			}
			rep, _ := Run(ctx, e, nil, Config{Model: "m", PP: []int{256, 512}, Batch: []int{2}}, progress)
			cancel()
			last := e.reloads[len(e.reloads)-1]
			if last != [2]int{8192, 4} {
				t.Fatalf("last load %v, want the original 8192 context × 4 slots; loads: %v", last, e.reloads)
			}
			if tc.cancel && !rep.Partial {
				t.Fatal("a cancelled run is not marked partial")
			}
		})
	}
}

// Each phase loads what it needs and no more: one long slot for the prompt
// sweep, many short ones for batching. One load for both would need 8 × 32K
// of KV cache, more than a 27B model leaves on a 32 GB GPU.
func TestPhasesLoadWhatTheyNeed(t *testing.T) {
	_, e := setup(t)
	if _, err := Run(context.Background(), e, nil, Config{Model: "m", PP: []int{1024, 8192}, Batch: []int{2, 8}}, nil); err != nil {
		t.Fatal(err)
	}
	if e.reloads[0] != [2]int{9216, 1} || e.reloads[1] != [2]int{2048, 8} {
		t.Fatalf("loads %v, want [9216 1] for the sweep then [2048 8] for batching", e.reloads[:2])
	}
}

func TestTextSaysHowToRepeatIt(t *testing.T) {
	_, e := setup(t)
	rep, err := Run(context.Background(), e, nil, Config{Model: "m", PP: []int{256}, Batch: []int{2}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	rep.Command = "mfsh bench -model m -prompts prose -pp 256 -tg 128 -batch 2 -batch-pp 1024 -reps 1"
	text := rep.Text()
	for _, want := range []string{"pp256/tg128", "same prompt", "different prompts", "Repeat with:", rep.Command, "prose-v1"} {
		if !strings.Contains(text, want) {
			t.Errorf("text report lacks %q:\n%s", want, text)
		}
	}
}

func TestCorpusIsFixed(t *testing.T) {
	for _, name := range []string{"prose", "code"} {
		c, err := LoadCorpus(name)
		if err != nil {
			t.Fatal(err)
		}
		if len(c.SHA256) != 64 || c.Text(10) == "" {
			t.Fatalf("%s: sha %q", name, c.SHA256)
		}
		if got := len(c.Text(len(c.text) * 3)); got != len(c.text)*3 {
			t.Fatalf("%s: wrapped text is %d bytes", name, got)
		}
	}
	if _, err := LoadCorpus("poetry"); err == nil {
		t.Fatal("an unknown prompt set was accepted")
	}
}

// helion's report named no hardware: there is no nvidia-smi on a Mac.
func TestMacGPU(t *testing.T) {
	name, driver, mb := macGPU("Apple M5 Pro\n51539607552\n")
	if name != "Apple M5 Pro (unified memory)" || driver != "" || mb != 49152 {
		t.Fatalf("got %q %q %d", name, driver, mb)
	}
	if name, _, mb := macGPU("garbage"); name != "" || mb != 0 {
		t.Fatalf("unparseable output gave %q %d", name, mb)
	}
}
