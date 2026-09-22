// Package inputs prepares immutable runtime and model inputs, rejecting
// missing or changed files before an engine is ever launched.
//
// Inputs carry their content identity separately from their location:
//
//	VerifiedInput{root, manifest_sha256, revision}
//	OCIImageRef{reference, digest}
//	PreparedInputs{runtime, model}
//
// # Canonical manifest encoding
//
// The canonical encoding records sorted relative paths, file sizes and
// SHA-256 hashes so a manifest can be reproduced across implementations:
//
//		<sha256 hex>  <size decimal>  <relative path>\n
//
//	  - one entry per line, LF endings, UTF-8, no trailing blank line
//	  - exactly two spaces between the three fields
//	  - relative paths use forward slashes, are sorted bytewise ascending
//	  - SHA-256 is lowercase 64-character hex
//
// manifest_sha256 is the SHA-256 of those bytes, lowercase hex. In Python:
//
//	hashlib.sha256(manifest).hexdigest()
package inputs

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// ErrInputVerification covers digest mismatch, missing files, invalid paths and
// incomplete manifests — the spec's InputVerificationError.
var ErrInputVerification = errors.New("input verification failed")

// VerifiedInput is a content-verified local tree.
type VerifiedInput struct {
	Root           string `json:"root"`
	ManifestSHA256 string `json:"manifest_sha256"`
	Revision       string `json:"revision,omitempty"`
}

// OCIImageRef is an immutable image reference. It does not imply an extracted
// local runtime, and it is only ever a *runtime*, never a model.
type OCIImageRef struct {
	Reference string `json:"reference"`
	Digest    string `json:"digest"`
}

// PreparedInputs pairs a runtime with a model.
//
// Runtime and RuntimeImage are mutually exclusive. This native host uses
// Runtime; RuntimeImage represents content pinned by container digest.
// The model is always a VerifiedInput — never an image reference.
type PreparedInputs struct {
	Runtime      *VerifiedInput `json:"runtime,omitempty"`
	RuntimeImage *OCIImageRef   `json:"runtime_image,omitempty"`
	Model        VerifiedInput  `json:"model"`
}

// Validate enforces the runtime/model asymmetry.
func (p PreparedInputs) Validate() error {
	switch {
	case p.Runtime == nil && p.RuntimeImage == nil:
		return fmt.Errorf("%w: prepared inputs have no runtime", ErrInputVerification)
	case p.Runtime != nil && p.RuntimeImage != nil:
		return fmt.Errorf("%w: runtime is both a verified tree and an image reference", ErrInputVerification)
	case p.Model.ManifestSHA256 == "":
		return fmt.Errorf("%w: model is not verified", ErrInputVerification)
	case p.Model.Root == "":
		// Revalidate would call VerifyInput(""), which resolves to the
		// process working directory and verifies whatever happens to be there.
		return fmt.Errorf("%w: model has no root", ErrInputVerification)
	case p.Runtime != nil && p.Runtime.Root == "":
		return fmt.Errorf("%w: runtime has no root", ErrInputVerification)
	}
	return nil
}

type entry struct {
	sha  string
	size int64
	path string // relative, forward slashes
}

// BuildManifest hashes each file and returns the canonical manifest bytes.
// Paths are recorded relative to root.
func BuildManifest(root string, files []string) ([]byte, error) {
	if len(files) == 0 {
		return nil, fmt.Errorf("%w: manifest would be empty", ErrInputVerification)
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}

	entries := make([]entry, 0, len(files))
	for _, f := range files {
		abs := f
		if !filepath.IsAbs(abs) {
			abs = filepath.Join(absRoot, f)
		}
		rel, err := relativeTo(absRoot, abs)
		if err != nil {
			return nil, err
		}
		// Metadata and content come from one open file handle. Two pathname
		// lookups could describe two different objects if the file, or a
		// symlink to it, were replaced in between — a manifest recording one
		// file's size and another's digest.
		info, sum, err := statAndSHA256(abs)
		if err != nil {
			return nil, err
		}
		if info.IsDir() {
			return nil, fmt.Errorf("%w: %s is a directory", ErrInputVerification, rel)
		}
		// The manifest is line-oriented, so a name containing a newline or a
		// carriage return would be written as several records that could not
		// be parsed back — or could be made to parse as something else.
		if strings.ContainsAny(rel, "\n\r") {
			return nil, fmt.Errorf("%w: %q contains a line break", ErrInputVerification, rel)
		}
		entries = append(entries, entry{sha: sum, size: info.Size(), path: rel})
	}
	return encodeManifest(entries)
}

func encodeManifest(entries []entry) ([]byte, error) {
	sort.Slice(entries, func(i, j int) bool { return entries[i].path < entries[j].path })
	var buf bytes.Buffer
	for i, e := range entries {
		// Duplicate paths would make verification ambiguous: one entry could
		// pass while a conflicting one for the same file is never reached.
		if i > 0 && entries[i-1].path == e.path {
			return nil, fmt.Errorf("%w: duplicate manifest entry for %s", ErrInputVerification, e.path)
		}
		fmt.Fprintf(&buf, "%s  %d  %s\n", e.sha, e.size, e.path)
	}
	return buf.Bytes(), nil
}

func parseManifest(manifest []byte) ([]entry, error) {
	if len(bytes.TrimSpace(manifest)) == 0 {
		return nil, fmt.Errorf("%w: manifest is empty", ErrInputVerification)
	}
	var entries []entry
	seen := map[string]bool{}
	for i, line := range strings.Split(strings.TrimRight(string(manifest), "\n"), "\n") {
		parts := strings.SplitN(line, "  ", 3)
		if len(parts) != 3 {
			return nil, fmt.Errorf("%w: manifest line %d is malformed", ErrInputVerification, i+1)
		}
		if !isHex64(parts[0]) {
			return nil, fmt.Errorf("%w: manifest line %d has an invalid digest", ErrInputVerification, i+1)
		}
		size, err := strconv.ParseInt(parts[1], 10, 64)
		if err != nil || size < 0 {
			return nil, fmt.Errorf("%w: manifest line %d has an invalid size", ErrInputVerification, i+1)
		}
		if err := checkRelPath(parts[2]); err != nil {
			return nil, err
		}
		if seen[parts[2]] {
			return nil, fmt.Errorf("%w: duplicate manifest entry for %s", ErrInputVerification, parts[2])
		}
		seen[parts[2]] = true
		entries = append(entries, entry{sha: parts[0], size: size, path: parts[2]})
	}
	return entries, nil
}

// VerifyInput checks the manifest against its expected digest, then checks
// every file on disk against the manifest.
//
// Both halves matter: the digest proves the manifest is the one we expect, and
// the per-file check proves the tree still matches it.
func VerifyInput(root string, manifest []byte, expectedSHA256, revision string) (*VerifiedInput, error) {
	actual := sha256.Sum256(manifest)
	got := hex.EncodeToString(actual[:])
	if !strings.EqualFold(got, expectedSHA256) {
		return nil, fmt.Errorf("%w: manifest digest %s does not match expected %s",
			ErrInputVerification, got, strings.ToLower(expectedSHA256))
	}

	entries, err := parseManifest(manifest)
	if err != nil {
		return nil, err
	}

	absRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		path := filepath.Join(absRoot, filepath.FromSlash(e.path))
		info, err := os.Stat(path)
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%w: %s is missing", ErrInputVerification, e.path)
		}
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrInputVerification, err)
		}
		// Size is checked first because it is free and rejects most drift
		// before we pay to hash a multi-gigabyte file.
		if info.Size() != e.size {
			return nil, fmt.Errorf("%w: %s changed size (%d, expected %d)",
				ErrInputVerification, e.path, info.Size(), e.size)
		}
		sum, err := fileSHA256(path)
		if err != nil {
			return nil, err
		}
		if sum != e.sha {
			return nil, fmt.Errorf("%w: %s content changed", ErrInputVerification, e.path)
		}
	}

	return &VerifiedInput{
		Root:           absRoot,
		ManifestSHA256: strings.ToLower(expectedSHA256),
		Revision:       revision,
	}, nil
}

// Revalidate re-checks prepared inputs immediately before launch.
//
// Verification and startup are separate moments, and the spec is explicit that
// "a manifest must not bless mutable files that are replaced between
// verification and startup". Calling this at spawn time closes that window.
func (p PreparedInputs) Revalidate(manifests Manifests) error {
	if err := p.Validate(); err != nil {
		return err
	}
	if p.Runtime != nil {
		m, ok := manifests[p.Runtime.ManifestSHA256]
		if !ok {
			return fmt.Errorf("%w: no manifest retained for runtime %s",
				ErrInputVerification, p.Runtime.ManifestSHA256)
		}
		if _, err := VerifyInput(p.Runtime.Root, m, p.Runtime.ManifestSHA256, p.Runtime.Revision); err != nil {
			return fmt.Errorf("runtime changed since verification: %w", err)
		}
	}
	m, ok := manifests[p.Model.ManifestSHA256]
	if !ok {
		return fmt.Errorf("%w: no manifest retained for model %s",
			ErrInputVerification, p.Model.ManifestSHA256)
	}
	if _, err := VerifyInput(p.Model.Root, m, p.Model.ManifestSHA256, p.Model.Revision); err != nil {
		return fmt.Errorf("model changed since verification: %w", err)
	}
	return nil
}

// Manifests maps a manifest digest to the manifest bytes it names. Retaining
// the bytes is what makes launch-time revalidation possible.
type Manifests map[string][]byte

// VerifyTree is the convenience path for a tree we are adopting for the first
// time: build the manifest and return it alongside its digest.
//
// It does not then verify the tree against that manifest. Doing so would re-read
// and re-hash every file to confirm what BuildManifest just measured — provably
// redundant, and for a multi-gigabyte model it doubles the cost of every load.
func VerifyTree(root string, files []string, revision string) (*VerifiedInput, []byte, error) {
	manifest, err := BuildManifest(root, files)
	if err != nil {
		return nil, nil, err
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, nil, err
	}
	sum := sha256.Sum256(manifest)
	return &VerifiedInput{
		Root:           absRoot,
		ManifestSHA256: hex.EncodeToString(sum[:]),
		Revision:       revision,
	}, manifest, nil
}

func relativeTo(root, path string) (string, error) {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return "", fmt.Errorf("%w: %s is not under %s", ErrInputVerification, path, root)
	}
	rel = filepath.ToSlash(rel)
	if err := checkRelPath(rel); err != nil {
		return "", err
	}
	return rel, nil
}

// checkRelPath rejects anything that could escape the root or be interpreted
// differently by another implementation.
func checkRelPath(p string) error {
	switch {
	case p == "":
		return fmt.Errorf("%w: empty path in manifest", ErrInputVerification)
	case strings.HasPrefix(p, "/"), filepath.IsAbs(p):
		return fmt.Errorf("%w: absolute path %q in manifest", ErrInputVerification, p)
	case strings.Contains(p, `\`):
		return fmt.Errorf("%w: backslash in manifest path %q", ErrInputVerification, p)
	case p == "..", strings.HasPrefix(p, "../"), strings.Contains(p, "/../"), strings.HasSuffix(p, "/.."):
		return fmt.Errorf("%w: path %q escapes the root", ErrInputVerification, p)
	}
	return nil
}

func isHex64(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// statAndSHA256 returns a file's metadata and digest from a single open
// handle, so both describe the same object.
func statAndSHA256(path string) (os.FileInfo, string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, "", fmt.Errorf("%w: %v", ErrInputVerification, err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, "", fmt.Errorf("%w: %v", ErrInputVerification, err)
	}
	if info.IsDir() {
		return info, "", nil
	}
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return nil, "", fmt.Errorf("%w: %v", ErrInputVerification, err)
	}
	return info, hex.EncodeToString(h.Sum(nil)), nil
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrInputVerification, err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("%w: %v", ErrInputVerification, err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
