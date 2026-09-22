package discovery

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRenderMatchesLLMDSchema(t *testing.T) {
	out, err := Render([]Endpoint{
		{Name: "inst-b", Address: "100.91.190.79", Port: 18200,
			Labels: map[string]string{"modelfabric.sh/node": "predator"}},
		{Name: "inst-a", Address: "100.107.225.6", Port: 18201},
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	got := string(out)

	// Required shape: port is a *string*, namespace defaults to "default".
	for _, want := range []string{
		"endpoints:\n",
		"  - name: inst-a\n",
		"    namespace: default\n",
		`    address: "100.107.225.6"`,
		`    port: "18201"`,
		"modelfabric.sh/node: predator",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("output missing %q:\n%s", want, got)
		}
	}
	// Sorted by name, so the file is stable across writes.
	if strings.Index(got, "inst-a") > strings.Index(got, "inst-b") {
		t.Fatalf("endpoints are not sorted by name:\n%s", got)
	}
}

// The file-discovery plugin is IPv4-only, and a tailnet advertises both
// families — so an IPv6 address must be rejected rather than written out.
func TestRenderRejectsIPv6(t *testing.T) {
	_, err := Render([]Endpoint{{Name: "a", Address: "fd7a:115c:a1e0::e339:e108", Port: 8000}})
	if err == nil || !strings.Contains(err.Error(), "IPv4 only") {
		t.Fatalf("expected an IPv6 rejection, got %v", err)
	}
}

func TestRenderRejectsInvalidEndpoints(t *testing.T) {
	for _, bad := range []Endpoint{
		{Name: "", Address: "10.0.0.1", Port: 8000},
		{Name: "a", Address: "not-an-ip", Port: 8000},
		{Name: "a", Address: "10.0.0.1", Port: 0},
		{Name: "a", Address: "10.0.0.1", Port: 70000},
	} {
		if _, err := Render([]Endpoint{bad}); err == nil {
			t.Fatalf("expected %+v to be rejected", bad)
		}
	}
}

func TestEmptyListIsValid(t *testing.T) {
	out, err := Render(nil)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !strings.Contains(string(out), "endpoints:\n  []") {
		t.Fatalf("an empty mesh should render an empty list, got:\n%s", out)
	}
}

// Loopback endpoints are useless to an EPP on another host; the caller needs to
// be able to detect that rather than publish undialable addresses silently.
func TestUnreachableDetectsLoopback(t *testing.T) {
	if !(Endpoint{Address: "127.0.0.1"}).Unreachable() {
		t.Fatal("127.0.0.1 should be reported unreachable")
	}
	if (Endpoint{Address: "100.107.225.6"}).Unreachable() {
		t.Fatal("a tailnet address should not be reported unreachable")
	}
}

// The EPP watches the file with fsnotify, so rewriting identical content would
// wake its reconciler for nothing.
func TestWriteFileSkipsUnchangedContent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "endpoints.yaml")
	eps := []Endpoint{{Name: "a", Address: "10.0.0.1", Port: 8000}}

	changed, err := WriteFile(path, eps)
	if err != nil || !changed {
		t.Fatalf("first write: changed=%v err=%v", changed, err)
	}
	changed, err = WriteFile(path, eps)
	if err != nil {
		t.Fatalf("second write: %v", err)
	}
	if changed {
		t.Fatal("rewriting identical content must not report a change")
	}

	eps = append(eps, Endpoint{Name: "b", Address: "10.0.0.2", Port: 8000})
	if changed, _ := WriteFile(path, eps); !changed {
		t.Fatal("a new endpoint must be reported as a change")
	}
	// No temp file left behind by the atomic rename.
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Fatal("temp file was not cleaned up by the rename")
	}
}

func TestYAMLScalarQuoting(t *testing.T) {
	cases := map[string]string{
		"inst-8646f3de4c80":           "inst-8646f3de4c80",           // '-' inside a word is fine
		"ggml-org/Qwen3.8-27B-GGUF/x": "ggml-org/Qwen3.8-27B-GGUF/x", // slashes and dots are fine
		"llama.cpp":                   "llama.cpp",
		"-leading-dash":               `"-leading-dash"`, // an indicator in first position
		"true":                        `"true"`,          // would otherwise become a bool
		"123":                         `"123"`,           // would otherwise become a number
		"":                            `""`,
		"has: colon":                  `"has: colon"`,
	}
	for in, want := range cases {
		if got := yamlScalar(in); got != want {
			t.Errorf("yamlScalar(%q) = %s, want %s", in, got, want)
		}
	}
}
