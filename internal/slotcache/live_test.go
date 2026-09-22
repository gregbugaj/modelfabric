package slotcache_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/gregbugaj/modelfabric/internal/config"
	"github.com/gregbugaj/modelfabric/internal/engineshim"
	"github.com/gregbugaj/modelfabric/internal/mesh"
	"github.com/gregbugaj/modelfabric/internal/router"
	"github.com/gregbugaj/modelfabric/internal/slotcache"
)

// TestLiveColdTier drives the real router and engine shim against a real
// llama-server, because the unit tests only prove this package agrees with its
// own idea of the engine. The cache places requests in the shim, as the
// supervisor wires it, so a conversation is cached the same whether it came
// through ModelFabric's router or straight to the shim the way llm-d's Envoy
// sends it. Skipped unless pointed at one:
//
//	llama-server -m model.gguf --port 18999 -np 1 --cache-ram 0 --slot-save-path /some/dir
//	MF_LIVE_ENGINE=http://127.0.0.1:18999 MF_LIVE_SLOTDIR=/some/dir go test ./internal/slotcache -run Live -v
//
// One slot and no host-RAM cache, so that a conversation coming back warm can
// only have come from disk.
func TestLiveColdTier(t *testing.T) {
	base, dir := os.Getenv("MF_LIVE_ENGINE"), os.Getenv("MF_LIVE_SLOTDIR")
	if base == "" || dir == "" {
		t.Skip("set MF_LIVE_ENGINE and MF_LIVE_SLOTDIR to run against a real llama-server")
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	store, err := slotcache.Open(dir, 0, &http.Client{}, log)
	if err != nil {
		t.Fatal(err)
	}
	store.Attach("engine", base, "live", 1)

	// The shim in front of the engine, holding the cache, and the router
	// pointed at it: what the supervisor does when the disk cache is on.
	sh, err := engineshim.New(base, log)
	if err != nil {
		t.Fatal(err)
	}
	port, err := sh.Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sh.Close() })
	sh.SetCache("engine", store)
	shimURL := fmt.Sprintf("http://127.0.0.1:%d", port)

	m := mesh.New(config.Default(), "self")
	e := mesh.NewEngine("engine", base)
	e.SetVia(shimURL)
	e.MarkReady("m")
	e.SetSlots(1)
	m.RegisterEngine(e)
	r := router.New(m, log)
	r.EnablePrefixAffinity()

	doc := func(seed int64) string {
		rng := rand.New(rand.NewSource(seed))
		words := strings.Fields("alpha beta gamma delta engine cache router mesh token prefix slot node gpu model fabric")
		var b strings.Builder
		for i := 0; i < 6000; i++ {
			b.WriteString(words[rng.Intn(len(words))])
			b.WriteByte(' ')
		}
		return b.String()
	}
	direct := false
	ask := func(path string, seed int64, question string) (promptN, cacheN int) {
		t.Helper()
		body := map[string]any{"model": "m", "max_tokens": 8,
			"messages": []map[string]string{{"role": "user", "content": question + " /no_think"}}}
		if path == "/v1/messages" {
			body["system"] = "Reference document: " + doc(seed)
		} else {
			body["messages"] = []map[string]string{
				{"role": "system", "content": "Reference document: " + doc(seed)},
				{"role": "user", "content": question + " /no_think"}}
		}
		raw, _ := json.Marshal(body)
		var out []byte
		if direct {
			// Straight to the shim, as llm-d's Envoy dials it: no router.
			resp, err := http.Post(shimURL+path, "application/json", bytes.NewReader(raw))
			if err != nil {
				t.Fatal(err)
			}
			out, _ = io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("%s direct: %d %s", path, resp.StatusCode, out)
			}
		} else {
			rec := httptest.NewRecorder()
			r.Forward(rec, httptest.NewRequest(http.MethodPost, path, bytes.NewReader(raw)), path)
			out, _ = io.ReadAll(rec.Body)
			if rec.Code != http.StatusOK {
				t.Fatalf("%s: %d %s", path, rec.Code, out)
			}
		}
		var resp struct {
			Timings struct {
				PromptN int `json:"prompt_n"`
				CacheN  int `json:"cache_n"`
			} `json:"timings"`
			Usage struct {
				InputTokens int `json:"input_tokens"`
				CacheRead   int `json:"cache_read_input_tokens"`
			} `json:"usage"`
		}
		_ = json.Unmarshal(out, &resp)
		if path == "/v1/messages" {
			return resp.Usage.InputTokens, resp.Usage.CacheRead
		}
		return resp.Timings.PromptN, resp.Timings.CacheN
	}

	step := func(what string, p, c int) {
		fmt.Printf("%-52s read %5d tokens, reused %5d\n", what, p, c)
	}
	p, c := ask("/v1/chat/completions", 1, "one?")
	step("A, first time", p, c)
	p, c = ask("/v1/chat/completions", 2, "one?")
	step("B, takes A's only slot", p, c)
	p, c = ask("/v1/chat/completions", 1, "two?")
	step("A returns", p, c)
	if c < 5000 {
		t.Errorf("A came back cold: reused %d tokens of ~6000; the slot was not restored from disk", c)
	}
	p, c = ask("/v1/chat/completions", 2, "two?")
	step("B returns", p, c)
	if c < 5000 {
		t.Errorf("B came back cold: reused %d tokens", c)
	}
	// The Anthropic shape, which Claude Code speaks: id_slot has to survive
	// llama-server's conversion of it.
	p, c = ask("/v1/messages", 3, "one?")
	step("C over /v1/messages", p, c)
	ask("/v1/messages", 4, "one?")
	p, c = ask("/v1/messages", 3, "two?")
	step("C returns over /v1/messages", p, c)
	if c < 5000 {
		t.Errorf("C came back cold over /v1/messages: reused %d tokens", c)
	}
	// One conversation by both routes: first straight to the shim, as llm-d
	// sends it, then back through the router. Before the cache moved into the
	// shim, the first half bypassed it entirely.
	direct = true
	p, c = ask("/v1/chat/completions", 5, "one?")
	step("D straight to the shim (llm-d's path)", p, c)
	p, c = ask("/v1/chat/completions", 6, "one?")
	step("E straight to the shim, takes D's slot", p, c)
	direct = false
	p, c = ask("/v1/chat/completions", 5, "two?")
	step("D returns through the router", p, c)
	if c < 5000 {
		t.Errorf("D came back cold through the router after arriving by the shim: reused %d tokens", c)
	}
	fmt.Printf("%+v\n", store.Stats())
}
