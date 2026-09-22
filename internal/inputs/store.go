package inputs

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// ErrChanged reports that pinned inputs no longer match what is on disk.
var ErrChanged = errors.New("inputs changed since they were pinned")

// Store persists prepared inputs so verification is meaningful across runs.
//
// Without it, verification is self-referential: building a manifest from the
// current files and checking the files against that manifest always succeeds,
// however the files were modified. Preparation must happen once, and every
// later load is checked against that pin.
type Store struct {
	// prep serializes first-time pinning; mu guards the files themselves.
	prep sync.Mutex

	dir string
	mu  sync.Mutex
	// VerifyFull re-hashes every file on every load, skipping the size+mtime
	// fast path.
	VerifyFull bool
}

type pin struct {
	Key            string `json:"key"`
	Root           string `json:"root"`
	ManifestSHA256 string `json:"manifest_sha256"`
	Revision       string `json:"revision,omitempty"`
	// Manifest is retained verbatim so a later load can be checked against the
	// original, not against a freshly computed one.
	Manifest string `json:"manifest"`
	// Stamps record each file's size and modification time at pin time. They
	// make a repeat load cheap: re-hashing a 16GB model on every load costs
	// seconds, and a file whose size and mtime are unchanged has almost
	// certainly not changed.
	//
	// This is a deliberate trade. It catches corruption, truncation, a partial
	// re-download or a swapped file — the failures that actually happen. It
	// does not catch an edit that preserves both size and mtime. Anyone able to
	// do that can usually also rewrite this pin file, so full hashing every
	// load would buy little; set VerifyFull when that assumption does not hold.
	Stamps []stamp `json:"stamps,omitempty"`
}

type stamp struct {
	Path    string `json:"path"`
	Size    int64  `json:"size"`
	ModNano int64  `json:"mod_nano"`
}

func stampsFor(root string, files []string) []stamp {
	out := make([]stamp, 0, len(files))
	for _, f := range files {
		// Resolved under root, the way BuildManifest and VerifyTree resolve
		// them: statting a relative path here looked it up against the
		// process working directory instead, so the stamps described
		// whatever happened to be there.
		p := f
		if !filepath.IsAbs(p) {
			p = filepath.Join(root, p)
		}
		info, err := os.Stat(p)
		if err != nil {
			return nil // incomplete stamps are no stamps
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return nil
		}
		out = append(out, stamp{
			Path:    filepath.ToSlash(rel),
			Size:    info.Size(),
			ModNano: info.ModTime().UnixNano(),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// unchanged reports whether every stamped file still has its recorded size and
// modification time.
func (p *pin) unchanged(root string) bool {
	if len(p.Stamps) == 0 {
		return false
	}
	for _, st := range p.Stamps {
		info, err := os.Stat(filepath.Join(root, filepath.FromSlash(st.Path)))
		if err != nil {
			return false
		}
		if info.Size() != st.Size || info.ModTime().UnixNano() != st.ModNano {
			return false
		}
	}
	return true
}

func NewStore(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create pin store: %w", err)
	}
	return &Store{dir: dir}, nil
}

// path names a key's pin file. The readable part is lossy — "a/b", "a:b" and
// "a_b" all mapped to "a_b.pin.json", so one model or runtime could be
// verified against another's pin — so a digest of the full key is appended.
func (s *Store) path(key string) string {
	sum := sha256.Sum256([]byte(key))
	return filepath.Join(s.dir, safeName(key)+"-"+hex.EncodeToString(sum[:4])+".pin.json")
}

// legacyPath is the pre-digest name, still read so an upgrade does not treat
// every pinned model as unseen and re-pin whatever happens to be on disk.
func (s *Store) legacyPath(key string) string {
	return filepath.Join(s.dir, safeName(key)+".pin.json")
}

func safeName(key string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			return r
		default:
			return '_'
		}
	}, key)
}

// Pin records prepared inputs as the accepted state for key.
func (s *Store) Pin(key string, vi *VerifiedInput, manifest []byte, files ...string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, err := json.MarshalIndent(pin{
		Key:            key,
		Root:           vi.Root,
		ManifestSHA256: vi.ManifestSHA256,
		Revision:       vi.Revision,
		Manifest:       string(manifest),
		Stamps:         stampsFor(vi.Root, files),
	}, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path(key) + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, s.path(key)); err != nil {
		return err
	}
	_ = os.Remove(s.legacyPath(key)) // superseded by the digest-named pin
	return nil
}

// Unpin forgets the pin for key, so the next load re-prepares from disk.
func (s *Store) Unpin(key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	// A pin written by an older build lives under the legacy name; forgetting
	// one must forget both, or the old file would be read again.
	_ = os.Remove(s.legacyPath(key))
	err := os.Remove(s.path(key))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// get reads a key's pin. A missing pin is "not pinned yet"; anything else is
// an error. Treating a corrupt or unreadable pin as missing meant Prepare
// re-pinned whatever was on disk, which is exactly the silent re-blessing the
// pin exists to prevent.
func (s *Store) get(key string) (*pin, bool, error) {
	b, err := os.ReadFile(s.path(key))
	if os.IsNotExist(err) {
		b, err = os.ReadFile(s.legacyPath(key))
	}
	if os.IsNotExist(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("read pin for %s: %w", key, err)
	}
	var p pin
	if err := json.Unmarshal(b, &p); err != nil {
		return nil, false, fmt.Errorf("pin for %s is unreadable (run `mfsh repin %s` to accept the current contents): %w", key, key, err)
	}
	// Defence in depth against a name collision: a pin only speaks for the
	// key it was written for.
	if p.Key != "" && p.Key != key {
		return nil, false, fmt.Errorf("pin file for %s records key %q", key, p.Key)
	}
	return &p, true, nil
}

// Prepare returns verified inputs for key.
//
// First sight pins whatever is on disk. Every load after that is verified
// against the pinned manifest, so a changed file is rejected rather than
// silently re-blessed.
func (s *Store) Prepare(key, root string, files []string, revision string) (*VerifiedInput, []byte, error) {
	// One pin per key at a time: the existence check and the first-time
	// creation were separate steps, so two goroutines could both see "no pin"
	// and both write one, and the loser's verified inputs described a pin that
	// no longer existed.
	s.prep.Lock()
	defer s.prep.Unlock()
	p, ok, err := s.get(key)
	if err != nil {
		return nil, nil, err
	}
	if ok {
		manifest := []byte(p.Manifest)
		if !s.VerifyFull && p.unchanged(p.Root) {
			// Nothing has been touched since the pin, so the recorded digest
			// still describes what is on disk.
			return &VerifiedInput{
				Root:           p.Root,
				ManifestSHA256: p.ManifestSHA256,
				Revision:       p.Revision,
			}, manifest, nil
		}
		vi, err := VerifyInput(p.Root, manifest, p.ManifestSHA256, p.Revision)
		if err != nil {
			return nil, nil, fmt.Errorf("%w: %s (run `mfsh repin %s` to accept the new contents): %v",
				ErrChanged, key, key, err)
		}
		return vi, manifest, nil
	}

	vi, manifest, err := VerifyTree(root, files, revision)
	if err != nil {
		return nil, nil, err
	}
	if err := s.Pin(key, vi, manifest, files...); err != nil {
		return nil, nil, err
	}
	return vi, manifest, nil
}
