package hub

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// A 206 alone does not establish that the body continues our partial file.
// Even a hashless hub file must reject a wrong range before appending bytes.
func TestResumeRejectsInvalidRangeWithoutChangingPartial(t *testing.T) {
	for _, tc := range []struct{ name, contentRange, body string }{
		{"missing range", "", "bytes"},
		{"wrong offset", "bytes 0-4/12", "bytes"},
		{"wrong total", "bytes 7-11/13", "bytes"},
		{"wrong length", "bytes 7-11/12", "byte"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dest := filepath.Join(t.TempDir(), "model.gguf")
			if err := os.WriteFile(dest+".part", []byte("engine "), 0o600); err != nil {
				t.Fatal(err)
			}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Range", tc.contentRange)
				w.WriteHeader(206)
				w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			err := client(srv).fetch(context.Background(), &Plan{Repo: "u/r", Revision: "pinned"}, File{Name: "model.gguf", Size: 12}, dest, func(int64) {})
			if err == nil {
				t.Error("accepted invalid range")
			}
			b, err := os.ReadFile(dest + ".part")
			if err != nil || string(b) != "engine " {
				t.Errorf("partial changed: %q, %v", b, err)
			}
		})
	}
}
