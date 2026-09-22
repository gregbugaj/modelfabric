package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gregbugaj/modelfabric/internal/config"
)

func settingsServer(t *testing.T, file string) (*frontFixture, string) {
	t.Helper()
	f := newFrontFixture(t, false, false)
	p := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(p, []byte(file), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := config.Load(p)
	if err != nil {
		t.Fatal(err)
	}
	f.srv.SetConfig(p, c)
	f.srv.SetCORS(c.CORSOrigins)
	return f, p
}

func putSettings(h http.Handler, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "/api/v1/server-settings", strings.NewReader(body)))
	return rec
}

func TestServerSettings(t *testing.T) {
	const file = `{"listen":"127.0.0.1:1234","from_a_newer_build":true}`
	tests := []struct {
		name  string
		body  string
		want  int
		check func(t *testing.T, f *frontFixture, v settingsView, onDisk string)
	}{
		{name: "require_api_key applies at once, without a restart", body: `{"require_api_key":true}`, want: 200,
			check: func(t *testing.T, f *frontFixture, v settingsView, _ string) {
				if !v.Running.RequireAPIKey || !v.Saved.RequireAPIKey {
					t.Fatalf("running %v, saved %v", v.Running.RequireAPIKey, v.Saved.RequireAPIKey)
				}
				if rec := call(f.srv.FrontHandler(), "", ""); rec.Code != 401 {
					t.Fatalf("a request without a key answered %d after require_api_key was saved", rec.Code)
				}
			}},
		{name: "cors_origins applies at once", body: `{"cors_origins":["http://localhost:3000"]}`, want: 200,
			check: func(t *testing.T, f *frontFixture, _ settingsView, _ string) {
				if !f.srv.corsAllows("http://localhost:3000") {
					t.Fatal("the saved origin is not allowed by the running node")
				}
			}},
		{name: "listen is saved, and reported as waiting for a restart", body: `{"listen":"127.0.0.1:3000"}`, want: 200,
			check: func(t *testing.T, _ *frontFixture, v settingsView, onDisk string) {
				if v.Saved.Listen != "127.0.0.1:3000" || v.Running.Listen != "127.0.0.1:1234" {
					t.Fatalf("saved %q, running %q", v.Saved.Listen, v.Running.Listen)
				}
				if !strings.Contains(onDisk, "from_a_newer_build") {
					t.Fatalf("an unknown key was dropped:\n%s", onDisk)
				}
			}},
		{name: "a cleared field is removed from the file, not saved empty", body: `{"public_listen":""}`, want: 200,
			check: func(t *testing.T, _ *frontFixture, _ settingsView, onDisk string) {
				if strings.Contains(onDisk, "public_listen") {
					t.Fatalf("public_listen left in the file:\n%s", onDisk)
				}
			}},
		{name: "engine_bind takes the tailnet by name", body: `{"engine_bind":"tailnet"}`, want: 200,
			check: func(t *testing.T, _ *frontFixture, v settingsView, _ string) {
				if v.Saved.EngineBind != "tailnet" {
					t.Fatalf("saved %q", v.Saved.EngineBind)
				}
			}},
		{name: "engine_bind is never a typed address: Tailscale assigns it", body: `{"engine_bind":"100.64.0.7"}`, want: 400},
		{name: "a router setting is saved and waits for a restart", body: `{"rate_weighted_routing":false,"prefix_affinity":false}`, want: 200,
			check: func(t *testing.T, _ *frontFixture, v settingsView, onDisk string) {
				if v.Saved.RateWeightedRouting || v.Saved.PrefixAffinity {
					t.Fatalf("saved %+v", v.Saved)
				}
				if !v.Running.RateWeightedRouting || !v.Running.PrefixAffinity {
					t.Fatal("the running node reports a router setting it reads only at startup as already changed")
				}
				if !strings.Contains(onDisk, `"prefix_affinity": false`) {
					t.Fatalf("file:\n%s", onDisk)
				}
			}},
		// 0 is "no ceiling"; an absent key is the 16384 default. Removing a 0
		// would quietly turn the choice of no ceiling back into a ceiling.
		{name: "an output ceiling of 0 is written, not removed", body: `{"max_output_tokens":0}`, want: 200,
			check: func(t *testing.T, _ *frontFixture, v settingsView, onDisk string) {
				if v.Saved.MaxOutputTokens != 0 || !strings.Contains(onDisk, `"max_output_tokens": 0`) {
					t.Fatalf("saved %d, file:\n%s", v.Saved.MaxOutputTokens, onDisk)
				}
			}},
		{name: "the disk cache is turned on with a size", body: `{"cache_disk_mib":51200,"cache_disk_dir":"/srv/slots"}`, want: 200,
			check: func(t *testing.T, _ *frontFixture, v settingsView, _ string) {
				if v.Saved.CacheDiskMiB != 51200 || v.Saved.CacheDiskDir != "/srv/slots" {
					t.Fatalf("saved %+v", v.Saved)
				}
			}},
		// The default bias is 0.5: a removed 0 would bring it back.
		{name: "a local bias of 0 is written, not removed", body: `{"local_bias":0}`, want: 200,
			check: func(t *testing.T, _ *frontFixture, v settingsView, onDisk string) {
				if v.Saved.LocalBias != 0 || !strings.Contains(onDisk, `"local_bias": 0`) {
					t.Fatalf("saved %v, file:\n%s", v.Saved.LocalBias, onDisk)
				}
			}},
		{name: "a relative cache directory is refused", body: `{"cache_disk_dir":"slots"}`, want: 400},
		{name: "a negative local bias is refused", body: `{"local_bias":-1}`, want: 400},
		{name: "mesh_port is not one node's to change", body: `{"mesh_port":1300}`, want: 400},
		{name: "an unknown setting is refused by name", body: `{"listne":"x"}`, want: 400},
		{name: "a bad address is refused", body: `{"listen":"localhost"}`, want: 400},
		{name: "a bad origin is refused", body: `{"cors_origins":["localhost:3000"]}`, want: 400},
		{name: "a bad jit_ttl is refused", body: `{"jit_ttl":"soon"}`, want: 400},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f, p := settingsServer(t, file)
			rec := putSettings(f.srv.Handler(), tc.body)
			onDisk, _ := os.ReadFile(p)
			if rec.Code != tc.want {
				t.Fatalf("got %d %s, want %d", rec.Code, rec.Body, tc.want)
			}
			if tc.want != 200 {
				if string(onDisk) != file {
					t.Fatalf("a refused change was written:\n%s", onDisk)
				}
				return
			}
			var v settingsView
			if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
				t.Fatal(err)
			}
			tc.check(t, f, v, string(onDisk))
		})
	}
}

// A refused save must not have touched the running node either: it would be
// running a value the file does not hold, and lose it on restart.
func TestRefusedSaveAppliesNothing(t *testing.T) {
	f, _ := settingsServer(t, `{}`)
	rec := putSettings(f.srv.Handler(), `{"require_api_key":true,"listen":"nope"}`)
	if rec.Code != 400 {
		t.Fatalf("got %d", rec.Code)
	}
	if f.srv.requireKey.Load() {
		t.Fatal("require_api_key was applied although the save was refused")
	}
}

// engine_bind used to be written as the node's tailnet IP; the page showed it
// as an address of its own, "(typed)", beside the Tailnet choice it means.
func TestTailnetAddressWrittenOutReadsAsTailnet(t *testing.T) {
	f, _ := settingsServer(t, `{"engine_bind":"100.64.0.7"}`)
	rec := httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/server-settings", nil))
	var v settingsView
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	if v.Saved.EngineBind != "tailnet" || v.Running.EngineBind != "tailnet" {
		t.Fatalf("saved %q, running %q; want both tailnet", v.Saved.EngineBind, v.Running.EngineBind)
	}
}

func TestServerSettingsWithoutAFile(t *testing.T) {
	f := newFrontFixture(t, false, false)
	rec := httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/server-settings", nil))
	if rec.Code != 404 || !strings.Contains(rec.Body.String(), "-config") {
		t.Fatalf("got %d %s", rec.Code, rec.Body)
	}
}
