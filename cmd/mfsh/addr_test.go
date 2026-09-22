package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/gregbugaj/modelfabric/internal/osproc"
)

func TestWithPort(t *testing.T) {
	tests := []struct {
		name    string
		listen  string
		port    int
		want    string
		wantErr string
	}{
		{name: "keeps a loopback bind loopback", listen: "127.0.0.1:1234", port: 3000, want: "127.0.0.1:3000"},
		{name: "keeps a wildcard bind wildcard", listen: "0.0.0.0:1234", port: 3000, want: "0.0.0.0:3000"},
		{name: "keeps an IPv6 bind", listen: "[::1]:1234", port: 3000, want: "[::1]:3000"},
		{name: "no listen configured binds loopback, never wider", listen: "", port: 3000, want: "127.0.0.1:3000"},
		{name: "port zero is refused", listen: "127.0.0.1:1234", port: 0, wantErr: "1-65535"},
		{name: "port above range is refused", listen: "127.0.0.1:1234", port: 70000, wantErr: "1-65535"},
		{name: "unparseable listen is named", listen: "localhost", port: 3000, wantErr: `"localhost"`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := withPort(tc.listen, tc.port)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want one mentioning %s", err, tc.wantErr)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("withPort(%q, %d) = %q, %v; want %q", tc.listen, tc.port, got, err, tc.want)
			}
		})
	}
}

// The CLI used to dial 127.0.0.1:1234 whatever the node listened on, so a
// node started with -port 3000 read as "not running" to every other command.
func TestPickAddrFollowsTheRunningNode(t *testing.T) {
	tests := []struct {
		name      string
		env       string
		running   string
		cfgListen string
		want      string
	}{
		{name: "MFSH_ADDR wins over everything", env: "http://peer:1234", running: "127.0.0.1:3000", cfgListen: "127.0.0.1:4000", want: "http://peer:1234"},
		{name: "a running node's address beats the config", running: "127.0.0.1:3000", cfgListen: "127.0.0.1:4000", want: "http://127.0.0.1:3000"},
		{name: "no running node falls to the config", cfgListen: "127.0.0.1:4000", want: "http://127.0.0.1:4000"},
		{name: "a wildcard listen is dialled on loopback", running: "0.0.0.0:3000", want: "http://127.0.0.1:3000"},
		{name: "nothing known falls back to the default port", want: "http://127.0.0.1:1234"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := pickAddr(tc.env, tc.running, tc.cfgListen); got != tc.want {
				t.Fatalf("pickAddr = %q, want %q", got, tc.want)
			}
		})
	}
}

// A foreground `mfsh serve -port` left no record, so the commands looked at
// the config and missed it. Every node now records its address while it runs.
func TestListenRecord(t *testing.T) {
	writeRec := func(t *testing.T, path string, r daemonRecord) {
		t.Helper()
		b, _ := json.Marshal(r)
		if err := os.WriteFile(path, b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	tests := []struct {
		name string
		// setup runs in a fresh state dir and returns the expected runningListen.
		setup func(t *testing.T) string
	}{
		{name: "this process's record is found, and gone once removed", setup: func(t *testing.T) string {
			remove, err := writeListenRecord("127.0.0.1:3000")
			if err != nil {
				t.Fatal(err)
			}
			if got := runningListen(); got != "127.0.0.1:3000" {
				t.Fatalf("while running: %q", got)
			}
			remove()
			return ""
		}},
		{name: "a record from a process that is gone is ignored", setup: func(t *testing.T) string {
			writeRec(t, listenRecordPath(), daemonRecord{PID: os.Getpid(), BirthID: "not-this-process", Listen: "127.0.0.1:3000"})
			return ""
		}},
		{name: "a node from an older build is still found by up's record", setup: func(t *testing.T) string {
			birth, _ := osproc.BirthID(os.Getpid())
			writeRec(t, daemonRecordPath(), daemonRecord{PID: os.Getpid(), BirthID: birth, Listen: "127.0.0.1:5000"})
			return "127.0.0.1:5000"
		}},
		{name: "removing does not delete a record another node wrote since", setup: func(t *testing.T) string {
			remove, err := writeListenRecord("127.0.0.1:3000")
			if err != nil {
				t.Fatal(err)
			}
			writeRec(t, listenRecordPath(), daemonRecord{PID: os.Getpid() + 1, BirthID: "other", Listen: "127.0.0.1:4000"})
			remove()
			if _, err := os.Stat(listenRecordPath()); err != nil {
				t.Fatalf("the other node's record was removed: %v", err)
			}
			return ""
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("MFSH_STATE_DIR", t.TempDir())
			want := tc.setup(t)
			if got := runningListen(); got != want {
				t.Fatalf("runningListen = %q, want %q", got, want)
			}
		})
	}
}

// A config without "listen" is the default address, not whatever node is
// running: another node sharing the state directory had recorded
// 127.0.0.1:18700, and `mfsh up` refused to start this one on :1234.
func TestResolveListenIgnoresOtherRunningNodes(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("MFSH_STATE_DIR", dir)
	if _, err := writeListenRecord("127.0.0.1:18700"); err != nil {
		t.Fatal(err)
	}
	cfg := dir + "/config.json"
	os.WriteFile(cfg, []byte(`{"engine_bind":"tailnet"}`), 0o644)
	tests := []struct{ name, flag, want string }{
		{name: "no listen in the config is the default", want: "127.0.0.1:1234"},
		{name: "the flag wins", flag: "127.0.0.1:3000", want: "127.0.0.1:3000"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolveListen(cfg, tc.flag); got != tc.want {
				t.Fatalf("resolveListen = %q, want %q", got, tc.want)
			}
		})
	}
}
