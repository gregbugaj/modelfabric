package yamlite

import (
	"os"
	"reflect"
	"strings"
	"testing"
)

func mustParse(t *testing.T, src string) any {
	t.Helper()
	v, err := Parse([]byte(src))
	if err != nil {
		t.Fatalf("parse: %v\n%s", err, src)
	}
	return v
}

// The file LM Studio actually writes, parsed end to end.
func TestParsesLMStudioModelYAML(t *testing.T) {
	data, err := os.ReadFile("testdata/qwen3.8-27b.model.yaml")
	if err != nil {
		t.Fatal(err)
	}
	v, err := Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	doc := v.(map[string]any)
	if doc["model"] != "qwen/qwen3.8-27b" {
		t.Fatalf("model = %v", doc["model"])
	}
	base := doc["base"].([]any)
	first := base[0].(map[string]any)
	src := first["sources"].([]any)[0].(map[string]any)
	if first["key"] != "lmstudio-community/qwen3.8-27b-gguf" || src["repo"] != "Qwen3.8-27B-GGUF" || len(base) != 5 {
		t.Fatalf("base = %#v", base)
	}
	meta := doc["metadataOverrides"].(map[string]any)
	if meta["vision"] != true || meta["minMemoryUsageBytes"] != int64(16100000000) {
		t.Fatalf("metadataOverrides = %#v", meta)
	}
	if !reflect.DeepEqual(meta["contextLengths"], []any{int64(262144)}) {
		t.Fatalf("contextLengths = %#v", meta["contextLengths"])
	}
	fields := doc["config"].(map[string]any)["operation"].(map[string]any)["fields"].([]any)
	topP := fields[2].(map[string]any)
	if topP["key"] != "llm.prediction.topPSampling" {
		t.Fatalf("fields[2] = %#v", topP)
	}
	if want := map[string]any{"checked": true, "value": 0.95}; !reflect.DeepEqual(topP["value"], want) {
		t.Fatalf("topP value = %#v", topP["value"])
	}
	if fields[0].(map[string]any)["value"] != 1.0 {
		t.Fatalf("temperature = %#v", fields[0])
	}
	custom := doc["customFields"].([]any)[0].(map[string]any)
	eff := custom["effects"].([]any)[0].(map[string]any)
	if custom["defaultValue"] != "xhigh" || eff["variable"] != "reasoning_effort" || len(custom["options"].([]any)) != 3 {
		t.Fatalf("customFields[0] = %#v", custom)
	}
}

func TestScalarsAndQuoting(t *testing.T) {
	got := mustParse(t, `
a: 1
b: -2.5
c: true
d: ~
e: "x: # y"
f: 'it''s'
g: plain text # a comment
h: it's fine
i: [1, "two", three]
j: []
k: 1.0
"quoted key": v
`).(map[string]any)
	want := map[string]any{
		"a": int64(1), "b": -2.5, "c": true, "d": nil, "e": "x: # y", "f": "it's",
		"g": "plain text", "h": "it's fine", "i": []any{int64(1), "two", "three"},
		"j": []any{}, "k": 1.0, "quoted key": "v",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got  %#v\nwant %#v", got, want)
	}
}

func TestSequencesAtKeyIndentAndNested(t *testing.T) {
	got := mustParse(t, `
list:
- one
- - inner
  - inner2
- k: v
  k2: v2
empty:
after: x
`).(map[string]any)
	want := map[string]any{
		"list":  []any{"one", []any{"inner", "inner2"}, map[string]any{"k": "v", "k2": "v2"}},
		"empty": nil,
		"after": "x",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got  %#v\nwant %#v", got, want)
	}
}

func TestBlockScalars(t *testing.T) {
	got := mustParse(t, "lit: |\n  line one\n  # not a comment\n\n  line three\nfold: >-\n  a\n  b\n\n  c\nnext: 1\n").(map[string]any)
	if got["lit"] != "line one\n# not a comment\n\nline three\n" {
		t.Fatalf("literal = %q", got["lit"])
	}
	if got["fold"] != "a b\nc" {
		t.Fatalf("folded = %q", got["fold"])
	}
	if got["next"] != int64(1) {
		t.Fatalf("next = %#v", got["next"])
	}
}

func TestRejectsUnsupportedSyntax(t *testing.T) {
	for _, src := range []string{
		"a: &anchor 1\nb: *anchor\n",
		"a: !!str 1\n",
		"a: {b: 1}\n",
		"a: 1\n---\nb: 2\n",
		"a:\n\tb: 1\n",
		"a: 1\na: 2\n",
		"a: 1\n    b: 2\n",
		"? complex\n: key\n",
	} {
		if _, err := Parse([]byte(src)); err == nil {
			t.Errorf("accepted unsupported input:\n%s", src)
		}
	}
}

func TestEmptyDocument(t *testing.T) {
	if v := mustParse(t, "# only a comment\n\n"); v != nil {
		t.Fatalf("got %#v", v)
	}
	if v := mustParse(t, strings.Repeat("\n", 3)); v != nil {
		t.Fatalf("got %#v", v)
	}
}
