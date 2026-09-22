package runtime

import (
	"os"
	"strings"
	"testing"

	"github.com/gregbugaj/modelfabric/internal/catalog"
)

func argvString(argv []string) string { return strings.Join(argv, " ") }

func testDef() *Definition {
	d := &Definition{Name: "test", Engine: "llama.cpp", Entrypoint: "llama-server"}
	d.resolved = "/pkg/llama-server"
	d.ContextLength, d.GPULayers, d.Parallel = 8192, 99, 4
	d.DraftMax, d.BatchSize, d.UBatchSize = 3, 2048, 512
	d.CacheTypeK, d.CacheTypeV = "f16", "f16"
	d.FlashAttention = true
	return d
}

func TestSpeculationEnabledOnlyWithADraftHead(t *testing.T) {
	d := testDef()
	eng := llamaCPP{}

	withHead := catalog.Model{Key: "qwen/qwen3.8-27b", Path: "/m.gguf", DraftLayers: 1}
	a := eng.Apply(d, withHead, Requested{})
	if !a.Speculative {
		t.Fatal("a model with a draft head should speculate by default")
	}
	got := argvString(eng.Argv(d, withHead, a, "127.0.0.1", 8000))
	if !strings.Contains(got, "--spec-type draft-mtp --spec-draft-n-max 3") {
		t.Fatalf("argv missing MTP flags: %s", got)
	}

	plain := catalog.Model{Key: "llama", Path: "/m.gguf"}
	a2 := eng.Apply(d, plain, Requested{})
	if a2.Speculative {
		t.Fatal("a model without a draft head must not speculate")
	}
	if got := argvString(eng.Argv(d, plain, a2, "127.0.0.1", 8000)); strings.Contains(got, "--spec-type") {
		t.Fatalf("argv should carry no MTP flags: %s", got)
	}
}

// Asking for speculation on a model that cannot do it must not produce a flag
// the engine will reject.
func TestSpeculationRequestCannotOverrideMissingHead(t *testing.T) {
	yes := true
	a := llamaCPP{}.Apply(testDef(), catalog.Model{Key: "llama", Path: "/m.gguf"}, Requested{Speculative: &yes})
	if a.Speculative {
		t.Fatal("speculation must stay off without a draft head, even when requested")
	}
}

func TestSpeculationCanBeTurnedOff(t *testing.T) {
	no := false
	m := catalog.Model{Key: "q", Path: "/m.gguf", DraftLayers: 1}
	a := llamaCPP{}.Apply(testDef(), m, Requested{Speculative: &no})
	if a.Speculative {
		t.Fatal("an explicit off must win")
	}
}

// Without --jinja the engine uses a generic chat format: reasoning blocks and
// tool calls stop parsing, and nothing errors.
func TestArgvCarriesTemplateAndTuningFlags(t *testing.T) {
	d := testDef()
	m := catalog.Model{Key: "q", Path: "/m.gguf", Projector: "/mmproj.gguf"}
	got := argvString(llamaCPP{}.Argv(d, m, llamaCPP{}.Apply(d, m, Requested{}), "127.0.0.1", 8000))
	for _, want := range []string{
		"--jinja", "--no-webui", "--metrics",
		"--batch-size 2048", "--ubatch-size 512",
		"--cache-type-k f16", "--cache-type-v f16",
		"--mmproj /mmproj.gguf", "-fa on",
		"--alias q",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("argv missing %q\n  got: %s", want, got)
		}
	}
}

// Two loads differing only in speculation are different work, so they must not
// be de-duplicated onto one operation.
func TestFingerprintDistinguishesSpeculation(t *testing.T) {
	a := Applied{Runtime: "r", ContextLength: 8192, Speculative: true, DraftMax: 3}
	b := Applied{Runtime: "r", ContextLength: 8192, Speculative: false}
	if a.Fingerprint() == b.Fingerprint() {
		t.Fatal("fingerprints must differ when speculation differs")
	}
}

// context_length is a per-request limit. The engine's -c is total KV capacity,
// shared (--kv-unified) or divided between slots, so it must be scaled by the
// parallel slot count or concentrated traffic overflows it.
func TestContextIsPerRequest(t *testing.T) {
	d := testDef()
	m := catalog.Model{Key: "q", Path: "/m.gguf"}
	a := llamaCPP{}.Apply(d, m, Requested{Settings: Settings{ContextLength: ptr(8192), Parallel: ptr(4)}})
	if a.ContextLength != 8192 || a.KVCacheTokens != 32768 {
		t.Fatalf("context %d / kv %d, want 8192 per request and 32768 total", a.ContextLength, a.KVCacheTokens)
	}
	if got := argvString(llamaCPP{}.Argv(d, m, a, "127.0.0.1", 8000)); !strings.Contains(got, "-c 32768") {
		t.Fatalf("argv should size the pool for all slots: %s", got)
	}
}

// An engine bound to a specific address does not listen on loopback, so the
// node must reach it — and probe its readiness — on that address.
func TestLocalURLFollowsBind(t *testing.T) {
	cases := map[string]string{
		"":             "http://127.0.0.1:18000",
		"127.0.0.1":    "http://127.0.0.1:18000",
		"0.0.0.0":      "http://127.0.0.1:18000",
		"100.100.69.3": "http://100.100.69.3:18000",
		"fd7a::1":      "http://[fd7a::1]:18000",
	}
	for bind, want := range cases {
		if got := LocalURL(bind, 18000); got != want {
			t.Errorf("LocalURL(%q) = %s, want %s", bind, got, want)
		}
	}
	spec, _ := LaunchSpec(testDef(), catalog.Model{Key: "q", Path: "/m"}, Applied{}, "100.100.69.3", 18000, 1, 0, 0)
	if spec.Endpoint != "http://100.100.69.3:18000" {
		t.Fatalf("readiness endpoint %s would never answer for a tailnet-bound engine", spec.Endpoint)
	}
}

// A model.yaml's recommendations become the engine's defaults — sampling as
// flags, template variables as --chat-template-kwargs — and a model without
// one gets neither, so its argv and fingerprint are unchanged.
func TestModelYAMLDefaultsReachTheEngine(t *testing.T) {
	data, err := os.ReadFile("../yamlite/testdata/qwen3.8-27b.model.yaml")
	if err != nil {
		t.Fatal(err)
	}
	spec, err := catalog.ParseModelYAML(data)
	if err != nil {
		t.Fatal(err)
	}
	d := testDef()
	plain := catalog.Model{Key: "q", Path: "/m.gguf"}
	withSpec := plain
	withSpec.Spec = spec

	got := argvString(llamaCPP{}.Argv(d, withSpec, llamaCPP{}.Apply(d, withSpec, Requested{}), "127.0.0.1", 8000))
	for _, want := range []string{
		"--temp 1 ", "--top-k 20 ", "--top-p 0.95 ",
		// Unchecked in model.yaml means the sampler is off — not llama.cpp's
		// own default, which for min-p is 0.05.
		"--min-p 0 ", "--repeat-penalty 1 ", "--presence-penalty 0 ",
		`--chat-template-kwargs {"enable_thinking":true,"preserve_thinking":true,"reasoning_effort":"xhigh"}`,
	} {
		if !strings.Contains(got+" ", want) {
			t.Errorf("argv missing %q\n  got: %s", want, got)
		}
	}

	a0 := llamaCPP{}.Apply(d, plain, Requested{})
	bare := argvString(llamaCPP{}.Argv(d, plain, a0, "127.0.0.1", 8000))
	if strings.Contains(bare, "--temp") || strings.Contains(bare, "--chat-template-kwargs") {
		t.Errorf("defaults applied without a model.yaml: %s", bare)
	}
	if strings.Contains(a0.Fingerprint(), "-d") {
		t.Errorf("fingerprint changed for a model without model.yaml: %s", a0.Fingerprint())
	}
	if a0.Fingerprint() == (llamaCPP{}).Apply(d, withSpec, Requested{}).Fingerprint() {
		t.Error("model.yaml defaults do not affect the fingerprint")
	}
}

// The engine's slot actions stay disabled unless the supervisor names a
// directory: an engine that can be told to write files should not be able to
// by default.
func TestSlotSavePathIsOnlyPassedWhenSet(t *testing.T) {
	d := testDef()
	m := catalog.Model{Key: "q", Path: "/m.gguf"}
	a := llamaCPP{}.Apply(d, m, Requested{})
	if got := argvString(llamaCPP{}.Argv(d, m, a, "127.0.0.1", 8000)); strings.Contains(got, "--slot-save-path") {
		t.Fatalf("slot save path passed without a disk cache: %s", got)
	}
	a.SlotSavePath = "/state/slots"
	if got := argvString(llamaCPP{}.Argv(d, m, a, "127.0.0.1", 8000)); !strings.Contains(got, "--slot-save-path /state/slots") {
		t.Fatalf("slot save path missing: %s", got)
	}
}

// The prompt cache is always sized explicitly — llama-server's own 8 GiB
// default is what OOM-killed an engine on a 15 GiB host — and an operator's
// value, including 0 (off), wins over the fitted one.
func TestCacheRAMIsFittedAndOverridable(t *testing.T) {
	old := memTotal
	defer func() { memTotal = old }()
	memTotal = func() int64 { return 16 << 30 }
	d := testDef()
	m := catalog.Model{Key: "q", Path: "/m.gguf"}

	a := llamaCPP{}.Apply(d, m, Requested{})
	if got := argvString(llamaCPP{}.Argv(d, m, a, "127.0.0.1", 8000)); !strings.Contains(got, "--cache-ram 2048") {
		t.Fatalf("fitted cache-ram missing: %s", got)
	}
	off := llamaCPP{}.Apply(d, m, Requested{Settings: Settings{CacheRAM: ptr(0)}})
	if got := argvString(llamaCPP{}.Argv(d, m, off, "127.0.0.1", 8000)); !strings.Contains(got, "--cache-ram 0") {
		t.Fatalf("cache_ram 0 should turn the cache off: %s", got)
	}
	if a.Fingerprint() == off.Fingerprint() {
		t.Fatal("different cache sizes are different loads")
	}
}

// An embedding model loaded like a chat model listed in /v1/models and then
// answered /v1/embeddings with 501 "This server does not support embeddings.
// Start it with `--embeddings`". And an input longer than the physical batch
// is refused outright, so the batch has to be the context.
func TestEmbeddingModelsAreStartedToEmbed(t *testing.T) {
	ctx := func(n int) *int { return &n }
	tests := []struct {
		name string
		m    catalog.Model
		req  Requested
		want []string
		not  []string
	}{
		{name: "an embedding model gets --embeddings and a batch the size of its context",
			m:    catalog.Model{Key: "e", Path: "/e.gguf", Type: "embedding", MaxContextLength: 512, DraftLayers: 1},
			want: []string{"--embeddings", "--batch-size 512", "--ubatch-size 512", "--cache-ram 0"},
			not:  []string{"--spec-type", "--slot-save-path"}},
		{name: "a context asked for is the context it gets",
			m:    catalog.Model{Key: "e", Path: "/e.gguf", Type: "embedding", MaxContextLength: 8192},
			req:  Requested{Settings: Settings{ContextLength: ctx(2048)}},
			want: []string{"--embeddings", "--batch-size 2048", "--ubatch-size 2048"}},
		{name: "a chat model is not started to embed",
			m:   catalog.Model{Key: "q", Path: "/m.gguf", Type: "llm"},
			not: []string{"--embeddings"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d := testDef()
			got := argvString(llamaCPP{}.Argv(d, tc.m, llamaCPP{}.Apply(d, tc.m, tc.req), "127.0.0.1", 8000))
			for _, w := range tc.want {
				if !strings.Contains(got, w) {
					t.Errorf("argv missing %q\n  got: %s", w, got)
				}
			}
			for _, n := range tc.not {
				if strings.Contains(got, n) {
					t.Errorf("argv has %q\n  got: %s", n, got)
				}
			}
		})
	}
}
