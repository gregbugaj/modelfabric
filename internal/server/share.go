package server

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gregbugaj/modelfabric/internal/catalog"
)

// Sharing a model's files with another node of the same owner, so a model one
// machine already has is copied across the tailnet instead of downloaded from
// Hugging Face again (`mfsh get <model> -from <node>`).
//
// Both routes are under /api/v1, so over the mesh they are same-owner only,
// like management: weights can be gated or licensed, and handing them to any
// tailnet member would be redistributing them. The routes serve only files
// that belong to a model in the catalog, looked up by name, never a path
// taken from the request.
//
// The file route has the shape of a Hugging Face resolve URL,
// <repo>/resolve/<revision>/<file>, so the receiving side reuses the hub
// client whole: resume by Range, a SHA-256 checked while the bytes stream,
// and a .part file renamed only once the digest matches.

func (s *Server) registerShare(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/share/model", s.handleShareModel)
	mux.HandleFunc("GET /api/v1/share/files/{rest...}", s.handleShareFile)
}

// SharedFile is one file of a shared model.
type SharedFile struct {
	Name   string `json:"name"` // relative to the model's directory
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// SharedModel is what a node offers for one model.
type SharedModel struct {
	Key    string `json:"key"`
	Format string `json:"format"`
	// Dir is where the model sits under its models root, forward slashes;
	// the copy lands at the same place under the receiver's root, so a model
	// named from its path keeps its name.
	Dir string `json:"dir"`
	// Repo is Dir as it appears in file URLs. It differs only for a model at
	// the top of its root, whose Dir is "." and cannot be a URL segment.
	Repo string `json:"repo"`
	// Revision is the digest of the file list, so a listing and the files
	// fetched after it can be told apart from a later change.
	Revision string       `json:"revision"`
	Files    []SharedFile `json:"files"`
}

func (s *Server) handleShareModel(w http.ResponseWriter, r *http.Request) {
	if !s.requireSupervisor(w) {
		return
	}
	ref := r.URL.Query().Get("model")
	if ref == "" {
		writeError(w, http.StatusBadRequest, "name the model: ?model=<key>")
		return
	}
	m, err := s.sup.Catalog().ResolveFormat(ref, r.URL.Query().Get("format"))
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	offer, err := s.offer(m)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "cannot share "+m.Key+": "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, offer)
}

func (s *Server) handleShareFile(w http.ResponseWriter, r *http.Request) {
	if !s.requireSupervisor(w) {
		return
	}
	rest := r.PathValue("rest")
	for _, m := range s.sup.Catalog().Models() {
		dir, rel, err := shareLayout(m)
		if err != nil {
			continue
		}
		prefix := repoToken(rel) + "/resolve/"
		if !strings.HasPrefix(rest, prefix) {
			continue
		}
		// <revision>/<file>. The revision is not checked: the receiver checks
		// every file against the digest it was listed with, which catches a
		// change between listing and fetching either way.
		_, name, ok := strings.Cut(strings.TrimPrefix(rest, prefix), "/")
		if !ok {
			continue
		}
		_, files, err := catalog.ModelFiles(m)
		if err != nil {
			continue
		}
		for _, f := range files {
			if f == name {
				serveSharedFile(w, r, filepath.Join(dir, filepath.FromSlash(f)))
				return
			}
		}
	}
	writeError(w, http.StatusNotFound, "no shared model file at "+rest)
}

func serveSharedFile(w http.ResponseWriter, r *http.Request, path string) {
	f, err := os.Open(path)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	// A weights file is bytes; a browser guessing otherwise from its first
	// kilobyte is no use to anyone.
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	http.ServeContent(w, r, "", info.ModTime(), f) // Range, for resume
}

// offer lists a model's files with their digests.
func (s *Server) offer(m catalog.Model) (SharedModel, error) {
	dir, rel, err := shareLayout(m)
	if err != nil {
		return SharedModel{}, err
	}
	_, files, err := catalog.ModelFiles(m)
	if err != nil {
		return SharedModel{}, err
	}
	out := SharedModel{Key: m.Key, Format: m.Format, Dir: rel, Repo: repoToken(rel)}
	list := sha256.New()
	for _, f := range files {
		p := filepath.Join(dir, filepath.FromSlash(f))
		size, sum, err := s.shared.digest(p)
		if err != nil {
			return SharedModel{}, err
		}
		out.Files = append(out.Files, SharedFile{Name: f, Size: size, SHA256: sum})
		fmt.Fprintf(list, "%s  %d  %s\n", sum, size, f)
	}
	out.Revision = hex.EncodeToString(list.Sum(nil))
	return out, nil
}

// shareLayout returns the directory holding a model's files and where that
// directory sits under the models root the model was found in.
func shareLayout(m catalog.Model) (dir, rel string, err error) {
	dir, _, err = catalog.ModelFiles(m)
	if err != nil {
		return "", "", err
	}
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return "", "", err
	}
	absRoot, err := filepath.Abs(m.Source)
	if err != nil {
		return "", "", err
	}
	r, err := filepath.Rel(absRoot, absDir)
	if err != nil || !filepath.IsLocal(r) {
		return "", "", fmt.Errorf("%s is not under its models root %s", dir, m.Source)
	}
	return absDir, filepath.ToSlash(r), nil
}

// repoToken turns a directory under the models root into a URL segment. A
// model at the top of its root has Dir "."; "/./resolve/" would be cleaned
// and redirected by the mux, so it travels as "-", which no scanned directory
// is named alone.
func repoToken(rel string) string {
	if rel == "." {
		return "-"
	}
	return rel
}

// shareHashes remembers file digests by path, size and modification time, so
// a second listing of the same model does not read 18 GB again. The same
// trade the pin store makes: an edit that keeps size and mtime goes unseen
// here, and the receiver's own digest check still catches corruption in
// transit.
type shareHashes struct {
	mu   sync.Mutex
	sums map[string]shareHash
}

type shareHash struct {
	size int64
	mod  time.Time
	sum  string
}

// shareHashesMax bounds the cache. Keys are catalog files, not request input,
// so this is a ceiling for a very large models directory, not a defence.
const shareHashesMax = 4096

func (h *shareHashes) digest(path string) (int64, string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, "", err
	}
	h.mu.Lock()
	if c, ok := h.sums[path]; ok && c.size == info.Size() && c.mod.Equal(info.ModTime()) {
		h.mu.Unlock()
		return c.size, c.sum, nil
	}
	h.mu.Unlock()

	// Hashed outside the lock: minutes for a large model, and a listing of
	// one model must not hold up a listing of another.
	f, err := os.Open(path)
	if err != nil {
		return 0, "", err
	}
	defer f.Close()
	sum := sha256.New()
	n, err := io.Copy(sum, f)
	if err != nil {
		return 0, "", err
	}
	if n != info.Size() {
		return 0, "", fmt.Errorf("%s changed while it was hashed", path)
	}
	digest := hex.EncodeToString(sum.Sum(nil))

	h.mu.Lock()
	if h.sums == nil || len(h.sums) >= shareHashesMax {
		h.sums = map[string]shareHash{}
	}
	h.sums[path] = shareHash{size: info.Size(), mod: info.ModTime(), sum: digest}
	h.mu.Unlock()
	return info.Size(), digest, nil
}
