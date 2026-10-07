// Package oci extracts a file from a container image through the registry HTTP API.
// Index, manifest, and layer digests pin the contents; every downloaded blob is
// verified before accepting extracted data. No container runtime is required.
package oci

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

// Ref is an image pinned by digest: registry host, repository, digest.
type Ref struct {
	Registry   string // "ghcr.io"
	Repository string // "llm-d/llm-d-router-endpoint-picker"
	Digest     string // "sha256:…" of the image index (or single manifest)
}

var ErrDigest = errors.New("digest mismatch")

const (
	mtOCIIndex     = "application/vnd.oci.image.index.v1+json"
	mtOCIManifest  = "application/vnd.oci.image.manifest.v1+json"
	mtDockerList   = "application/vnd.docker.distribution.manifest.list.v2+json"
	mtDockerManif  = "application/vnd.docker.distribution.manifest.v2+json"
	acceptManifest = mtOCIIndex + ", " + mtOCIManifest + ", " + mtDockerList + ", " + mtDockerManif
)

type client struct {
	http  *http.Client
	ref   Ref
	token string
}

// Progress reports bytes read from layers while searching for the file.
type Progress func(done, total int64)

// ExtractFile writes the file at filePath (e.g. "app/epp") inside the image,
// for linux/arch, to dest with mode 0755. arch is a Go GOARCH.
func ExtractFile(ctx context.Context, ref Ref, arch, filePath, dest string, progress Progress) error {
	c := &client{http: &http.Client{Transport: &http.Transport{ResponseHeaderTimeout: 60 * time.Second}}, ref: ref}
	if err := c.auth(ctx); err != nil {
		return err
	}
	return extractWith(ctx, c, arch, filePath, dest, progress)
}

func extractWith(ctx context.Context, c *client, arch, filePath, dest string, progress Progress) error {
	ref := c.ref
	mt, body, err := c.manifest(ctx, ref.Digest)
	if err != nil {
		return err
	}
	if mt == mtOCIIndex || mt == mtDockerList {
		var idx struct {
			Manifests []struct {
				Digest   string `json:"digest"`
				Platform struct {
					OS           string `json:"os"`
					Architecture string `json:"architecture"`
				} `json:"platform"`
			} `json:"manifests"`
		}
		if err := json.Unmarshal(body, &idx); err != nil {
			return fmt.Errorf("image index: %w", err)
		}
		found := ""
		for _, m := range idx.Manifests {
			if m.Platform.OS == "linux" && m.Platform.Architecture == arch {
				found = m.Digest
				break
			}
		}
		if found == "" {
			return fmt.Errorf("image has no linux/%s build", arch)
		}
		if _, body, err = c.manifest(ctx, found); err != nil {
			return err
		}
	}
	var man struct {
		Layers []struct {
			Digest    string `json:"digest"`
			Size      int64  `json:"size"`
			MediaType string `json:"mediaType"`
		} `json:"layers"`
	}
	if err := json.Unmarshal(body, &man); err != nil {
		return fmt.Errorf("image manifest: %w", err)
	}
	var total int64
	for _, l := range man.Layers {
		total += l.Size
	}
	want := strings.TrimPrefix(path.Clean("/"+filePath), "/")
	// The topmost layer holding the file is the one the image runs, so search
	// from the top down and stop at the first hit.
	var done int64
	var skipped []string
	for i := len(man.Layers) - 1; i >= 0; i-- {
		l := man.Layers[i]
		// Only gzipped layers: searchLayer always wraps the body in a gzip
		// reader, so an uncompressed or zstd layer (both legal, and both
		// containing "tar" in their media type) failed as a corrupt archive
		// rather than being skipped or handled.
		if !strings.Contains(l.MediaType, "tar") {
			continue
		}
		if !strings.Contains(l.MediaType, "gzip") {
			skipped = append(skipped, l.MediaType)
			continue
		}
		found, deleted, err := c.searchLayer(ctx, l.Digest, want, dest, func(n int64) {
			if progress != nil {
				progress(done+n, total)
			}
		})
		done += l.Size
		if err != nil {
			return err
		}
		if found {
			return nil
		}
		if deleted {
			// A higher layer removed it, so a copy in a lower layer is not
			// part of this image.
			return fmt.Errorf("%s was removed by a later layer of the image", filePath)
		}
	}
	if len(skipped) > 0 {
		return fmt.Errorf("%s is not in the image; %d layer(s) were skipped because ModelFabric reads only gzipped layers (%s)",
			filePath, len(skipped), strings.Join(skipped, ", "))
	}
	return fmt.Errorf("%s is not in the image", filePath)
}

// auth gets an anonymous pull token, the way registries grant public reads.
func (c *client) auth(ctx context.Context) error {
	u := fmt.Sprintf("https://%s/token?scope=repository:%s:pull", c.ref.Registry, c.ref.Repository)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("registry token: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("registry token: %s", resp.Status)
	}
	var t struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&t); err != nil || t.Token == "" {
		return fmt.Errorf("registry token: no token in response")
	}
	c.token = t.Token
	return nil
}

func (c *client) get(ctx context.Context, kind, digest, accept string) (*http.Response, error) {
	u := fmt.Sprintf("https://%s/v2/%s/%s/%s", c.ref.Registry, c.ref.Repository, kind, digest)
	// Ref is exported, so a registry or repository with a space or a control
	// character reaches here; the discarded error left a nil request to
	// dereference on the next line.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, fmt.Errorf("image reference %s/%s: %w", c.ref.Registry, c.ref.Repository, err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("%s %s: %s", kind, digest, resp.Status)
	}
	return resp, nil
}

func (c *client) manifest(ctx context.Context, digest string) (string, []byte, error) {
	resp, err := c.get(ctx, "manifests", digest, acceptManifest)
	if err != nil {
		return "", nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return "", nil, err
	}
	if err := verify(digest, sha256Of(body)); err != nil {
		return "", nil, err
	}
	mt, _, _ := strings.Cut(resp.Header.Get("Content-Type"), ";")
	return strings.TrimSpace(mt), body, nil
}

// whiteoutOf reports whether name is an OCI/Docker whiteout that deletes want:
// ".wh.<base>" in want's directory, or an opaque ".wh..wh..opq" on any parent.
func whiteoutOf(name, want string) bool {
	dir, base := path.Split(name)
	switch {
	case base == ".wh..wh..opq":
		return want == path.Clean(dir) || strings.HasPrefix(want, path.Clean(dir)+"/") || dir == ""
	case strings.HasPrefix(base, ".wh."):
		return path.Join(dir, strings.TrimPrefix(base, ".wh.")) == want
	}
	return false
}

// searchLayer streams one layer, hashing it to completion even after finding
// the file; extracted bytes are kept only after the layer digest verifies.
// It reports whether it extracted the
// file, and separately whether the layer deletes it; a whiteout means the
// search must stop rather than continue into lower layers.
func (c *client) searchLayer(ctx context.Context, digest, want, dest string, progress func(int64)) (found, deleted bool, err error) {
	resp, err := c.get(ctx, "blobs", digest, "")
	if err != nil {
		return false, false, err
	}
	defer resp.Body.Close()
	h := sha256.New()
	counted := &countingReader{r: io.TeeReader(resp.Body, h), progress: progress}
	gz, err := gzip.NewReader(counted)
	if err != nil {
		return false, false, fmt.Errorf("layer %s: %w", digest, err)
	}
	// tmp is created, not named in advance: "<dest>.part" was predictable, and
	// opening it with O_CREATE|O_TRUNC followed a symlink pre-planted at that
	// path, so a writable cache directory let another process choose where
	// this write landed.
	var tmp string
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return false, false, fmt.Errorf("layer %s: %w", digest, err)
		}
		name := strings.TrimPrefix(path.Clean("/"+hdr.Name), "/")
		// A layer can delete a path instead of containing it. Ignoring these
		// markers meant the search carried on into lower layers and extracted
		// a file the image had removed.
		if whiteoutOf(name, want) {
			deleted = true
			continue
		}
		if name != want || hdr.Typeflag != tar.TypeReg {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return false, false, err
		}
		tf, err := os.CreateTemp(filepath.Dir(dest), filepath.Base(dest)+".part-*")
		if err != nil {
			return false, false, err
		}
		tmp = tf.Name()
		if err := tf.Chmod(0o755); err != nil {
			tf.Close()
			return false, false, err
		}
		f := tf
		if _, err := io.Copy(f, tr); err != nil {
			f.Close()
			return false, false, err
		}
		if err := f.Close(); err != nil {
			return false, false, err
		}
		found = true
	}
	// Drain what tar did not read so the whole blob is hashed.
	if _, err := io.Copy(io.Discard, counted); err != nil {
		return false, false, err
	}
	if err := verify(digest, h); err != nil {
		if tmp != "" {
			_ = os.Remove(tmp)
		}
		return false, false, err
	}
	if !found {
		return found, deleted, nil
	}
	return true, false, os.Rename(tmp, dest)
}

type countingReader struct {
	r        io.Reader
	n        int64
	progress func(int64)
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	if c.progress != nil && n > 0 {
		c.progress(c.n)
	}
	return n, err
}

func sha256Of(b []byte) hash.Hash {
	h := sha256.New()
	h.Write(b)
	return h
}

func verify(digest string, h hash.Hash) error {
	want, ok := strings.CutPrefix(digest, "sha256:")
	if !ok {
		return fmt.Errorf("unsupported digest %q", digest)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != want {
		return fmt.Errorf("%w: %s hashed to sha256:%s", ErrDigest, digest, got)
	}
	return nil
}
