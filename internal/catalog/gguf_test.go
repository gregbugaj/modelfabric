package catalog

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

func ggufStr(s string) []byte {
	b := make([]byte, 8+len(s))
	binary.LittleEndian.PutUint64(b, uint64(len(s)))
	copy(b[8:], s)
	return b
}

func ggufKV(key string, vtype uint32, value []byte) []byte {
	var buf bytes.Buffer
	buf.Write(ggufStr(key))
	_ = binary.Write(&buf, binary.LittleEndian, vtype)
	buf.Write(value)
	return buf.Bytes()
}

func u32(v uint32) []byte {
	b := make([]byte, 4)
	binary.LittleEndian.PutUint32(b, v)
	return b
}

func u64(v uint64) []byte {
	b := make([]byte, 8)
	binary.LittleEndian.PutUint64(b, v)
	return b
}

func writeGGUF(t *testing.T, dir, name string, kvs ...[]byte) string {
	t.Helper()
	var buf bytes.Buffer
	buf.Write(u32(ggufMagic))
	buf.Write(u32(3))                // version
	buf.Write(u64(0))                // tensor count
	buf.Write(u64(uint64(len(kvs)))) // metadata count
	for _, kv := range kvs {
		buf.Write(kv)
	}
	buf.Write(make([]byte, 128)) // stand-in for tensor data
	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestReadGGUFMetadata(t *testing.T) {
	dir := t.TempDir()
	path := writeGGUF(t, dir, "model.gguf",
		ggufKV("general.architecture", ggufString, ggufStr("qwen35")),
		ggufKV("general.name", ggufString, ggufStr("Qwen3.8 27B")),
		ggufKV("general.parameter_count", ggufUint64, u64(27_000_000_000)),
		ggufKV("qwen35.context_length", ggufUint32, u32(262144)),
	)
	meta, err := readGGUF(path)
	if err != nil {
		t.Fatalf("readGGUF: %v", err)
	}
	if meta.Architecture != "qwen35" {
		t.Fatalf("architecture = %q, want qwen35", meta.Architecture)
	}
	if meta.ContextLength != 262144 {
		t.Fatalf("context = %d, want 262144", meta.ContextLength)
	}
	if got := humanParams(meta.ParamCount); got != "27B" {
		t.Fatalf("params = %q, want 27B", got)
	}
}

// A large token vocabulary must be skipped without blowing up; this is the
// only unbounded structure in the header.
func TestReadGGUFSkipsLargeArrays(t *testing.T) {
	dir := t.TempDir()
	var tokens bytes.Buffer
	tokens.Write(u32(ggufString))
	tokens.Write(u64(20000))
	for i := 0; i < 20000; i++ {
		tokens.Write(ggufStr("token"))
	}
	var scores bytes.Buffer
	scores.Write(u32(ggufFloat32))
	scores.Write(u64(20000))
	scores.Write(make([]byte, 4*20000))

	path := writeGGUF(t, dir, "model.gguf",
		ggufKV("tokenizer.ggml.tokens", ggufArray, tokens.Bytes()),
		ggufKV("tokenizer.ggml.scores", ggufArray, scores.Bytes()),
		// The key we want sits *after* the big arrays, so it is only reachable
		// if skipping actually works.
		ggufKV("general.architecture", ggufString, ggufStr("llama")),
	)
	meta, err := readGGUF(path)
	if err != nil {
		t.Fatalf("readGGUF: %v", err)
	}
	if meta.Architecture != "llama" {
		t.Fatalf("architecture = %q; the array skip did not reach it", meta.Architecture)
	}
}

func TestPoolingTypeMarksEmbedding(t *testing.T) {
	dir := t.TempDir()
	path := writeGGUF(t, dir, "mystery-name.gguf",
		ggufKV("general.architecture", ggufString, ggufStr("nomic-bert")),
		ggufKV("nomic-bert.pooling_type", ggufUint32, u32(1)),
	)
	meta, err := readGGUF(path)
	if err != nil {
		t.Fatalf("readGGUF: %v", err)
	}
	if !meta.Embedding {
		t.Fatal("pooling_type did not mark the model as an embedding model")
	}
}

func TestReadGGUFRejectsNonGGUF(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "not-a-model.gguf")
	if err := os.WriteFile(path, []byte("this is not a gguf file at all"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readGGUF(path); err == nil {
		t.Fatal("expected a non-GGUF file to be rejected")
	}
}

// A corrupt header must not send the parser into a huge allocation or a spin.
func TestReadGGUFRejectsImplausibleCounts(t *testing.T) {
	var buf bytes.Buffer
	buf.Write(u32(ggufMagic))
	buf.Write(u32(3))
	buf.Write(u64(0))
	buf.Write(u64(1 << 40)) // absurd metadata count
	dir := t.TempDir()
	path := filepath.Join(dir, "corrupt.gguf")
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readGGUF(path); err == nil {
		t.Fatal("expected an implausible metadata count to be rejected")
	}
}

func TestScanUsesHeaderOverFilename(t *testing.T) {
	dir := t.TempDir()
	writeGGUF(t, dir, "publisher/mystery.gguf",
		ggufKV("general.architecture", ggufString, ggufStr("qwen35")),
		ggufKV("general.parameter_count", ggufUint64, u64(4_000_000_000)),
		ggufKV("qwen35.context_length", ggufUint32, u32(32768)),
	)
	c, err := Scan(dir)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	m, ok := c.Lookup("publisher/mystery")
	if !ok {
		t.Fatalf("model not indexed; have %v", c.Models())
	}
	if m.Architecture != "qwen35" {
		t.Fatalf("architecture = %q", m.Architecture)
	}
	if m.ParamsString != "4B" {
		t.Fatalf("params = %q, want 4B", m.ParamsString)
	}
	if m.MaxContextLength != 32768 {
		t.Fatalf("context = %d", m.MaxContextLength)
	}
}

func TestScanPairsProjectorAndSkipsShards(t *testing.T) {
	dir := t.TempDir()
	writeGGUF(t, dir, "org/vision-model.gguf",
		ggufKV("general.architecture", ggufString, ggufStr("qwen35")))
	writeGGUF(t, dir, "org/mmproj-vision-f16.gguf",
		ggufKV("general.architecture", ggufString, ggufStr("clip")))
	writeGGUF(t, dir, "org/big-00001-of-00003.gguf",
		ggufKV("general.architecture", ggufString, ggufStr("llama")))
	writeGGUF(t, dir, "org/big-00002-of-00003.gguf",
		ggufKV("general.architecture", ggufString, ggufStr("llama")))

	c, err := Scan(dir)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	m, ok := c.Lookup("org/vision-model")
	if !ok {
		t.Fatal("vision model not indexed")
	}
	if m.Projector == "" {
		t.Fatal("mmproj sibling was not paired; the model would silently run text-only")
	}
	if _, ok := c.Lookup("org/mmproj-vision-f16"); ok {
		t.Fatal("projector was indexed as a loadable model")
	}
	if _, ok := c.Lookup("org/big-00001-of-00003"); !ok {
		t.Fatal("first shard should be indexed")
	}
	if _, ok := c.Lookup("org/big-00002-of-00003"); ok {
		t.Fatal("later shards must not be indexed separately")
	}
}

func TestResolveAmbiguityIsAnError(t *testing.T) {
	dir := t.TempDir()
	writeGGUF(t, dir, "a/model.gguf", ggufKV("general.architecture", ggufString, ggufStr("llama")))
	writeGGUF(t, dir, "b/model.gguf", ggufKV("general.architecture", ggufString, ggufStr("llama")))
	c, _ := Scan(dir)
	if _, err := c.Resolve("model"); err == nil {
		t.Fatal("an ambiguous short name must not silently pick one")
	}
	if _, err := c.Resolve("a/model"); err != nil {
		t.Fatalf("exact key should resolve: %v", err)
	}
}

// Tools are configured with the identifier the model declares about itself,
// not with wherever the file happens to sit on disk.
func TestHubIDFromMetadata(t *testing.T) {
	cases := []struct {
		basename, size, want string
	}{
		{"Qwen_Qwen3.8", "27B", "qwen/qwen3.8-27b"},
		{"Meta_Llama-3.1", "8B", "meta/llama-3.1-8b"},
		{"nomic-embed-text", "", "nomic-embed-text"}, // no publisher separator
		{"Org_Model_With_Underscores", "4B", "org/model_with_underscores-4b"},
		{"", "27B", ""}, // not enough to be unambiguous
		{"Solo", "", "solo"},
	}
	for _, c := range cases {
		m := &ggufMeta{Basename: c.basename, SizeLabel: c.size}
		if got := m.HubID(); got != c.want {
			t.Errorf("HubID(%q,%q) = %q, want %q", c.basename, c.size, got, c.want)
		}
	}
}

func TestScanPrefersHubIDAndKeepsPathAlias(t *testing.T) {
	dir := t.TempDir()
	writeGGUF(t, dir, "lmstudio-community/Qwen3.8-27B-GGUF/Qwen3.8-27B-Q4_K_M.gguf",
		ggufKV("general.architecture", ggufString, ggufStr("qwen35")),
		ggufKV("general.basename", ggufString, ggufStr("Qwen_Qwen3.8")),
		ggufKV("general.size_label", ggufString, ggufStr("27B")),
	)
	c, err := Scan(dir)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if _, ok := c.Lookup("qwen/qwen3.8-27b"); !ok {
		t.Fatalf("hub id not used as the key; have %v", c.Models())
	}
	if m, err := c.Resolve("lmstudio-community/Qwen3.8-27B-GGUF/Qwen3.8-27B-Q4_K_M"); err != nil {
		t.Fatalf("path key should still resolve: %v", err)
	} else if m.Key != "qwen/qwen3.8-27b" {
		t.Fatalf("path alias resolved to %q", m.Key)
	}
	if m, _ := c.Lookup("qwen/qwen3.8-27b"); m.Publisher != "qwen" {
		t.Fatalf("publisher = %q, want qwen", m.Publisher)
	}
}

// Two quantizations of one model share a hub id. Keep one as the canonical
// entry and leave the other addressable, rather than losing it.
func TestQuantizationVariantsShareAHubID(t *testing.T) {
	dir := t.TempDir()
	for _, q := range []string{"Q4_K_M", "Q8_0"} {
		writeGGUF(t, dir, "repo/Qwen3.8-27B-"+q+".gguf",
			ggufKV("general.basename", ggufString, ggufStr("Qwen_Qwen3.8")),
			ggufKV("general.size_label", ggufString, ggufStr("27B")))
	}
	c, _ := Scan(dir)
	if len(c.Models()) != 1 {
		t.Fatalf("expected one canonical entry, got %v", c.Models())
	}
	m, _ := c.Lookup("qwen/qwen3.8-27b")
	if m.Variants != 1 {
		t.Fatalf("variants = %d, want 1", m.Variants)
	}
	for _, q := range []string{"Q4_K_M", "Q8_0"} {
		if _, err := c.Resolve("repo/Qwen3.8-27B-" + q); err != nil {
			t.Fatalf("variant %s unreachable: %v", q, err)
		}
	}
}

func TestScanRootsPrecedence(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	writeGGUF(t, a, "x/model.gguf", ggufKV("general.architecture", ggufString, ggufStr("llama")))
	writeGGUF(t, b, "x/model.gguf", ggufKV("general.architecture", ggufString, ggufStr("qwen35")))
	c, err := ScanRoots(a, b)
	if err != nil {
		t.Fatalf("ScanRoots: %v", err)
	}
	m, ok := c.Lookup("x/model")
	if !ok {
		t.Fatal("model not indexed")
	}
	if m.Architecture != "llama" {
		t.Fatalf("earlier root should win, got %q", m.Architecture)
	}
	if m.Source != a {
		t.Fatalf("source = %q, want the first root", m.Source)
	}
}

// Qwen3.8 carries its own multi-token-prediction draft head, so speculative
// decoding needs no second model. Detecting it from metadata is what lets the
// runtime enable it automatically instead of hardcoding a model name.
func TestDetectsMTPDraftHead(t *testing.T) {
	dir := t.TempDir()
	writeGGUF(t, dir, "qwen.gguf",
		ggufKV("general.architecture", ggufString, ggufStr("qwen35")),
		ggufKV("general.basename", ggufString, ggufStr("Qwen_Qwen3.8")),
		ggufKV("general.size_label", ggufString, ggufStr("27B")),
		ggufKV("qwen35.nextn_predict_layers", ggufUint32, u32(1)),
	)
	c, err := Scan(dir)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	m, ok := c.Lookup("qwen/qwen3.8-27b")
	if !ok {
		t.Fatalf("model not indexed: %v", c.Models())
	}
	if m.DraftLayers != 1 {
		t.Fatalf("DraftLayers = %d, want 1", m.DraftLayers)
	}
	var speculative bool
	for _, cap := range m.Capabilities {
		if cap == "speculative" {
			speculative = true
		}
	}
	if !speculative {
		t.Fatalf("capabilities = %v, want speculative", m.Capabilities)
	}
}

// A model without a draft head must not advertise speculation, or the runtime
// would pass --spec-type to an engine that cannot honour it.
func TestNoDraftHeadMeansNoSpeculation(t *testing.T) {
	dir := t.TempDir()
	writeGGUF(t, dir, "plain.gguf", ggufKV("general.architecture", ggufString, ggufStr("llama")))
	c, _ := Scan(dir)
	m, _ := c.Lookup("plain")
	if m.DraftLayers != 0 {
		t.Fatalf("DraftLayers = %d, want 0", m.DraftLayers)
	}
	for _, cap := range m.Capabilities {
		if cap == "speculative" {
			t.Fatal("a model with no draft head must not advertise speculation")
		}
	}
}

// A renamed file keeps its header. Quantization and size must come from
// general.file_type and general.size_label when the name says nothing.
func TestQuantAndSizeFromHeaderWhenNameIsUninformative(t *testing.T) {
	dir := t.TempDir()
	writeGGUF(t, dir, "local/my-model.gguf",
		ggufKV("general.architecture", ggufString, ggufStr("qwen3")),
		ggufKV("general.size_label", ggufString, ggufStr("0.6B")),
		ggufKV("general.file_type", ggufUint32, u32(15)),
	)
	c, _ := Scan(dir)
	m, ok := c.Lookup("local/my-model")
	if !ok {
		t.Fatalf("not indexed: %v", c.Models())
	}
	if m.Quantization != "Q4_K_M" || m.ParamsString != "0.6B" {
		t.Fatalf("quant %q params %q; want Q4_K_M and 0.6B from the header", m.Quantization, m.ParamsString)
	}
}
