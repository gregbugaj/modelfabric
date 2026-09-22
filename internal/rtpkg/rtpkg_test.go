package rtpkg

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"testing"

	"github.com/gregbugaj/modelfabric/internal/runtime"
)

func asset(name string) Asset {
	return Asset{Name: name, URL: "https://x/" + name, Size: 1, SHA256: "ab"}
}

func build(tag string, num int, names ...string) Build {
	b := Build{Tag: tag, Number: num, Assets: map[string]Asset{}}
	for _, n := range names {
		b.Assets[n] = asset(n)
	}
	return b
}

func skipUnlessLinuxAMD64(t *testing.T) {
	if goruntime.GOOS != "linux" || goruntime.GOARCH != "amd64" {
		t.Skip("variant choice is tested for linux/amd64")
	}
}

var nvidia = runtime.Hardware{
	GPUs:        []runtime.GPU{{Name: "NVIDIA GeForce RTX 5090", ComputeCap: "12.0"}},
	CUDAVersion: "13.0",
}

func full(tag string, num int) Build {
	return build(tag, num,
		"llama-"+tag+"-bin-ubuntu-x64.tar.gz",
		"llama-"+tag+"-bin-ubuntu-vulkan-x64.tar.gz",
		"llama-"+tag+"-bin-ubuntu-cuda-12.8-x64.tar.gz",
		"cudart-llama-"+tag+"-bin-ubuntu-cuda-12.8-x64.tar.gz",
		"llama-"+tag+"-bin-ubuntu-cuda-13.3-x64.tar.gz",
		"cudart-llama-"+tag+"-bin-ubuntu-cuda-13.3-x64.tar.gz",
		"llama-"+tag+"-bin-ubuntu-cuda-12.8-arm64.tar.gz",
	)
}

// The newest CUDA build the driver supports: a 13.0 driver cannot run 13.3.
func TestChooseCapsCUDAAtTheDriver(t *testing.T) {
	skipUnlessLinuxAMD64(t)
	p, err := Choose([]Build{full("b200", 200)}, Want{}, nvidia)
	if err != nil {
		t.Fatal(err)
	}
	if p.Backend != "cuda" || p.BackendVersion != "12.8" || p.Vendor == nil || p.VendorName != "cudart-12.8-x64" {
		t.Fatalf("plan = %+v", p)
	}
	if p.RuntimeName() != "llama.cpp-upstream-linux-x86_64-cuda-12.8@b200" {
		t.Fatalf("name = %s", p.RuntimeName())
	}
	newer := nvidia
	newer.CUDAVersion = "13.4"
	if p, _ := Choose([]Build{full("b200", 200)}, Want{}, newer); p.BackendVersion != "13.3" {
		t.Fatalf("a 13.4 driver should get 13.3, got %s", p.BackendVersion)
	}
}

// A release still uploading (engine present, CUDA runtime not yet) is passed
// over for the newest complete one.
func TestChooseSkipsIncompleteRelease(t *testing.T) {
	skipUnlessLinuxAMD64(t)
	partial := build("b201", 201, "llama-b201-bin-ubuntu-cuda-12.8-x64.tar.gz")
	p, err := Choose([]Build{partial, full("b200", 200)}, Want{}, nvidia)
	if err != nil || p.Build != "b200" {
		t.Fatalf("plan = %+v, err = %v", p, err)
	}
}

func TestChooseBackends(t *testing.T) {
	skipUnlessLinuxAMD64(t)
	builds := []Build{full("b200", 200)}
	if p, _ := Choose(builds, Want{}, runtime.Hardware{Vulkan: true}); p.Backend != "vulkan" || p.Vendor != nil {
		t.Fatalf("vulkan plan = %+v", p)
	}
	if p, _ := Choose(builds, Want{}, runtime.Hardware{}); p.Backend != "cpu" || p.Name != "llama.cpp-upstream-linux-x86_64-cpu" {
		t.Fatalf("cpu plan = %+v", p)
	}
	if p, _ := Choose(builds, Want{Backend: "cpu"}, nvidia); p.Backend != "cpu" {
		t.Fatalf("explicit backend ignored: %+v", p)
	}
	if _, err := Choose(builds, Want{}, runtime.Hardware{GPUs: nvidia.GPUs}); err == nil {
		t.Fatal("chose a CUDA build without knowing the driver's CUDA version")
	}
	if p, err := Choose(builds, Want{CUDA: "12.8"}, runtime.Hardware{GPUs: nvidia.GPUs}); err != nil || p.BackendVersion != "12.8" {
		t.Fatalf("explicit -cuda: %+v %v", p, err)
	}
	if _, err := Choose(builds, Want{Build: "b999"}, nvidia); err == nil {
		t.Fatal("accepted a build that does not exist")
	}
}

type entry struct {
	name, body, link string
	typ              byte
}

func tarGz(t *testing.T, entries ...entry) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		hdr := &tar.Header{Name: e.name, Typeflag: e.typ, Mode: 0o755, Linkname: e.link, Size: int64(len(e.body))}
		if e.typ != tar.TypeReg {
			hdr.Size = 0
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if e.typ == tar.TypeReg {
			tw.Write([]byte(e.body))
		}
	}
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

func TestExtractStripsTopDirAndKeepsLinks(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "pkg")
	data := tarGz(t,
		entry{name: "llama-b1/", typ: tar.TypeDir},
		entry{name: "llama-b1/llama-server", body: "bin", typ: tar.TypeReg},
		entry{name: "llama-b1/libx.so.0.1", body: "lib", typ: tar.TypeReg},
		entry{name: "llama-b1/libx.so.0", link: "libx.so.0.1", typ: tar.TypeSymlink},
	)
	if err := extractTarGz(bytes.NewReader(data), dest); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dest, "libx.so.0")); string(b) != "lib" {
		t.Fatalf("symlink not preserved: %q", b)
	}
	if _, err := os.Stat(filepath.Join(dest, "llama-server")); err != nil {
		t.Fatal("top-level directory not stripped")
	}
}

func TestExtractRejectsEscapes(t *testing.T) {
	for _, e := range []entry{
		{name: "top/../../evil", body: "x", typ: tar.TypeReg},
		{name: "top/link", link: "../../etc/passwd", typ: tar.TypeSymlink},
		{name: "top/abs", link: "/etc/passwd", typ: tar.TypeSymlink},
		{name: "top/hard", link: "top/x", typ: tar.TypeLink},
	} {
		if err := extractTarGz(bytes.NewReader(tarGz(t, e)), t.TempDir()); err == nil {
			t.Errorf("accepted %s -> %s", e.name, e.link)
		}
	}
}

func TestDownloadVerifiesPublishedDigest(t *testing.T) {
	body := []byte("engine bytes")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.Write(body) }))
	defer srv.Close()
	c := &Client{HTTP: srv.Client()}
	sum := sha256.Sum256(body)
	dest := filepath.Join(t.TempDir(), "a.tar.gz")

	good := Asset{Name: "a.tar.gz", URL: srv.URL, Size: int64(len(body)), SHA256: hex.EncodeToString(sum[:])}
	if err := c.download(context.Background(), good, dest, nil); err != nil {
		t.Fatal(err)
	}
	bad := good
	bad.SHA256 = strings.Repeat("0", 64)
	os.Remove(dest)
	if err := c.download(context.Background(), bad, dest, nil); !errors.Is(err, ErrChecksum) {
		t.Fatalf("err = %v, want ErrChecksum", err)
	}
	if _, err := os.Stat(dest); err == nil {
		t.Fatal("a file that failed verification was kept")
	}
	bad.SHA256 = ""
	if err := c.download(context.Background(), bad, dest, nil); err == nil {
		t.Fatal("downloaded without a digest to verify")
	}
}

// What ModelFabric writes, ModelFabric's shared package reader reads: same schema as LM
// Studio's, with the probe standing in for declared GPU targets.
func TestManifestRoundTripsThroughPackageDiscovery(t *testing.T) {
	root := t.TempDir()
	p := &Plan{Build: "b200", BuildNumber: 200, Backend: "cuda", BackendVersion: "12.8", Arch: "x64",
		Engine: asset("e"), Name: "llama.cpp-upstream-linux-x86_64-cuda-12.8", VendorName: "cudart-12.8-x64"}
	dir := filepath.Join(root, p.Dir())
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "llama-server"), []byte("x"), 0o755)
	os.MkdirAll(filepath.Join(root, "vendor", "cudart-12.8-x64"), 0o755)
	devices := []string{"CUDA0: NVIDIA GeForce RTX 5090 (32086 MiB, 29758 MiB free)"}
	if err := writeManifests(dir, p, devices); err != nil {
		t.Fatal(err)
	}

	defs, err := runtime.DiscoverPackages(root, runtime.OriginUpstream)
	if err != nil || len(defs) != 1 {
		t.Fatalf("discovered %d, err %v", len(defs), err)
	}
	d := defs[0]
	if d.Name != p.RuntimeName() || d.LlamaBuild != 200 || d.Backend != "cuda" {
		t.Fatalf("definition = %+v", d)
	}
	if !strings.Contains(d.Env["LD_LIBRARY_PATH"], "cudart-12.8-x64") {
		t.Fatalf("vendor not on the library path: %v", d.Env)
	}
	if fit, why := d.Check(nvidia); fit != runtime.FitYes {
		t.Fatalf("fit = %s %v; the probe found this GPU", fit, why)
	}
	other := nvidia
	other.GPUs = []runtime.GPU{{Name: "NVIDIA RTX 6000 Ada Generation", ComputeCap: "8.9"}}
	if fit, _ := d.Check(other); fit != runtime.FitUnknown {
		t.Fatalf("fit on an unprobed GPU = %s, want unknown", fit)
	}
	old := nvidia
	old.CUDAVersion = "12.4"
	if fit, _ := d.Check(old); fit != runtime.FitNo {
		t.Fatalf("a 12.4 driver fits a CUDA 12.8 build: %s", fit)
	}
}

func TestRemoveStaysInsideRootAndPrunesVendors(t *testing.T) {
	root := t.TempDir()
	mk := func(pkg, vendor string) {
		dir := filepath.Join(root, pkg)
		os.MkdirAll(dir, 0o755)
		os.WriteFile(filepath.Join(dir, "backend-manifest.json"),
			[]byte(`{"vendor_lib_package_names":["`+vendor+`"]}`), 0o644)
		os.MkdirAll(filepath.Join(root, "vendor", vendor), 0o755)
	}
	mk("a-b1", "cudart-12.8-x64")
	mk("b-b2", "cudart-12.8-x64")
	mk("c-b3", "cudart-13.3-x64")

	outside := t.TempDir()
	if err := Remove(root, outside); err == nil {
		t.Fatal("removed a directory outside the runtimes root")
	}
	if err := Remove(root, filepath.Join(root, "vendor")); err == nil {
		t.Fatal("removed the vendor directory as if it were a package")
	}

	if err := Remove(root, filepath.Join(root, "a-b1")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "vendor", "cudart-12.8-x64")); err != nil {
		t.Fatal("pruned a vendor package another runtime still needs")
	}
	if err := Remove(root, filepath.Join(root, "c-b3")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "vendor", "cudart-13.3-x64")); err == nil {
		t.Fatal("kept a vendor package nothing needs")
	}
}
