package rtpkg

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/gregbugaj/modelfabric/internal/download"
	"github.com/gregbugaj/modelfabric/internal/runtime"
)

// ErrChecksum means a download did not match its published digest.
var ErrChecksum = errors.New("checksum mismatch")

// Progress reports download progress for one file.
type Progress func(file string, done, total int64)

// Install downloads, verifies, extracts and proves a plan, then publishes it
// into root atomically: the package directory appears only once everything,
// including the device probe, has succeeded. It returns the package dir.
func (c *Client) Install(ctx context.Context, p *Plan, root string, hw runtime.Hardware, progress Progress) (string, error) {
	final := filepath.Join(root, p.Dir())
	if _, err := os.Stat(final); err == nil {
		return "", fmt.Errorf("%s is already installed", p.RuntimeName())
	}
	downloads := filepath.Join(root, ".downloads")
	if err := os.MkdirAll(downloads, 0o755); err != nil {
		return "", err
	}

	var vendorDir string
	if p.Vendor != nil {
		vendorDir = filepath.Join(root, "vendor", p.VendorName)
		if _, err := os.Stat(filepath.Join(vendorDir, vendorMarker)); err != nil {
			if err := c.installArchive(ctx, *p.Vendor, downloads, vendorDir, progress); err != nil {
				return "", fmt.Errorf("CUDA runtime: %w", err)
			}
			if err := writeJSON(filepath.Join(vendorDir, vendorMarker), map[string]string{
				"source": p.Vendor.URL, "sha256": p.Vendor.SHA256,
				"installed": time.Now().UTC().Format(time.RFC3339),
			}); err != nil {
				return "", err
			}
		}
	}

	staging := filepath.Join(root, ".staging-"+p.Dir())
	_ = os.RemoveAll(staging)
	if err := c.fetchAndExtract(ctx, p.Engine, downloads, staging, progress); err != nil {
		_ = os.RemoveAll(staging)
		return "", err
	}
	defer os.RemoveAll(staging) // a no-op once renamed into place

	if _, err := os.Stat(filepath.Join(staging, "llama-server")); err != nil {
		return "", fmt.Errorf("%s contains no llama-server", p.Engine.Name)
	}
	devices, err := Probe(ctx, staging, vendorDir, p.Backend, hw)
	if err != nil {
		return "", err
	}
	if err := writeManifests(staging, p, devices); err != nil {
		return "", err
	}
	if err := os.Rename(staging, final); err != nil {
		return "", err
	}
	_ = os.Remove(filepath.Join(downloads, p.Engine.Name))
	if p.Vendor != nil {
		_ = os.Remove(filepath.Join(downloads, p.Vendor.Name))
	}
	return final, nil
}

const vendorMarker = "mfsh-vendor.json"

// installArchive fetches an archive into dest, atomically.
func (c *Client) installArchive(ctx context.Context, a Asset, downloads, dest string, progress Progress) error {
	staging := dest + ".staging"
	_ = os.RemoveAll(staging)
	if err := c.fetchAndExtract(ctx, a, downloads, staging, progress); err != nil {
		_ = os.RemoveAll(staging)
		return err
	}
	_ = os.RemoveAll(dest)
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	return os.Rename(staging, dest)
}

func (c *Client) fetchAndExtract(ctx context.Context, a Asset, downloads, dest string, progress Progress) error {
	archive := filepath.Join(downloads, a.Name)
	if err := c.download(ctx, a, archive, progress); err != nil {
		return err
	}
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer f.Close()
	return extractTarGz(f, dest)
}

// download fetches an asset, resuming a partial file, and verifies the
// published SHA-256 before the file is used at all.
func (c *Client) download(ctx context.Context, a Asset, dest string, progress Progress) error {
	if a.SHA256 == "" {
		return fmt.Errorf("%s has no published digest; refusing an unverifiable download", a.Name)
	}
	// A complete file from an earlier interrupted install is reused if it
	// still verifies.
	if sum, err := fileSHA256(dest); err == nil && sum == a.SHA256 {
		return nil
	}
	part := dest + ".part"
	h := sha256.New()
	var have int64
	if f, err := os.Open(part); err == nil {
		have, err = io.Copy(h, f)
		f.Close()
		if err != nil {
			return fmt.Errorf("read partial download: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("open partial download: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.URL, nil)
	if err != nil {
		return err
	}
	if have > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", have))
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("download %s: %w", a.Name, err)
	}
	defer resp.Body.Close()
	flags := os.O_CREATE | os.O_WRONLY
	switch resp.StatusCode {
	case http.StatusPartialContent:
		if err := download.ValidateRange(resp, have, a.Size); err != nil {
			return err
		}
		flags |= os.O_APPEND
	case http.StatusOK:
		// The server ignored the range: start over.
		flags |= os.O_TRUNC
		h, have = sha256.New(), 0
	default:
		return fmt.Errorf("download %s: %s", a.Name, resp.Status)
	}
	out, err := os.OpenFile(part, flags, 0o644)
	if err != nil {
		return err
	}
	buf := make([]byte, 1<<20)
	done := have
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if _, err := out.Write(buf[:n]); err != nil {
				out.Close()
				return err
			}
			h.Write(buf[:n])
			done += int64(n)
			if progress != nil {
				progress(a.Name, done, a.Size)
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			out.Close()
			return fmt.Errorf("download %s: %w", a.Name, rerr)
		}
	}
	if err := out.Close(); err != nil {
		return err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != a.SHA256 {
		_ = os.Remove(part)
		return fmt.Errorf("%w: %s is %s, upstream published %s", ErrChecksum, a.Name, got, a.SHA256)
	}
	return os.Rename(part, dest)
}

func fileSHA256(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// extractTarGz unpacks an archive whose entries share one top-level
// directory, dropping that directory. Anything that would land outside dest —
// absolute paths, "..", symlinks pointing out — is rejected, not skipped: an
// archive that tries is not one to install.
func extractTarGz(r io.Reader, dest string) error {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return err
	}
	defer gz.Close()
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return err
	}
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		name := filepath.ToSlash(filepath.Clean(hdr.Name))
		// Check before stripping the top directory: "top/../../x" cleans to
		// "../x", whose first component is not a directory to drop.
		if escapes(name) {
			return fmt.Errorf("archive entry %q escapes the package", hdr.Name)
		}
		if _, rest, ok := strings.Cut(name, "/"); ok {
			name = rest
		} else {
			continue // the top-level directory itself
		}
		if escapes(name) {
			return fmt.Errorf("archive entry %q escapes the package", hdr.Name)
		}
		target := filepath.Join(dest, filepath.FromSlash(name))
		// The path check above only proves the entry names a place inside
		// dest. If a component of that path is already a symlink — a
		// destination that is not freshly created, say "bin" pointing
		// somewhere else — the write follows it straight back out.
		if err := noSymlinkUnder(dest, target); err != nil {
			return err
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(hdr.Mode)&0o755|0o644)
			if err != nil {
				return err
			}
			if _, err := io.Copy(f, tr); err != nil {
				f.Close()
				return err
			}
			if err := f.Close(); err != nil {
				return err
			}
		case tar.TypeSymlink:
			link := hdr.Linkname
			resolved := filepath.Clean(filepath.Join(filepath.Dir(name), link))
			if filepath.IsAbs(link) || escapes(filepath.ToSlash(resolved)) {
				return fmt.Errorf("archive symlink %q -> %q escapes the package", hdr.Name, link)
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			if err := os.Symlink(link, target); err != nil {
				return err
			}
		default:
			// Hard links, devices and the like have no place in an engine build.
			return fmt.Errorf("archive entry %q has unsupported type %c", hdr.Name, hdr.Typeflag)
		}
	}
}

// escapes reports whether a cleaned, slash-separated path leaves its root.
// noSymlinkUnder refuses to write through a symlinked component between dest
// and target. It is the difference between "this entry names a path inside the
// package" and "this write lands inside the package".
func noSymlinkUnder(dest, target string) error {
	rel, err := filepath.Rel(dest, target)
	if err != nil {
		return fmt.Errorf("%q is not under %q: %w", target, dest, err)
	}
	cur := dest
	for _, part := range strings.Split(filepath.ToSlash(rel), "/") {
		if part == "" || part == "." {
			continue
		}
		cur = filepath.Join(cur, part)
		fi, err := os.Lstat(cur)
		if os.IsNotExist(err) {
			return nil // nothing here yet, so nothing to follow
		}
		if err != nil {
			return err
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("refusing to extract through the symlink %q", cur)
		}
	}
	return nil
}

func escapes(p string) bool {
	return strings.HasPrefix(p, "/") || p == ".." || strings.HasPrefix(p, "../")
}

var devicePattern = regexp.MustCompile(`^\s*(CUDA|ROCm|Vulkan)\d+:\s*(.+)$`)

// Probe proves a package runs here: the engine must start, and for a GPU
// backend enumerate at least one device of that kind — and every GPU the
// survey found, so a build compiled without this card's architecture is
// caught at install rather than at the first load. It returns the devices.
func Probe(ctx context.Context, pkgDir, vendorDir, backend string, hw runtime.Hardware) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	run := func(args ...string) (string, error) {
		cmd := exec.CommandContext(ctx, filepath.Join(pkgDir, "llama-server"), args...)
		cmd.Dir = pkgDir
		cmd.Env = os.Environ()
		if vendorDir != "" {
			cmd.Env = append(cmd.Env, "LD_LIBRARY_PATH="+vendorDir)
		}
		var out bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &out
		err := cmd.Run()
		return out.String(), err
	}
	if out, err := run("--version"); err != nil {
		return nil, fmt.Errorf("the engine does not start on this machine: %v\n%s", err, tail(out, 12))
	}
	if backend == "cpu" {
		return nil, nil
	}
	out, err := run("--list-devices")
	if err != nil {
		return nil, fmt.Errorf("the engine could not list devices: %v\n%s", err, tail(out, 12))
	}
	want := map[string]string{"cuda": "CUDA", "rocm": "ROCm", "vulkan": "Vulkan"}[backend]
	var devices []string
	for _, line := range strings.Split(out, "\n") {
		if m := devicePattern.FindStringSubmatch(line); m != nil && m[1] == want {
			devices = append(devices, strings.TrimSpace(line))
		}
	}
	if len(devices) == 0 {
		return nil, fmt.Errorf("the %s build found no %s device on this machine:\n%s", backend, want, tail(out, 12))
	}
	if backend == "cuda" {
		for _, g := range hw.GPUs {
			found := false
			for _, d := range devices {
				found = found || strings.Contains(d, g.Name)
			}
			if !found {
				return nil, fmt.Errorf("the build does not see %s; it may not be compiled for compute %s", g.Name, g.ComputeCap)
			}
		}
	}
	return devices, nil
}

func tail(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// writeManifests writes LM Studio's two package files, so ModelFabric's own reader
// — the same one that reads LM Studio's packages — discovers this one.
func writeManifests(dir string, p *Plan, devices []string) error {
	platform := map[string]string{"x64": "x86_64", "arm64": "arm64"}[p.Arch]
	gpu := map[string]any{}
	switch p.Backend {
	case "cuda":
		gpu = map[string]any{"make": "Nvidia", "framework": "CUDA", "targets": []string{},
			"minimum_driver_version": fmt.Sprint(cudaCode(p.BackendVersion))}
	case "rocm":
		gpu = map[string]any{"make": "AMD", "framework": "ROCm", "targets": []string{}}
	case "vulkan":
		gpu = map[string]any{"framework": "Vulkan", "targets": []string{}}
	}
	var vendors []string
	if p.VendorName != "" {
		vendors = []string{p.VendorName}
	}
	manifest := map[string]any{
		"name":           p.Name,
		"version":        p.Build,
		"domains":        []string{"llm", "embedding"},
		"engine":         "llama.cpp",
		"extension_type": "engine",
		"platform":       "linux",
		// Upstream CPU builds carry a backend per instruction set and pick one
		// at runtime, so there is no single extension to require.
		"cpu":                      map[string]any{"architecture": platform, "instruction_set_extensions": []string{}},
		"gpu":                      gpu,
		"supported_model_formats":  []string{"gguf"},
		"manifest_version":         "4",
		"vendor_lib_package_names": vendors,
		"engine_protocol_server":   map[string]string{"runtime_kind": "llama-server", "executable_relative_path": "llama-server"},
		"modelfabric": runtime.PackageProvenance{
			Source: p.Engine.URL, SHA256: p.Engine.SHA256, Build: p.Build,
			Installed:     time.Now().UTC().Format(time.RFC3339),
			ProbedDevices: devices,
		},
	}
	if err := writeJSON(filepath.Join(dir, "backend-manifest.json"), manifest); err != nil {
		return err
	}
	// LM Studio's display-data.json, likewise: the name people read, and
	// release notes naming the upstream build.
	display := [][]any{{"en", map[string]any{
		"langKey":     "en",
		"displayName": p.DisplayName,
		"description": "Upstream llama.cpp " + p.Build + ", installed by ModelFabric",
		"releaseNotes": []map[string]string{{
			"version": p.Build, "releaseNotes": "- llama.cpp release " + p.Build + " (upstream)\n",
		}},
	}}}
	if err := writeJSON(filepath.Join(dir, "display-data.json"), display); err != nil {
		return err
	}

	type file struct {
		RelativePath string `json:"relative_path"`
		Executable   bool   `json:"executable"`
	}
	var files []file
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(dir, path)
		if rel == "backend-manifest.json" || rel == "display-data.json" {
			return nil
		}
		info, err := os.Stat(path) // follows symlinks: the bytes that run
		if err != nil {
			return err
		}
		files = append(files, file{RelativePath: filepath.ToSlash(rel), Executable: info.Mode()&0o111 != 0})
		return nil
	})
	if err != nil {
		return err
	}
	sort.Slice(files, func(i, j int) bool { return files[i].RelativePath < files[j].RelativePath })
	return writeJSON(filepath.Join(dir, "engine-protocol-server-artifacts.json"), map[string]any{
		"runtime_kind": "llama-server", "executable_relative_path": "llama-server", "files": files,
	})
}

func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}

// Remove deletes a ModelFabric-installed package, then any vendor package no
// remaining package needs. It refuses anything outside root, which is what
// keeps it from ever touching LM Studio's packages.
func Remove(root, pkgDir string) error {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	absPkg, err := filepath.Abs(pkgDir)
	if err != nil {
		return err
	}
	base := filepath.Base(absPkg)
	if filepath.Dir(absPkg) != absRoot || base == "vendor" || strings.HasPrefix(base, ".") {
		return fmt.Errorf("%s is not a ModelFabric-installed runtime (not a package under %s)", pkgDir, root)
	}
	if _, err := os.Stat(filepath.Join(absPkg, "backend-manifest.json")); err != nil {
		return fmt.Errorf("%s is not a runtime package: no backend-manifest.json", pkgDir)
	}
	if err := os.RemoveAll(absPkg); err != nil {
		return err
	}
	return pruneVendors(absRoot)
}

func pruneVendors(root string) error {
	needed := map[string]bool{}
	entries, _ := os.ReadDir(root)
	for _, e := range entries {
		raw, err := os.ReadFile(filepath.Join(root, e.Name(), "backend-manifest.json"))
		if err != nil {
			continue
		}
		var m struct {
			Vendors []string `json:"vendor_lib_package_names"`
		}
		if json.Unmarshal(raw, &m) == nil {
			for _, v := range m.Vendors {
				needed[v] = true
			}
		}
	}
	vendors, _ := os.ReadDir(filepath.Join(root, "vendor"))
	for _, v := range vendors {
		if !needed[v.Name()] {
			if err := os.RemoveAll(filepath.Join(root, "vendor", v.Name())); err != nil {
				return err
			}
		}
	}
	return nil
}

// Download fetches an asset to dest, resumably, verifying its SHA-256 before
// the file is kept. Shared with other verified downloads (the uv bootstrap).
func Download(ctx context.Context, a Asset, dest string, progress Progress) error {
	return NewClient().download(ctx, a, dest, progress)
}
