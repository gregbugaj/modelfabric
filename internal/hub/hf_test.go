package hub

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
)

func sum(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

// fakeHub serves the two endpoints the client uses and records which revision
// each download asked for.
type fakeHub struct {
	files     map[string][]byte // name -> content served
	published map[string]string // name -> sha256 the API claims
	sha       string
	revisions atomic.Value
	rangeHits atomic.Int32
}

func newFakeHub(t *testing.T, files map[string][]byte) (*fakeHub, *httptest.Server) {
	f := &fakeHub{files: files, published: map[string]string{}, sha: "abc123def4567890"}
	for n, b := range files {
		f.published[n] = sum(b)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/models/") {
			type sib struct {
				Name string `json:"rfilename"`
				Size int64  `json:"size"`
				LFS  any    `json:"lfs"`
			}
			var sibs []sib
			for n, b := range f.files {
				sibs = append(sibs, sib{n, int64(len(b)), map[string]any{"sha256": f.published[n], "size": len(b)}})
			}
			sibs = append(sibs, sib{Name: "README.md", Size: 10})
			_ = json.NewEncoder(w).Encode(map[string]any{"sha": f.sha, "siblings": sibs})
			return
		}
		parts := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/"), "/", 5)
		if len(parts) < 5 || parts[2] != "resolve" {
			http.NotFound(w, r)
			return
		}
		f.revisions.Store(parts[3])
		b, ok := f.files[parts[4]]
		if !ok {
			http.NotFound(w, r)
			return
		}
		if rg := r.Header.Get("Range"); rg != "" {
			f.rangeHits.Add(1)
			from, _ := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(rg, "bytes="), "-"))
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", from, len(b)-1, len(b)))
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write(b[from:])
			return
		}
		_, _ = w.Write(b)
	}))
	t.Cleanup(srv.Close)
	return f, srv
}

func client(srv *httptest.Server) *Client {
	return &Client{BaseURL: srv.URL, HTTP: srv.Client()}
}

func TestParseRef(t *testing.T) {
	cases := map[string]Ref{
		"lmstudio-community/Qwen3-0.6B-GGUF":                {Repo: "lmstudio-community/Qwen3-0.6B-GGUF"},
		"user/repo@q8_0":                                    {Repo: "user/repo", Quant: "Q8_0"},
		"https://huggingface.co/user/repo":                  {Repo: "user/repo"},
		"https://huggingface.co/user/repo/blob/main/x.gguf": {Repo: "user/repo"},
	}
	for in, want := range cases {
		got, err := ParseRef(in)
		if err != nil || got != want {
			t.Errorf("ParseRef(%q) = %+v, %v; want %+v", in, got, err, want)
		}
	}
	for _, bad := range []string{"justaname", "https://example.com/user/repo", "a/b/c", "../etc/passwd"} {
		if _, err := ParseRef(bad); err == nil {
			t.Errorf("ParseRef(%q) should fail", bad)
		}
	}
}

func TestResolveChoosesQuantAndProjector(t *testing.T) {
	_, srv := newFakeHub(t, map[string][]byte{
		"M-Q8_0.gguf":        []byte("q8"),
		"M-Q4_K_M.gguf":      []byte("q4km"),
		"mmproj-M-F16.gguf":  []byte("proj16"),
		"mmproj-M-BF16.gguf": []byte("projbf"),
	})
	p, err := client(srv).Resolve(context.Background(), Ref{Repo: "u/r"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if p.Quant != "Q4_K_M" {
		t.Fatalf("default quant = %s, want Q4_K_M", p.Quant)
	}
	names := []string{}
	for _, f := range p.Files {
		names = append(names, f.Name)
	}
	if strings.Join(names, ",") != "M-Q4_K_M.gguf,mmproj-M-F16.gguf" {
		t.Fatalf("files = %v; want the Q4_K_M weights plus the F16 projector", names)
	}

	p, err = client(srv).Resolve(context.Background(), Ref{Repo: "u/r", Quant: "Q8_0"})
	if err != nil || p.Files[0].Name != "M-Q8_0.gguf" {
		t.Fatalf("explicit quant: %v %v", p, err)
	}
	if _, err := client(srv).Resolve(context.Background(), Ref{Repo: "u/r", Quant: "Q2_K"}); err == nil ||
		!strings.Contains(err.Error(), "available") {
		t.Fatalf("a missing quant should list what is available, got %v", err)
	}
}

// Downloads are pinned to the resolved commit, never a moving branch, and land
// only after the content matches the hub's published hash.
func TestDownloadVerifiesAndPinsRevision(t *testing.T) {
	fh, srv := newFakeHub(t, map[string][]byte{"M-Q4_K_M.gguf": []byte("the real weights")})
	c := client(srv)
	p, _ := c.Resolve(context.Background(), Ref{Repo: "u/r"})
	root := t.TempDir()
	dir, err := c.Download(context.Background(), p, root, nil)
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	if got := fh.revisions.Load(); got != fh.sha {
		t.Fatalf("downloaded revision %v, want the resolved commit %s", got, fh.sha)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "M-Q4_K_M.gguf"))
	if string(b) != "the real weights" {
		t.Fatalf("content = %q", b)
	}
	if dir != filepath.Join(root, "u", "r") {
		t.Fatalf("dir = %s; want the catalog's <user>/<repo> layout", dir)
	}
}

func TestDownloadRejectsChecksumMismatch(t *testing.T) {
	fh, srv := newFakeHub(t, map[string][]byte{"M-Q4_K_M.gguf": []byte("tampered bytes!!")})
	fh.published["M-Q4_K_M.gguf"] = sum([]byte("what the hub said"))
	c := client(srv)
	p, _ := c.Resolve(context.Background(), Ref{Repo: "u/r"})
	root := t.TempDir()
	_, err := c.Download(context.Background(), p, root, nil)
	if !errors.Is(err, ErrChecksum) {
		t.Fatalf("err = %v; want ErrChecksum", err)
	}
	for _, n := range []string{"M-Q4_K_M.gguf", "M-Q4_K_M.gguf.part"} {
		if _, err := os.Stat(filepath.Join(root, "u", "r", n)); err == nil {
			t.Fatalf("%s left behind after a checksum failure", n)
		}
	}
}

func TestDownloadResumesPartialFile(t *testing.T) {
	content := []byte(strings.Repeat("0123456789", 1000))
	fh, srv := newFakeHub(t, map[string][]byte{"M-Q4_K_M.gguf": content})
	c := client(srv)
	p, _ := c.Resolve(context.Background(), Ref{Repo: "u/r"})
	root := t.TempDir()
	dir := filepath.Join(root, "u", "r")
	_ = os.MkdirAll(dir, 0o755)
	_ = os.WriteFile(filepath.Join(dir, "M-Q4_K_M.gguf.part"), content[:4000], 0o644)

	if _, err := c.Download(context.Background(), p, root, nil); err != nil {
		t.Fatalf("Download: %v", err)
	}
	if fh.rangeHits.Load() != 1 {
		t.Fatalf("expected a Range request to resume, got %d", fh.rangeHits.Load())
	}
	b, _ := os.ReadFile(filepath.Join(dir, "M-Q4_K_M.gguf"))
	if string(b) != string(content) {
		t.Fatal("resumed file does not match the original")
	}
}

func TestDownloadSkipsCompleteFile(t *testing.T) {
	content := []byte("already here")
	fh, srv := newFakeHub(t, map[string][]byte{"M-Q4_K_M.gguf": content})
	c := client(srv)
	p, _ := c.Resolve(context.Background(), Ref{Repo: "u/r"})
	root := t.TempDir()
	dir := filepath.Join(root, "u", "r")
	_ = os.MkdirAll(dir, 0o755)
	_ = os.WriteFile(filepath.Join(dir, "M-Q4_K_M.gguf"), content, 0o644)
	if _, err := c.Download(context.Background(), p, root, nil); err != nil {
		t.Fatalf("Download: %v", err)
	}
	if fh.revisions.Load() != nil {
		t.Fatal("a complete, verified file should not be downloaded again")
	}
}

func TestRefusesAmbiguousNonShardFiles(t *testing.T) {
	_, srv := newFakeHub(t, map[string][]byte{
		"a-Q4_K_M.gguf": []byte("a"),
		"b-Q4_K_M.gguf": []byte("b"),
	})
	if _, err := client(srv).Resolve(context.Background(), Ref{Repo: "u/r"}); err == nil ||
		!strings.Contains(err.Error(), "not shards") {
		t.Fatalf("ambiguous files should be refused, got %v", err)
	}
}

func TestHubGGUFRepoMapsExactNameOnly(t *testing.T) {
	var queries []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		queries = append(queries, r.URL.RawQuery)
		if r.URL.Query().Get("author") != LMStudioCommunity {
			t.Errorf("searched author %q", r.URL.Query().Get("author"))
		}
		_, _ = io.WriteString(w, `[
			{"id":"lmstudio-community/Qwen3.8-27B-MLX-4bit"},
			{"id":"lmstudio-community/Qwen3.8-27B-GGUF"},
			{"id":"lmstudio-community/gemma-4-12B-it-GGUF"}]`)
	}))
	defer srv.Close()
	c := client(srv)
	ctx := context.Background()

	repo, _, err := c.HubGGUFRepo(ctx, "qwen/qwen3.8-27b")
	if err != nil || repo != "lmstudio-community/Qwen3.8-27B-GGUF" {
		t.Fatalf("repo = %q, err = %v", repo, err)
	}
	// A near miss is offered, never silently chosen: "gemma-4-12b" is not
	// "gemma-4-12B-it", and guessing would download a different model.
	repo, near, err := c.HubGGUFRepo(ctx, "google/gemma-4-12b")
	if err != nil || repo != "" || len(near) == 0 {
		t.Fatalf("repo = %q, near = %v, err = %v", repo, near, err)
	}
	for _, n := range near {
		if strings.Contains(n, "MLX") {
			t.Errorf("non-GGUF repo offered: %s", n)
		}
	}
	before := len(queries)
	for _, id := range []string{"bartowski/Qwen_Qwen3-8B-GGUF", "lmstudio-community/anything"} {
		if repo, near, err := c.HubGGUFRepo(ctx, id); repo != "" || near != nil || err != nil {
			t.Errorf("%s was remapped: %q %v %v", id, repo, near, err)
		}
	}
	if len(queries) != before {
		t.Error("searched for a reference that is already a repository")
	}
}

// Repository paths and filenames are not URL-safe by nature: "#", "?", "%" and
// spaces are legal in a Hugging Face filename and used to be pasted into the
// download URL as syntax.
func TestDownloadURLComponentsAreEscaped(t *testing.T) {
	got := joinEscaped("lmstudio-community/Qwen 3#1/model (q4).gguf")
	for _, raw := range []string{" ", "#"} {
		if strings.Contains(got, raw) {
			t.Errorf("%q survived escaping: %s", raw, got)
		}
	}
	if !strings.Contains(got, "/") {
		t.Errorf("path separators must survive: %s", got)
	}
}

// Two files with the same basename in different directories used to be written
// to one destination, where the second could "resume" the first on a size match.
func TestPlanFilesKeepTheirRepositoryPath(t *testing.T) {
	a, err := safeRel("a/model.gguf")
	if err != nil {
		t.Fatal(err)
	}
	b, err := safeRel("b/model.gguf")
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Fatalf("two repository paths collapsed to %q", a)
	}
	if _, err := safeRel("../../etc/passwd"); err != nil {
		t.Fatalf("a traversing name should be cleaned, not rejected outright: %v", err)
	}
	if rel, _ := safeRel("../../etc/passwd"); strings.HasPrefix(rel, "..") {
		t.Errorf("a traversing name escaped: %q", rel)
	}
}

// Several files at one quantization are either the shards of one model or
// something ambiguous. Checking only that each name looks shard-shaped
// accepted two models' shards mixed together, and an incomplete set; both of
// which download something that cannot load.
func TestShardSetsMustBeOneCompleteModel(t *testing.T) {
	ok := []File{
		{Name: "big-00001-of-00003.gguf"},
		{Name: "big-00002-of-00003.gguf"},
		{Name: "big-00003-of-00003.gguf"},
	}
	if err := oneCompleteShardSet(ok); err != nil {
		t.Errorf("a complete set was rejected: %v", err)
	}
	for name, files := range map[string][]File{
		"two models": {{Name: "a-00001-of-00002.gguf"}, {Name: "b-00002-of-00002.gguf"}},
		"incomplete": {{Name: "big-00001-of-00003.gguf"}, {Name: "big-00002-of-00003.gguf"}},
		"duplicate":  {{Name: "big-00001-of-00002.gguf"}, {Name: "big-00001-of-00002.gguf"}},
		"not shards": {{Name: "big.gguf"}, {Name: "big2.gguf"}},
	} {
		if err := oneCompleteShardSet(files); err == nil {
			t.Errorf("%s was accepted as one complete set", name)
		}
	}
}

// A repository holding two multimodal variants holds two projectors, named the
// way the hub names them (mmproj-<model>-<precision>.gguf). Choosing the
// best-ranked one globally could pair a model with another model's projector,
// which makes it claim vision it cannot do.
func TestProjectorFollowsTheSelectedWeights(t *testing.T) {
	files := []File{
		{Name: "Gemma-3-27B-Q4_K_M.gguf"},
		{Name: "mmproj-Gemma-3-27B-F16.gguf"},
		{Name: "Qwen3-VL-8B-Q4_K_M.gguf"},
		{Name: "mmproj-Qwen3-VL-8B-BF16.gguf"},
	}
	got := chooseProjector(files, []File{{Name: "Qwen3-VL-8B-Q4_K_M.gguf"}})
	if got == nil || got.Name != "mmproj-Qwen3-VL-8B-BF16.gguf" {
		t.Fatalf("chose %v, want the Qwen projector", got)
	}
	got = chooseProjector(files, []File{{Name: "Gemma-3-27B-Q4_K_M.gguf"}})
	if got == nil || got.Name != "mmproj-Gemma-3-27B-F16.gguf" {
		t.Fatalf("chose %v, want the Gemma projector", got)
	}
	single := []File{{Name: "Qwen3.8-27B-Q4_K_M.gguf"}, {Name: "mmproj-Qwen3.8-27B-BF16.gguf"}}
	if got := chooseProjector(single, single[:1]); got == nil || got.Name != "mmproj-Qwen3.8-27B-BF16.gguf" {
		t.Fatalf("a lone projector was not used: %v", got)
	}
}
