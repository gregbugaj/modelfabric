package rtpkg

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func TestDownloadValidatesResumeBeforeAppending(t *testing.T) {
	for _, tc := range []struct {
		name, partial, contentRange, body string
		status                            int
		wantErr                           bool
	}{
		{"valid resume", "engine ", "bytes 7-11/12", "bytes", 206, false},
		{"ignored range restarts", "engine ", "", "engine bytes", 200, false},
		{"unsolicited partial is refused", "", "bytes 0-11/12", "engine bytes", 206, true},
		{"missing range preserves partial", "engine ", "", "bytes", 206, true},
		{"wrong offset preserves partial", "engine ", "bytes 0-4/12", "bytes", 206, true},
		{"wrong total preserves partial", "engine ", "bytes 7-11/13", "bytes", 206, true},
		{"wrong length preserves partial", "engine ", "bytes 7-11/12", "byte", 206, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dest := filepath.Join(t.TempDir(), "engine.tar.gz")
			if tc.partial != "" {
				if err := os.WriteFile(dest+".part", []byte(tc.partial), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tc.partial != "" && r.Header.Get("Range") != "bytes=7-" {
					t.Errorf("Range = %q", r.Header.Get("Range"))
				}
				w.Header().Set("Content-Range", tc.contentRange)
				w.WriteHeader(tc.status)
				w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			sum := sha256.Sum256([]byte("engine bytes"))
			a := Asset{Name: "engine.tar.gz", URL: srv.URL, Size: 12, SHA256: hex.EncodeToString(sum[:])}
			err := (&Client{HTTP: srv.Client()}).download(context.Background(), a, dest, nil)
			if tc.wantErr {
				if err == nil {
					t.Fatal("accepted invalid partial response")
				}
				if _, err := os.Stat(dest); !os.IsNotExist(err) {
					t.Errorf("published an invalid download: %v", err)
				}
				if tc.partial != "" {
					b, err := os.ReadFile(dest + ".part")
					if err != nil || string(b) != tc.partial {
						t.Errorf("partial changed: %q, %v", b, err)
					}
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				b, err := os.ReadFile(dest)
				if err != nil || string(b) != "engine bytes" {
					t.Fatalf("download = %q, %v", b, err)
				}
			}
		})
	}
}

func TestDownloadReportsUnreadablePartialBeforeRequest(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "engine.tar.gz")
	if err := os.Mkdir(dest+".part", 0o700); err != nil {
		t.Fatal(err)
	}
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests.Add(1); w.Write([]byte("engine bytes")) }))
	defer srv.Close()
	err := (&Client{HTTP: srv.Client()}).download(context.Background(), Asset{Name: "engine.tar.gz", URL: srv.URL, SHA256: strings.Repeat("0", 64)}, dest, nil)
	if err == nil {
		t.Error("unreadable partial was ignored")
	}
	if requests.Load() != 0 {
		t.Error("download began despite failing to read its partial file")
	}
}
