package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/gregbugaj/modelfabric/internal/config"
	"github.com/gregbugaj/modelfabric/internal/hub"
	"github.com/gregbugaj/modelfabric/internal/mesh"
	"github.com/gregbugaj/modelfabric/internal/router"
	"github.com/gregbugaj/modelfabric/internal/supervisor"
)

// shareServer is a node whose models root holds one split GGUF with a
// projector, and a file beside it that belongs to no model.
func shareServer(t *testing.T) (*Server, string) {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "acme", "tiny-GGUF")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"tiny-Q4_K_M-00001-of-00002.gguf": "first shard",
		"tiny-Q4_K_M-00002-of-00002.gguf": "second shard",
		"mmproj-tiny-F16.gguf":            "projector",
		"notes.txt":                       "not part of any model",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "outside.txt"), []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	m := mesh.New(config.Default(), "minion")
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	sup := supervisor.New(supervisor.Config{DataDir: t.TempDir(), ModelsRoot: root}, nil, nil, nil, m, log)
	if err := sup.Rescan(); err != nil {
		t.Fatal(err)
	}
	return New(m, router.New(m, log), sup, log, nil), root
}

func shareOffer(t *testing.T, base string) SharedModel {
	t.Helper()
	resp, err := http.Get(base + "/api/v1/share/model?model=" + url.QueryEscape("acme/tiny-GGUF/tiny-Q4_K_M-00001-of-00002"))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("listing: %d %s", resp.StatusCode, b)
	}
	var offer SharedModel
	if err := json.NewDecoder(resp.Body).Decode(&offer); err != nil {
		t.Fatal(err)
	}
	return offer
}

// The receiving side is the Hugging Face client pointed at a node, so this is
// the whole copy: listing, fetch, digest check and rename.
func TestShareCopiesEveryFileOfAModel(t *testing.T) {
	s, _ := shareServer(t)
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	offer := shareOffer(t, ts.URL)
	if offer.Dir != "acme/tiny-GGUF" {
		t.Errorf("dir %q", offer.Dir)
	}
	// A split model is every shard; a model listed by its first shard alone
	// would arrive as a third of itself.
	if len(offer.Files) != 3 {
		t.Fatalf("want both shards and the projector, got %+v", offer.Files)
	}
	for _, f := range offer.Files {
		if f.Name == "notes.txt" {
			t.Errorf("a file beside the model is not part of it")
		}
	}

	files := make([]hub.File, len(offer.Files))
	for i, f := range offer.Files {
		files[i] = hub.File{Name: f.Name, Size: f.Size, SHA256: f.SHA256}
	}
	dest := filepath.Join(t.TempDir(), filepath.FromSlash(offer.Dir))
	c := &hub.Client{BaseURL: ts.URL + "/api/v1/share/files", HTTP: http.DefaultClient}
	err := c.DownloadInto(context.Background(), &hub.Plan{Repo: offer.Repo, Revision: offer.Revision, Files: files}, dest, nil)
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(dest, "tiny-Q4_K_M-00002-of-00002.gguf"))
	if err != nil || string(got) != "second shard" {
		t.Errorf("second shard: %q %v", got, err)
	}
}

func TestShareListsTheDigestOfWhatIsOnDisk(t *testing.T) {
	s, _ := shareServer(t)
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	want := sha256.Sum256([]byte("projector"))
	for _, f := range shareOffer(t, ts.URL).Files {
		if f.Name == "mmproj-tiny-F16.gguf" && f.SHA256 != hex.EncodeToString(want[:]) {
			t.Errorf("projector digest %s", f.SHA256)
		}
	}
}

// Only a file of a cataloged model is served, looked up by name. A path built
// from the request would read anything the node can.
func TestShareServesNothingElse(t *testing.T) {
	s, _ := shareServer(t)
	for _, p := range []string{
		"/api/v1/share/files/acme/tiny-GGUF/resolve/x/notes.txt",
		"/api/v1/share/files/acme/tiny-GGUF/resolve/x/../../outside.txt",
		"/api/v1/share/files/acme/tiny-GGUF/resolve/x/%2E%2E/%2E%2E/outside.txt",
		"/api/v1/share/files/-/resolve/x/outside.txt",
		"/api/v1/share/files/acme/tiny-GGUF/tiny-Q4_K_M-00001-of-00002.gguf",
	} {
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, p, nil))
		if rec.Code == 200 {
			t.Errorf("%s served: %q", p, rec.Body.String())
		}
	}
}

// Weights can be gated or licensed, so a peer gets them only if it is the
// owner's own device, the same gate as management.
func TestShareIsForTheOwnersDevices(t *testing.T) {
	s := adminServer(t, "")
	for _, p := range []string{"/api/v1/share/model?model=x", "/api/v1/share/files/a/resolve/r/f.gguf"} {
		if rec := peerGet(s, "100.0.0.2", p); rec.Code != 403 {
			t.Errorf("another user's device, %s: %d", p, rec.Code)
		}
		if rec := peerGet(s, "100.0.0.1", p); rec.Code == 403 {
			t.Errorf("owner's device refused, %s: %s", p, rec.Body)
		}
	}
	if rec := peerGet(adminServer(t, "off"), "100.0.0.1", "/api/v1/share/model?model=x"); rec.Code != 403 {
		t.Errorf("mesh_admin off: %d", rec.Code)
	}
}
