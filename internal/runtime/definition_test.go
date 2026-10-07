package runtime

import (
	"os"
	"path/filepath"
	"testing"
)

// Only a zero counted as "unset", so a negative context length or parallel
// count was defaulted past and rendered straight into the engine's flags.
func TestResolveRejectsNegativeParameters(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "llama-server")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name string
		mut  func(*Definition)
	}{
		{"context_length", func(d *Definition) { d.ContextLength = -1 }},
		{"parallel", func(d *Definition) { d.Parallel = -4 }},
		{"gpu_layers", func(d *Definition) { d.GPULayers = -2 }},
	} {
		d := &Definition{Name: "test", Entrypoint: bin}
		c.mut(d)
		if err := d.Resolve(); err == nil {
			t.Errorf("a negative %s was accepted", c.name)
		}
	}
	d := &Definition{Name: "test", Entrypoint: bin}
	if err := d.Resolve(); err != nil {
		t.Fatal(err)
	}
	if d.ContextLength != 8192 || d.Parallel != 4 {
		t.Errorf("defaults not applied: ctx=%d parallel=%d", d.ContextLength, d.Parallel)
	}
}

// A load's extra_args are screened for flags ModelFabric manages; a runtime
// definition's own Args land in the same override position and were not, so a
// definition could re-expose the engine on every interface or point it at a
// different model.
func TestDefinitionArgsCannotOverrideManagedFlags(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "llama-server")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"--host", "0.0.0.0"},
		{"--model=/etc/passwd"},
		{"--threads", "8", "--port", "9999"},
	} {
		d := &Definition{Name: "test", Entrypoint: bin, Args: args}
		if err := d.Resolve(); err == nil {
			t.Errorf("definition args %v were accepted", args)
		}
	}
	d := &Definition{Name: "test", Entrypoint: bin, Args: []string{"--threads", "8", "--flash-attn"}}
	if err := d.Resolve(); err != nil {
		t.Errorf("ordinary args were refused: %v", err)
	}
}

// context_length * parallel is computed in 64 bits: validation sets minimums,
// not maximums, so the product could wrap negative and be passed as -c.
func TestKVCacheTokensCannotWrap(t *testing.T) {
	a := Applied{ContextLength: 1 << 40, Parallel: 1 << 20}
	got := llamaCPP{}.applyKV(a)
	if got <= 0 {
		t.Fatalf("KV cache tokens wrapped to %d", got)
	}
}
