package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestJITPolicy(t *testing.T) {
	off := false
	cases := []struct {
		ttl   string
		evict *bool
		want  time.Duration
		evOK  bool
		err   bool
	}{
		{"", nil, 60 * time.Minute, true, false}, // LM Studio's default
		{"0", nil, 0, true, false},
		{"never", &off, 0, false, false},
		{"15m", nil, 15 * time.Minute, true, false},
		{"-5m", nil, 0, true, true},
		{"soon", nil, 0, true, true},
	}
	for _, c := range cases {
		ttl, evict, err := Config{JITTTL: c.ttl, JITAutoEvict: c.evict}.JITPolicy()
		if (err != nil) != c.err {
			t.Errorf("%q: err = %v", c.ttl, err)
			continue
		}
		if !c.err && (ttl != c.want || evict != c.evOK) {
			t.Errorf("%q: got %v/%v, want %v/%v", c.ttl, ttl, evict, c.want, c.evOK)
		}
	}
}

// Empty or whitespace-only config files use defaults instead of returning a parse error.
func TestEmptyConfigFileIsNotAParseError(t *testing.T) {
	for _, body := range []string{"", "   ", "\n\t\n"} {
		dir := t.TempDir()
		p := filepath.Join(dir, "config.json")
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		c, err := Load(p)
		if err != nil {
			t.Errorf("Load(%q) = %v; want the defaults", body, err)
			continue
		}
		if c.Listen != "127.0.0.1:1234" {
			t.Errorf("defaults not applied for %q: listen=%q", body, c.Listen)
		}
	}
	dir := t.TempDir()
	p := filepath.Join(dir, "config.json")
	if err := os.WriteFile(p, []byte("{oops"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(p); err == nil {
		t.Error("malformed JSON was accepted")
	}
}

func TestValidateCORSOrigins(t *testing.T) {
	tests := []struct {
		name    string
		origins []string
		wantErr string
	}{
		{name: "empty is off", origins: nil},
		{name: "any origin", origins: []string{"*"}},
		{name: "bare origins", origins: []string{"http://localhost:3000", "https://chat.example.com"}},
		{name: "a path would match nothing, so it is refused", origins: []string{"http://localhost:3000/app"}, wantErr: "not an origin"},
		{name: "a trailing slash is named, not trimmed", origins: []string{"http://localhost:3000/"}, wantErr: "trailing slash"},
		{name: "no scheme", origins: []string{"localhost:3000"}, wantErr: "not an origin"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateCORSOrigins(tc.origins)
			if tc.wantErr == "" && err != nil || tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)) {
				t.Fatalf("err = %v, want %q", err, tc.wantErr)
			}
		})
	}
}

// Saving from the dashboard rewrites the file a person keeps by hand, so it
// must change only what was asked and refuse anything Load would.
func TestUpdate(t *testing.T) {
	const orig = `{"listen":"127.0.0.1:1234","engines":[{"name":"gpu0","base_url":"http://127.0.0.1:8000"}],"from_a_newer_build":{"x":1}}`
	tests := []struct {
		name    string
		set     map[string]any
		wantErr string
		check   func(t *testing.T, c Config, file string)
	}{
		{name: "changes the key and keeps every other, even unknown ones",
			set: map[string]any{"listen": "127.0.0.1:3000"},
			check: func(t *testing.T, c Config, file string) {
				if c.Listen != "127.0.0.1:3000" || len(c.Engines) != 1 {
					t.Fatalf("config = %+v", c)
				}
				if !strings.Contains(file, `"from_a_newer_build"`) {
					t.Fatalf("an unknown key was dropped:\n%s", file)
				}
			}},
		{name: "nil removes a key, so the default applies",
			set: map[string]any{"listen": nil},
			check: func(t *testing.T, c Config, file string) {
				if c.Listen != "127.0.0.1:1234" || strings.Contains(file, `"listen"`) {
					t.Fatalf("listen = %q, file:\n%s", c.Listen, file)
				}
			}},
		{name: "a value Load would refuse is not written",
			set: map[string]any{"cors_origins": []string{"localhost:3000"}}, wantErr: "not an origin"},
		{name: "a bad jit_ttl is not written",
			set: map[string]any{"jit_ttl": "soon"}, wantErr: "jit_ttl"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "config.json")
			if err := os.WriteFile(p, []byte(orig), 0o600); err != nil {
				t.Fatal(err)
			}
			c, err := Update(p, tc.set)
			after, _ := os.ReadFile(p)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want %q", err, tc.wantErr)
				}
				if string(after) != orig {
					t.Fatalf("a refused update changed the file:\n%s", after)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if bak, _ := os.ReadFile(p + ".bak"); string(bak) != orig {
				t.Fatalf(".bak = %q, want the previous file", bak)
			}
			if fi, _ := os.Stat(p); fi.Mode().Perm() != 0o600 {
				t.Fatalf("mode = %o, want the original 600", fi.Mode().Perm())
			}
			tc.check(t, c, string(after))
		})
	}
}

func TestUpdateRefusesABrokenFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.json")
	os.WriteFile(p, []byte("{broken"), 0o644)
	if _, err := Update(p, map[string]any{"listen": "127.0.0.1:3000"}); err == nil || !strings.Contains(err.Error(), "fix it by hand") {
		t.Fatalf("err = %v", err)
	}
}

// "Serve on Network" binds every interface; on a port the node already holds
// it failed only at the next restart, long after Save said it worked.
func TestPublicListenPortClash(t *testing.T) {
	tests := []struct {
		name, file, wantErr string
	}{
		{name: "a free port is fine", file: `{"listen":"127.0.0.1:1234","public_listen":"0.0.0.0:1235"}`},
		{name: "the front door's port is refused", file: `{"listen":"127.0.0.1:1234","public_listen":"0.0.0.0:1234"}`, wantErr: "listen already has"},
		{name: "the mesh port is refused", file: `{"listen":"127.0.0.1:3000","public_listen":"0.0.0.0:1234"}`, wantErr: "mesh port"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "config.json")
			os.WriteFile(p, []byte(tc.file), 0o644)
			_, err := Load(p)
			if tc.wantErr == "" && err != nil || tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)) {
				t.Fatalf("err = %v, want %q", err, tc.wantErr)
			}
		})
	}
}

// "tailnet" means the address Tailscale gave this node, which is known only
// when the node starts; typed in, it could not be shared between nodes.
func TestResolveEngineBind(t *testing.T) {
	tests := []struct {
		name, bind, self, want string
		warns                  bool
	}{
		{name: "unset stays loopback", bind: "", self: "100.64.0.7", want: ""},
		{name: "tailnet is this node's tailnet address", bind: "tailnet", self: "100.64.0.7", want: "100.64.0.7"},
		{name: "tailnet without Tailscale falls back to loopback, and says so", bind: "tailnet", self: "", want: "", warns: true},
		// The setting used to be written as the node's tailnet IP. Tailscale
		// can change it; a literal tailnet address follows Tailscale's.
		{name: "this node's tailnet address, written out, is the tailnet", bind: "100.64.0.7", self: "100.64.0.7", want: "100.64.0.7"},
		{name: "a stale tailnet address follows Tailscale's current one, and says so", bind: "100.64.0.9", self: "100.64.0.7", want: "100.64.0.7", warns: true},
		{name: "a LAN address is not the tailnet and is kept", bind: "192.168.1.10", self: "100.64.0.7", want: "192.168.1.10"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, warn := Config{EngineBind: tc.bind}.ResolveEngineBind(tc.self)
			if got != tc.want || (warn != "") != tc.warns {
				t.Fatalf("got %q, warning %q", got, warn)
			}
		})
	}
}

// doctor called any non-loopback engine_bind "reachable over the tailnet
// only". A LAN address there exposes unauthenticated engines to the LAN, so
// what counts as a tailnet address has to be exact at both ends of the range.
func TestIsTailnetAddr(t *testing.T) {
	for _, in := range []string{"100.64.0.1", "100.64.0.9", "100.127.255.254", "fd7a:115c:a1e0::1"} {
		if !IsTailnetAddr(in) {
			t.Errorf("%s is a tailnet address", in)
		}
	}
	for _, in := range []string{"192.168.1.10", "10.0.0.5", "100.63.255.255", "100.128.0.1", "fd00::1", "example.com", ""} {
		if IsTailnetAddr(in) {
			t.Errorf("%s is not a tailnet address", in)
		}
	}
}
