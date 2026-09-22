package runtime

import (
	"os"
	"path/filepath"
	"testing"
)

// A package manifest names its own entrypoint and vendor directories. Joining
// those names without confining them let "../.." reach a binary the package
// does not own — and that binary is what gets launched.
func TestManifestPathsCannotEscapeTheirPackage(t *testing.T) {
	root := "/models/.lmstudio/extensions/backends/llama.cpp-cuda"
	for _, ok := range []string{"llama-server", "bin/llama-server", "./bin/llama-server"} {
		if _, err := confine(root, ok); err != nil {
			t.Errorf("%q should be allowed: %v", ok, err)
		}
	}
	for _, bad := range []string{"../../../bin/sh", "..", "/bin/sh", "bin/../../../../etc/passwd"} {
		if p, err := confine(root, bad); err == nil {
			t.Errorf("%q escaped to %q", bad, p)
		}
	}
}

// An LM Studio manifest states its minimum driver either as a bare code
// ("12040") or as a dotted version ("12.4"). The dotted form used to become 0,
// which reads as "no requirement" — so a build was offered on a driver that
// may be too old for it.
func TestMinDriverAcceptsBothManifestForms(t *testing.T) {
	for in, want := range map[string]int{
		"12040": 12040,
		"12.4":  12040,
		"11.8":  11080,
		"":      0,
		"weird": 0,
	} {
		if got := minDriverCode(in); got != want {
			t.Errorf("minDriverCode(%q) = %d, want %d", in, got, want)
		}
	}
}

// A package installed before the rename to ModelFabric records its
// provenance under "llmz". It is still ours: its build number and the devices
// probed at install time have to survive the rename.
func TestProvenanceIsReadUnderEitherKey(t *testing.T) {
	for _, key := range []string{"modelfabric", "llmz"} {
		t.Run(key, func(t *testing.T) {
			pkg := t.TempDir()
			if err := os.WriteFile(filepath.Join(pkg, "llama-server"), []byte("#!/bin/sh\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			manifest := `{"name":"llama.cpp-linux-x86_64-cpu","version":"b7000","extension_type":"engine","engine":"llama.cpp",
				"supported_model_formats":["gguf"],
				"engine_protocol_server":{"runtime_kind":"llama-server","executable_relative_path":"llama-server"},
				"` + key + `":{"source":"https://example.com/a.zip","build":"b7000"}}`
			if err := os.WriteFile(filepath.Join(pkg, "backend-manifest.json"), []byte(manifest), 0o644); err != nil {
				t.Fatal(err)
			}
			d, err := definitionFromPackage(pkg, "", OriginUpstream)
			if err != nil || d == nil {
				t.Fatalf("definition: %v %v", d, err)
			}
			if d.provenance == nil || d.provenance.Build != "b7000" {
				t.Errorf("provenance under %q was lost: %+v", key, d.provenance)
			}
		})
	}
}
