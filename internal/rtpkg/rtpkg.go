// Package rtpkg installs upstream llama.cpp releases under ModelFabric's runtime
// root, using LM Studio-compatible manifests and shared vendor libraries.
// Assets are SHA-256 verified. A local device probe records compatibility
// because upstream releases do not list supported GPU architectures.
package rtpkg

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"regexp"
	goruntime "runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gregbugaj/modelfabric/internal/runtime"
)

const DefaultRepo = "ggml-org/llama.cpp"

type Client struct {
	APIBase string // https://api.github.com
	Repo    string
	HTTP    *http.Client
	Token   string // optional GITHUB_TOKEN, only to lift the anonymous rate limit
}

func NewClient() *Client {
	return &Client{
		APIBase: "https://api.github.com",
		Repo:    DefaultRepo,
		HTTP:    &http.Client{Transport: &http.Transport{ResponseHeaderTimeout: 60 * time.Second}},
		Token:   os.Getenv("GITHUB_TOKEN"),
	}
}

type Asset struct {
	Name   string
	URL    string
	Size   int64
	SHA256 string // lowercase hex, from GitHub's published digest
}

// Build is one upstream release (b11040), with its assets by name.
type Build struct {
	Tag    string
	Number int
	Assets map[string]Asset
}

var buildTag = regexp.MustCompile(`^b(\d+)$`)

// Builds lists recent upstream builds, newest first.
func (c *Client) Builds(ctx context.Context, n int) ([]Build, error) {
	u := fmt.Sprintf("%s/repos/%s/releases?per_page=%d", c.APIBase, c.Repo, n)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("list %s releases: %w", c.Repo, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("list %s releases: %s", c.Repo, resp.Status)
	}
	var rels []struct {
		Tag    string `json:"tag_name"`
		Assets []struct {
			Name   string `json:"name"`
			URL    string `json:"browser_download_url"`
			Size   int64  `json:"size"`
			Digest string `json:"digest"`
		} `json:"assets"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&rels); err != nil {
		return nil, fmt.Errorf("list %s releases: %w", c.Repo, err)
	}
	var out []Build
	for _, r := range rels {
		// Build releases are tagged bNNNN; other tags (v0.4.1) are not engine
		// builds, which is why GitHub's "latest release" is the wrong query.
		m := buildTag.FindStringSubmatch(r.Tag)
		if m == nil {
			continue
		}
		num, _ := strconv.Atoi(m[1])
		b := Build{Tag: r.Tag, Number: num, Assets: map[string]Asset{}}
		for _, a := range r.Assets {
			sum, ok := strings.CutPrefix(a.Digest, "sha256:")
			if !ok {
				continue // no published digest: nothing to verify against
			}
			b.Assets[a.Name] = Asset{Name: a.Name, URL: a.URL, Size: a.Size, SHA256: strings.ToLower(sum)}
		}
		out = append(out, b)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Number > out[j].Number })
	return out, nil
}

// Want is what the operator asked for; zero values mean "choose".
type Want struct {
	Backend string // cpu | cuda | vulkan | rocm
	CUDA    string // "12.8"; for cuda, the toolkit version
	Build   string // "b11040"
}

type Plan struct {
	Build          string // b11040
	BuildNumber    int
	Backend        string // cpu | cuda | vulkan | rocm
	BackendVersion string // "12.8" for cuda, "10.0" for rocm
	Arch           string // x64 | arm64
	Engine         Asset
	Vendor         *Asset // the CUDA runtime, for cuda builds
	Name           string // package name, without version
	VendorName     string // vendor package directory name
	DisplayName    string // "CUDA 12.8 llama.cpp (upstream)"
	Reason         string // why this variant, for the operator
}

// Dir is the package directory name, <name>-<version> as LM Studio names them.
func (p *Plan) Dir() string { return p.Name + "-" + p.Build }

// RuntimeName is how the installed runtime is listed and selected.
func (p *Plan) RuntimeName() string { return p.Name + "@" + p.Build }

// TotalBytes is the download size, counting the vendor package.
func (p *Plan) TotalBytes() int64 {
	n := p.Engine.Size
	if p.Vendor != nil {
		n += p.Vendor.Size
	}
	return n
}

// assetPattern matches upstream's Linux archives:
// llama-b11040-bin-ubuntu-cuda-12.8-x64.tar.gz, llama-b11040-bin-ubuntu-x64.tar.gz.
var assetPattern = regexp.MustCompile(`^llama-(b\d+)-bin-ubuntu-(?:(cuda|rocm|vulkan)(?:-([0-9.]+))?-)?(x64|arm64)\.tar\.gz$`)

type variant struct {
	backend, version, arch string
	asset                  Asset
}

func variants(b Build, arch string) []variant {
	var out []variant
	for name, a := range b.Assets {
		m := assetPattern.FindStringSubmatch(name)
		if m == nil || m[1] != b.Tag || m[4] != arch {
			continue
		}
		backend := m[2]
		if backend == "" {
			backend = "cpu"
		}
		out = append(out, variant{backend: backend, version: m[3], arch: arch, asset: a})
	}
	return out
}

func hostArch() (string, error) {
	if goruntime.GOOS != "linux" {
		return "", fmt.Errorf("upstream Linux builds only; this is %s", goruntime.GOOS)
	}
	switch goruntime.GOARCH {
	case "amd64":
		return "x64", nil
	case "arm64":
		return "arm64", nil
	}
	return "", fmt.Errorf("no upstream build for %s", goruntime.GOARCH)
}

func autoBackend(hw runtime.Hardware) (string, string) {
	switch {
	case len(hw.GPUs) > 0: // the survey finds GPUs through nvidia-smi
		return "cuda", fmt.Sprintf("%s found", hw.GPUs[0].Name)
	case hw.Vulkan:
		return "vulkan", "Vulkan driver found, no NVIDIA GPU"
	}
	return "cpu", "no GPU found"
}

// Choose resolves a Want against upstream builds and this machine.
func Choose(builds []Build, want Want, hw runtime.Hardware) (*Plan, error) {
	arch, err := hostArch()
	if err != nil {
		return nil, err
	}
	backend, reason := strings.ToLower(want.Backend), ""
	if backend == "" {
		backend, reason = autoBackend(hw)
	} else {
		reason = map[string]string{
			"cpu":    "runs without a GPU",
			"cuda":   "NVIDIA GPUs",
			"vulkan": "any GPU with a Vulkan driver",
			"rocm":   "AMD GPUs through ROCm",
		}[backend]
	}
	switch backend {
	case "cpu", "cuda", "vulkan", "rocm":
	default:
		return nil, fmt.Errorf("unknown backend %q (want cpu, cuda, vulkan or rocm)", backend)
	}

	driverCUDA := cudaCode(hw.CUDAVersion)
	if backend == "cuda" && want.CUDA == "" && driverCUDA == 0 {
		return nil, fmt.Errorf("cannot read which CUDA version the driver supports; pass -cuda (e.g. -cuda 12.8)")
	}

	for _, b := range builds {
		if want.Build != "" && b.Tag != want.Build {
			continue
		}
		var best *variant
		for _, v := range variants(b, arch) {
			if v.backend != backend {
				continue
			}
			if backend == "cuda" {
				switch {
				case want.CUDA != "":
					if v.version != want.CUDA {
						continue
					}
				case cudaCode(v.version) > driverCUDA:
					// A CUDA runtime newer than the driver supports fails at
					// startup; the newest one it does support is the pick.
					continue
				}
			}
			if best == nil || cudaCode(v.version) > cudaCode(best.version) {
				vv := v
				best = &vv
			}
		}
		if best == nil {
			continue
		}
		p := &Plan{
			Build: b.Tag, BuildNumber: b.Number, Backend: backend, BackendVersion: best.version,
			Arch: arch, Engine: best.asset, Reason: reason,
		}
		p.DisplayName = displayName(backend, best.version)
		platform := map[string]string{"x64": "x86_64", "arm64": "arm64"}[arch]
		p.Name = "llama.cpp-upstream-linux-" + platform + "-" + backend
		if best.version != "" {
			p.Name += "-" + best.version
		}
		if backend == "cuda" {
			// The engine links libcudart/libcublas, which upstream ships as a
			// separate archive; LM Studio's vendor package, by another name.
			name := fmt.Sprintf("cudart-llama-%s-bin-ubuntu-cuda-%s-%s.tar.gz", b.Tag, best.version, arch)
			va, ok := b.Assets[name]
			if !ok {
				continue // a release still uploading; an older build is complete
			}
			p.Vendor = &va
			p.VendorName = fmt.Sprintf("cudart-%s-%s", best.version, arch)
			if want.CUDA == "" {
				p.Reason += fmt.Sprintf("; CUDA %s is the newest build the driver (CUDA %s) supports", best.version, hw.CUDAVersion)
			}
		}
		return p, nil
	}
	if want.Build != "" {
		return nil, fmt.Errorf("no %s build for %s/%s in upstream release %s", backend, "linux", arch, want.Build)
	}
	return nil, fmt.Errorf("no complete %s build for linux/%s among the recent upstream releases", backend, arch)
}

func displayName(backend, version string) string {
	label := map[string]string{"cpu": "CPU", "cuda": "CUDA", "vulkan": "Vulkan", "rocm": "ROCm"}[backend]
	if version != "" {
		label += " " + version
	}
	return label + " llama.cpp (upstream)"
}

// cudaCode turns "12.8" into 12080, the encoding LM Studio's manifests use.
func cudaCode(v string) int {
	major, minor, ok := strings.Cut(v, ".")
	if !ok {
		return 0
	}
	a, err1 := strconv.Atoi(major)
	b, err2 := strconv.Atoi(minor)
	if err1 != nil || err2 != nil {
		return 0
	}
	return a*1000 + b*10
}
