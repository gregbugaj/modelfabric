package catalog

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeHubEntry(t *testing.T, root, owner, name string, repos ...[2]string) {
	t.Helper()
	dir := filepath.Join(root, "hub", "models", owner, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	srcs := ""
	for i, r := range repos {
		if i > 0 {
			srcs += ","
		}
		srcs += `{"type":"huggingface","user":"` + r[0] + `","repo":"` + r[1] + `"}`
	}
	body := `{"type":"model","owner":"` + owner + `","name":"` + name + `",
	  "dependencies":[{"purpose":"baseModel","sources":[` + srcs + `]}]}`
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestHubResolvesIdentityFromCatalog(t *testing.T) {
	lm := t.TempDir()
	writeHubEntry(t, lm, "qwen", "qwen3.8-27b",
		[2]string{"lmstudio-community", "Qwen3.8-27B-GGUF"},
		[2]string{"lmstudio-community", "Qwen3.8-27B-MLX-4bit"})

	h := LoadHub(lm)
	if h.Len() != 2 {
		t.Fatalf("indexed %d repos, want 2", h.Len())
	}
	// Repo matching must ignore case: the directory on disk is capitalised,
	// the manifest may not be.
	e, ok := h.LookupPath("lmstudio-community/Qwen3.8-27B-GGUF/Qwen3.8-27B-Q4_K_M")
	if !ok {
		t.Fatal("did not match the model file back to its catalog entry")
	}
	if e.ID != "qwen/qwen3.8-27b" || e.Owner != "qwen" {
		t.Fatalf("entry = %+v", e)
	}
}

// The catalog states identity; GGUF metadata only implies it. A model whose
// metadata is missing or odd must still get its real name.
func TestCatalogIdentityBeatsDerivedMetadata(t *testing.T) {
	lm := t.TempDir()
	writeHubEntry(t, lm, "qwen", "qwen3.8-27b", [2]string{"lmstudio-community", "Qwen3.8-27B-GGUF"})

	models := t.TempDir()
	writeGGUF(t, models, "lmstudio-community/Qwen3.8-27B-GGUF/weights.gguf",
		ggufKV("general.architecture", ggufString, ggufStr("qwen35")),
		// Deliberately wrong/absent identity metadata.
		ggufKV("general.basename", ggufString, ggufStr("Something_Else")),
		ggufKV("general.size_label", ggufString, ggufStr("99B")),
	)
	c, err := ScanRootsWithHub(LoadHub(lm), models)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if _, ok := c.Lookup("qwen/qwen3.8-27b"); !ok {
		t.Fatalf("catalog identity not used; have %v", c.Models())
	}
	if _, ok := c.Lookup("something/else-99b"); ok {
		t.Fatal("derived identity should have been overridden by the catalog")
	}
}

// No LM Studio install, or no catalog entry, must fall back to derivation
// rather than losing the model.
func TestMissingCatalogFallsBackToDerivation(t *testing.T) {
	models := t.TempDir()
	writeGGUF(t, models, "someone/repo/weights.gguf",
		ggufKV("general.basename", ggufString, ggufStr("Qwen_Qwen3.8")),
		ggufKV("general.size_label", ggufString, ggufStr("27B")))

	for _, h := range []*Hub{nil, LoadHub(""), LoadHub(t.TempDir())} {
		c, err := ScanRootsWithHub(h, models)
		if err != nil {
			t.Fatalf("scan: %v", err)
		}
		m, ok := c.Lookup("qwen/qwen3.8-27b")
		if !ok {
			t.Fatalf("derived identity lost; have %v", c.Models())
		}
		if m.HubID != "" {
			t.Fatal("HubID should be empty when identity was derived, not read")
		}
	}
}

func TestHubIgnoresMalformedEntries(t *testing.T) {
	lm := t.TempDir()
	dir := filepath.Join(lm, "hub", "models", "broken", "entry")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	// An entry with no huggingface sources is unusable for path matching.
	writeHubEntry(t, lm, "empty", "model")
	if got := LoadHub(lm).Len(); got != 0 {
		t.Fatalf("indexed %d repos from malformed entries, want 0", got)
	}
}

// `mfsh get user/repo` suggests `mfsh load user/repo`; the repo must resolve to
// the model inside it, and refuse when it holds several.
func TestResolveByRepositoryPath(t *testing.T) {
	root := t.TempDir()
	writeGGUF(t, root, "lmstudio-community/Qwen3-0.6B-GGUF/Qwen3-0.6B-Q4_K_M.gguf",
		ggufKV("general.basename", ggufString, ggufStr("Qwen_Qwen3")),
		ggufKV("general.size_label", ggufString, ggufStr("0.6B")))
	c, _ := ScanRoots(root)
	m, err := c.Resolve("lmstudio-community/Qwen3-0.6B-GGUF")
	if err != nil {
		t.Fatalf("repo path should resolve: %v", err)
	}
	if m.Key != "qwen/qwen3-0.6b" {
		t.Fatalf("resolved %q", m.Key)
	}

	writeGGUF(t, root, "multi/repo/a.gguf", ggufKV("general.architecture", ggufString, ggufStr("llama")))
	writeGGUF(t, root, "multi/repo/b.gguf", ggufKV("general.architecture", ggufString, ggufStr("llama")))
	c, _ = ScanRoots(root)
	if _, err := c.Resolve("multi/repo"); err == nil {
		t.Fatal("a repo holding several models must be ambiguous, not an arbitrary pick")
	}
}

// A hub entry's model.yaml reaches the catalog model: capabilities the GGUF
// cannot state, the memory floor, and the spec itself for the engine. A
// model.yaml that does not parse costs the recommendations, not the model.
func TestModelYAMLEnrichesCatalogModel(t *testing.T) {
	lm := t.TempDir()
	writeHubEntry(t, lm, "qwen", "qwen3.8-27b", [2]string{"lmstudio-community", "Qwen3.8-27B-GGUF"})
	writeHubEntry(t, lm, "acme", "broken", [2]string{"acme", "Broken-GGUF"})
	yaml, err := os.ReadFile("../yamlite/testdata/qwen3.8-27b.model.yaml")
	if err != nil {
		t.Fatal(err)
	}
	hubDir := filepath.Join(lm, "hub", "models")
	if err := os.WriteFile(filepath.Join(hubDir, "qwen", "qwen3.8-27b", "model.yaml"), yaml, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hubDir, "acme", "broken", "model.yaml"), []byte("a: &x 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	models := t.TempDir()
	writeGGUF(t, models, "lmstudio-community/Qwen3.8-27B-GGUF/w.gguf",
		ggufKV("general.architecture", ggufString, ggufStr("qwen35")))
	writeGGUF(t, models, "acme/Broken-GGUF/w.gguf",
		ggufKV("general.architecture", ggufString, ggufStr("llama")))

	hub := LoadHub(lm)
	c, err := ScanRootsWithHub(hub, models)
	if err != nil {
		t.Fatal(err)
	}
	m, ok := c.Lookup("qwen/qwen3.8-27b")
	if !ok || m.Spec == nil {
		t.Fatalf("model.yaml not attached: %+v", m)
	}
	if m.MinMemoryBytes != 16100000000 {
		t.Errorf("min memory = %d", m.MinMemoryBytes)
	}
	caps := strings.Join(m.Capabilities, ",")
	if !strings.Contains(caps, "tool_use") || !strings.Contains(caps, "reasoning") {
		t.Errorf("capabilities = %v", m.Capabilities)
	}
	if strings.Contains(caps, "vision") {
		t.Error("vision claimed without a projector file")
	}

	b, ok := c.Lookup("acme/broken")
	if !ok {
		t.Fatal("a bad model.yaml lost the model")
	}
	if b.Spec != nil {
		t.Error("a model.yaml that failed to parse was used")
	}
	if e, _ := hub.Lookup("acme/broken"); e.SpecErr == nil {
		t.Error("parse failure not recorded")
	}
}
