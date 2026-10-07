package main

import (
	"strings"
	"testing"
)

func TestParseTTL(t *testing.T) {
	for in, want := range map[string]int{"": 0, "3600": 3600, "30m": 1800, "1h30m": 5400, " 90s ": 90} {
		if got, err := parseTTL(in); err != nil || got != want {
			t.Errorf("parseTTL(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, bad := range []string{"soon", "-5", "500ms"} {
		if _, err := parseTTL(bad); err == nil {
			t.Errorf("parseTTL(%q) accepted", bad)
		}
	}
}

// Resolve positional model keys to instance IDs before unloading; sending a key as instance_id rejected loaded models.
func TestUnloadTargetsResolvesAModelKey(t *testing.T) {
	models := []apiModel{
		{Key: "qwen/qwen3-0.6b", LoadedInstances: loadedIDs("inst-aaa", "inst-bbb")},
		{Key: "qwen/qwen3.8-27b", LoadedInstances: loadedIDs("inst-ccc")},
		{Key: "unloaded/model"},
	}
	got, err := unloadTargets(models, "qwen/qwen3-0.6b")
	if err != nil || len(got) != 2 || got[0] != "inst-aaa" || got[1] != "inst-bbb" {
		t.Fatalf("unloadTargets(key) = %v, %v; want both instances", got, err)
	}
	if got, err := unloadTargets(models, "inst-bbb"); err != nil || len(got) != 1 || got[0] != "inst-bbb" {
		t.Fatalf("unloadTargets(id) = %v, %v; want just that instance", got, err)
	}
	_, err = unloadTargets(models, "qwen/nope")
	if err == nil {
		t.Fatal("an unknown argument must be an error, not a silent no-op")
	}
	for _, want := range []string{`"qwen/nope"`, "qwen/qwen3-0.6b", "qwen/qwen3.8-27b"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q should name %s", err, want)
		}
	}
	if strings.Count(err.Error(), "qwen/qwen3-0.6b") != 1 {
		t.Errorf("a model with replicas should be listed once: %q", err)
	}
	if _, err := unloadTargets(models, "unloaded/model"); err == nil {
		t.Error("a model with no instances cannot be unloaded")
	}
	if _, err := unloadTargets(nil, "anything"); err == nil {
		t.Error("with nothing loaded the message should say so")
	}
}

func loadedIDs(ids ...string) []apiInstance {
	var out []apiInstance
	for _, id := range ids {
		out = append(out, apiInstance{ID: id})
	}
	return out
}

// The unit file interpolated paths straight into ExecStart. systemd has its own
// quoting rules and expands "%" specifiers, so a space, a quote, a backslash or
// a percent sign in the binary or config path changed what got run.
func TestSystemdArgQuotesAwkwardPaths(t *testing.T) {
	cases := map[string]string{
		`/usr/bin/mfsh`:    `"/usr/bin/mfsh"`,
		`/home/a b/mfsh`:   `"/home/a b/mfsh"`,
		`/tmp/100%/mfsh`:   `"/tmp/100%%/mfsh"`,
		`/tmp/q"uote/mfsh`: `"/tmp/q\"uote/mfsh"`,
		`C:\mfsh\mfsh.exe`: `"C:\\mfsh\\mfsh.exe"`,
	}
	for in, want := range cases {
		if got := systemdArg(in); got != want {
			t.Errorf("systemdArg(%q) = %s, want %s", in, got, want)
		}
	}
}

// Probe all wildcard forms through loopback; dialing 0.0.0.0 or [::] fails on some systems.
func TestHTTPBaseTranslatesEveryWildcard(t *testing.T) {
	for in, want := range map[string]string{
		":1234":          "http://127.0.0.1:1234",
		"0.0.0.0:1234":   "http://127.0.0.1:1234",
		"[::]:1234":      "http://127.0.0.1:1234",
		"127.0.0.1:1234": "http://127.0.0.1:1234",
		"minion:1234":    "http://minion:1234",
		"http://x:1/v1":  "http://x:1/v1",
	} {
		if got := httpBase(in); got != want {
			t.Errorf("httpBase(%q) = %q, want %q", in, got, want)
		}
	}
}
