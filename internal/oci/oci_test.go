package oci

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func layer(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range files {
		tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg})
		tw.Write([]byte(body))
	}
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

func digest(b []byte) string { s := sha256.Sum256(b); return "sha256:" + hex.EncodeToString(s[:]) }

// fakeRegistry serves an index → amd64 manifest → two layers; the top layer
// overrides the file in the bottom one, as image layers do.
func fakeRegistry(t *testing.T, tamper bool) (*httptest.Server, string) {
	bottom := layer(t, map[string]string{"app/epp": "old", "etc/other": "x"})
	top := layer(t, map[string]string{"app/epp": "new binary"})
	blobs := map[string][]byte{digest(bottom): bottom, digest(top): top}
	man := []byte(fmt.Sprintf(`{"schemaVersion":2,"layers":[
		{"mediaType":"application/vnd.oci.image.layer.v1.tar+gzip","digest":%q,"size":%d},
		{"mediaType":"application/vnd.oci.image.layer.v1.tar+gzip","digest":%q,"size":%d}]}`,
		digest(bottom), len(bottom), digest(top), len(top)))
	idx := []byte(fmt.Sprintf(`{"schemaVersion":2,"manifests":[
		{"digest":"sha256:%s","platform":{"os":"linux","architecture":"arm64"}},
		{"digest":%q,"platform":{"os":"linux","architecture":"amd64"}}]}`, strings.Repeat("0", 64), digest(man)))
	manifests := map[string][]byte{digest(man): man, digest(idx): idx}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/token":
			w.Write([]byte(`{"token":"t"}`))
		case r.Header.Get("Authorization") != "Bearer t":
			http.Error(w, "unauthorized", http.StatusUnauthorized)
		case strings.Contains(r.URL.Path, "/manifests/"):
			d := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
			b, ok := manifests[d]
			if !ok {
				http.NotFound(w, r)
				return
			}
			mt := "application/vnd.oci.image.manifest.v1+json"
			if d == digest(idx) {
				mt = "application/vnd.oci.image.index.v1+json"
			}
			w.Header().Set("Content-Type", mt)
			w.Write(b)
		case strings.Contains(r.URL.Path, "/blobs/"):
			d := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
			b := blobs[d]
			if tamper && d == digest(top) {
				b = layer(t, map[string]string{"app/epp": "evil"})
			}
			w.Write(b)
		}
	}))
	return srv, digest(idx)
}

func extract(t *testing.T, srv *httptest.Server, idx string) (string, error) {
	t.Helper()
	dest := filepath.Join(t.TempDir(), "epp")
	ref := Ref{Registry: strings.TrimPrefix(srv.URL, "https://"), Repository: "llm-d/epp", Digest: idx}
	c := &client{http: srv.Client(), ref: ref}
	if err := c.auth(context.Background()); err != nil {
		return "", err
	}
	err := extractWith(context.Background(), c, "amd64", "app/epp", dest, nil)
	b, _ := os.ReadFile(dest)
	return string(b), err
}

func TestExtractsTopmostCopyOfFile(t *testing.T) {
	srv, idx := fakeRegistry(t, false)
	defer srv.Close()
	got, err := extract(t, srv, idx)
	if err != nil || got != "new binary" {
		t.Fatalf("got %q, %v", got, err)
	}
}

func TestRejectsTamperedLayer(t *testing.T) {
	srv, idx := fakeRegistry(t, true)
	defer srv.Close()
	got, err := extract(t, srv, idx)
	if !errors.Is(err, ErrDigest) || got != "" {
		t.Fatalf("tampered layer accepted: %q, %v", got, err)
	}
}

func TestRejectsWrongPinnedDigest(t *testing.T) {
	srv, _ := fakeRegistry(t, false)
	defer srv.Close()
	if _, err := extract(t, srv, "sha256:"+strings.Repeat("a", 64)); err == nil {
		t.Fatal("an unknown pin was accepted")
	}
}

// An image can delete a file in a later layer. The search reads layers top
// down, so ignoring whiteouts meant it walked past the deletion and extracted
// the copy from a lower layer.
func TestWhiteoutsDeleteAPath(t *testing.T) {
	cases := []struct {
		name, want string
		deletes    bool
	}{
		{"usr/bin/.wh.epp", "usr/bin/epp", true},
		{".wh.epp", "epp", true},
		{"usr/bin/.wh..wh..opq", "usr/bin/epp", true},
		{"usr/.wh..wh..opq", "usr/bin/epp", true},
		{"usr/bin/.wh.other", "usr/bin/epp", false},
		{"usr/bin/epp", "usr/bin/epp", false},
		{"other/.wh.epp", "usr/bin/epp", false},
	}
	for _, c := range cases {
		if got := whiteoutOf(c.name, c.want); got != c.deletes {
			t.Errorf("whiteoutOf(%q, %q) = %v, want %v", c.name, c.want, got, c.deletes)
		}
	}
}
