package runtime

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// LM Studio packages declare their launch contract in backend-manifest.json:
// engine_protocol_server names the executable and vendor_lib_package_names
// lists shared dependencies. engine-protocol-server-artifacts.json lists files
// for verification. Pinning the engine directory alone excludes vendor libraries.

type backendManifest struct {
	Name     string   `json:"name"`
	Engine   string   `json:"engine"`
	Version  string   `json:"version"`
	Platform string   `json:"platform"`
	Type     string   `json:"extension_type"`
	Domains  []string `json:"domains"`
	CPU      struct {
		Architecture string   `json:"architecture"`
		Extensions   []string `json:"instruction_set_extensions"`
	} `json:"cpu"`
	GPU struct {
		Make      string   `json:"make"`
		Framework string   `json:"framework"`
		Targets   []string `json:"targets"`
		MinDriver string   `json:"minimum_driver_version"`
	} `json:"gpu"`
	SupportedModelFormats []string `json:"supported_model_formats"`
	VendorLibPackages     []string `json:"vendor_lib_package_names"`
	EngineProtocolServer  struct {
		RuntimeKind    string `json:"runtime_kind"`
		ExecutablePath string `json:"executable_relative_path"`
	} `json:"engine_protocol_server"`
	// Provenance is present only in packages ModelFabric installed itself; LM
	// Studio ignores unknown keys, so the manifest stays in its schema.
	Provenance *PackageProvenance `json:"modelfabric,omitempty"`
	// LegacyProvenance reads the pre-rename provenance key so previously
	// installed packages remain eligible for runtime removal. Never written.
	LegacyProvenance *PackageProvenance `json:"llmz,omitempty"`
}

// PackageProvenance records where a ModelFabric-installed package came from and
// what was proven about it at install time.
type PackageProvenance struct {
	Source    string `json:"source"`    // the archive URL
	SHA256    string `json:"sha256"`    // the archive's published digest, verified
	Build     string `json:"build"`     // upstream build tag, e.g. "b11040"
	Installed string `json:"installed"` // RFC 3339
	// ProbedDevices are the accelerators the engine enumerated on this
	// machine at install. Upstream builds do not declare which GPU
	// architectures they were compiled for, so this is the evidence of fit.
	ProbedDevices []string `json:"probed_devices,omitempty"`
}

type artifactManifest struct {
	RuntimeKind    string `json:"runtime_kind"`
	ExecutablePath string `json:"executable_relative_path"`
	Files          []struct {
		RelativePath string `json:"relative_path"`
		Executable   bool   `json:"executable"`
	} `json:"files"`
}

func confine(root, rel string) (string, error) {
	p := filepath.Join(root, filepath.FromSlash(rel))
	r, err := filepath.Rel(root, p)
	if err != nil {
		return "", fmt.Errorf("path %q is not under %s", rel, root)
	}
	if r == ".." || strings.HasPrefix(r, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", fmt.Errorf("path %q escapes %s", rel, root)
	}
	return p, nil
}

// LMStudioRoot returns the LM Studio data directory, or "" if absent.
func LMStudioRoot() string {
	if r := os.Getenv("LMSTUDIO_HOME"); r != "" {
		return r
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	root := filepath.Join(home, ".lmstudio")
	if fi, err := os.Stat(root); err == nil && fi.IsDir() {
		return root
	}
	return ""
}

// DiscoverLMStudio finds installed LM Studio engine packages usable by ModelFabric.
//
// Only packages whose manifest declares an engine_protocol_server are returned:
// that block is the documented way to launch the engine as a standalone server,
// and without it the package is an LM Studio internal, not a public interface.
func DiscoverLMStudio(root string) ([]*Definition, error) {
	if root == "" {
		return nil, nil
	}
	return DiscoverPackages(filepath.Join(root, "extensions", "backends"), OriginLMStudio)
}

// DiscoverPackages reads every engine package in a backends directory laid out
// as LM Studio lays out its own: <dir>/<name>-<version>/backend-manifest.json,
// with shared dependency packages under <dir>/vendor/. ModelFabric installs its own
// runtimes in exactly this form, so one reader serves both.
func DiscoverPackages(backends string, origin Origin) ([]*Definition, error) {
	if backends == "" {
		return nil, nil
	}
	entries, err := os.ReadDir(backends)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	vendorDir := filepath.Join(backends, "vendor")

	var defs []*Definition
	for _, e := range entries {
		if !e.IsDir() || e.Name() == "vendor" {
			continue
		}
		if strings.HasPrefix(e.Name(), ".") {
			continue // an install in progress
		}
		pkg := filepath.Join(backends, e.Name())
		d, err := definitionFromPackage(pkg, vendorDir, origin)
		if err != nil || d == nil {
			continue
		}
		defs = append(defs, d)
	}
	// Newest first, so the default pick is the most recent build.
	sort.Slice(defs, func(i, j int) bool { return defs[i].Name > defs[j].Name })
	return defs, nil
}

func definitionFromPackage(pkg, vendorDir string, origin Origin) (*Definition, error) {
	raw, err := os.ReadFile(filepath.Join(pkg, "backend-manifest.json"))
	if err != nil {
		return nil, err
	}
	var bm backendManifest
	if err := json.Unmarshal(raw, &bm); err != nil {
		return nil, err
	}
	if bm.Provenance == nil {
		bm.Provenance = bm.LegacyProvenance
	}
	if bm.Type != "engine" || bm.EngineProtocolServer.ExecutablePath == "" {
		return nil, nil
	}
	// Reject unsupported engine families and formats. LM Studio's MLX packages
	// contain in-process .node addons, so ModelFabric uses a separate mlx-lm server.
	if _, ok := engines[bm.Engine]; !ok {
		return nil, nil
	}
	if !supportsFormat(bm.Engine, bm.SupportedModelFormats) {
		return nil, nil
	}

	// The manifest is data from a package, not a trusted instruction: an
	// executable_relative_path of "../../../bin/sh" joins and cleans to a real
	// path outside the package, and the entrypoint is what gets launched.
	entry, err := confine(pkg, bm.EngineProtocolServer.ExecutablePath)
	if err != nil {
		return nil, fmt.Errorf("package %s: %w", pkg, err)
	}
	if fi, err := os.Stat(entry); err != nil || fi.IsDir() {
		return nil, fmt.Errorf("package %s declares a missing entrypoint", pkg)
	}

	// The entrypoint's RUNPATH is $ORIGIN, so it finds its own siblings. What
	// it cannot find is the vendor package (libcudart and friends), which the
	// manifest names separately - so those directories go on the library path.
	var libDirs []string
	for _, v := range bm.VendorLibPackages {
		// Same for a vendor package name: these directories become
		// LD_LIBRARY_PATH for the engine process.
		dir, err := confine(vendorDir, v)
		if err != nil {
			return nil, fmt.Errorf("package %s: %w", pkg, err)
		}
		if fi, err := os.Stat(dir); err == nil && fi.IsDir() {
			libDirs = append(libDirs, dir)
		}
	}

	env := map[string]string{}
	if len(libDirs) > 0 {
		env["LD_LIBRARY_PATH"] = strings.Join(libDirs, string(os.PathListSeparator))
	}
	if bm.Engine == "mlx" {
		// mlx-lm downloads unrecognized model ids. Offline mode makes an unexpected
		// lookup fail instead of fetching weights outside the prepared model.
		env["HF_HUB_OFFLINE"] = "1"
	}

	// Match LM Studio's own naming so `mfsh runtime ls` and `lms runtime ls`
	// refer to the same thing: "<name>@<version>".
	name := bm.Name
	if name == "" {
		name = filepath.Base(pkg)
	}
	if bm.Version != "" {
		name += "@" + bm.Version
	}

	display, build := readDisplayData(pkg, bm.Version)
	if bm.Provenance != nil {
		build = atoiOr0(strings.TrimPrefix(bm.Provenance.Build, "b"))
		if display == "" {
			// Installed before ModelFabric wrote display data.
			label := bm.GPU.Framework
			if label == "" {
				label = "CPU"
			}
			if code := atoiOr0(bm.GPU.MinDriver); label == "CUDA" && code > 0 {
				label += fmt.Sprintf(" %d.%d", code/1000, (code%1000)/10)
			}
			display = label + " llama.cpp (upstream)"
		}
	}

	req := Requirements{
		CPUExtensions: bm.CPU.Extensions,
		GPUFramework:  bm.GPU.Framework,
		GPUTargets:    bm.GPU.Targets,
		MinDriver:     minDriverCode(bm.GPU.MinDriver),
	}
	if bm.Provenance != nil {
		req.ProbedDevices = bm.Provenance.ProbedDevices
	}
	return &Definition{
		Name:        name,
		Engine:      bm.Engine,
		Origin:      origin,
		Version:     bm.Version,
		LlamaBuild:  build,
		DisplayName: display,
		Backend:     backendLabel(bm),
		Protocol:    ProtocolOpenAI,
		Entrypoint:  entry,
		Env:         env,
		vendorDirs:  libDirs,
		pkgDir:      pkg,
		domains:     bm.Domains,
		provenance:  bm.Provenance,
		req:         req,
	}, nil
}

var llamaReleasePattern = regexp.MustCompile(`llama\.cpp release b(\d+)`)

// readDisplayData reads the display name and upstream llama.cpp build from
// package release notes. Returns empty and zero when absent.
func readDisplayData(pkg, version string) (name string, build int) {
	raw, err := os.ReadFile(filepath.Join(pkg, "display-data.json"))
	if err != nil {
		return "", 0
	}
	// [["en", {"displayName", "releaseNotes": [{"version", "releaseNotes"}]}], ...]
	var langs [][]json.RawMessage
	if json.Unmarshal(raw, &langs) != nil {
		return "", 0
	}
	for _, l := range langs {
		if len(l) < 2 {
			continue
		}
		var lang string
		_ = json.Unmarshal(l[0], &lang)
		var dd struct {
			DisplayName  string `json:"displayName"`
			ReleaseNotes []struct {
				Version string `json:"version"`
				Notes   string `json:"releaseNotes"`
			} `json:"releaseNotes"`
		}
		if json.Unmarshal(l[1], &dd) != nil {
			continue
		}
		if name == "" || lang == "en" {
			name = dd.DisplayName
		}
		for _, n := range dd.ReleaseNotes {
			if n.Version != version || build != 0 {
				continue
			}
			if m := llamaReleasePattern.FindStringSubmatch(n.Notes); m != nil {
				build = atoiOr0(m[1])
			}
		}
	}
	return name, build
}

// engineFormats are the weights formats each engine loads, as manifests spell
// them. MLX packages name the file format (safetensors) or the framework.
var engineFormats = map[string][]string{
	"llama.cpp": {"gguf"},
	"mlx":       {"mlx", "safetensors"},
}

func supportsFormat(engine string, formats []string) bool {
	for _, f := range formats {
		for _, want := range engineFormats[engine] {
			if strings.EqualFold(f, want) {
				return true
			}
		}
	}
	return false
}

func backendLabel(bm backendManifest) string {
	if bm.GPU.Framework != "" {
		return strings.ToLower(bm.GPU.Framework)
	}
	if bm.GPU.Make != "" {
		return strings.ToLower(bm.GPU.Make)
	}
	return "cpu"
}

// ArtifactFiles returns the package-declared artifact set for verification,
// excluding incidental files.
func (d *Definition) ArtifactFiles() ([]string, error) {
	if d.pkgDir == "" {
		return []string{d.Path()}, nil
	}
	raw, err := os.ReadFile(filepath.Join(d.pkgDir, "engine-protocol-server-artifacts.json"))
	if os.IsNotExist(err) {
		// Not every package ships the manifest. Verification then covers the
		// entrypoint alone, which is the most that can be checked.
		return []string{d.Path()}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read artifact manifest for %s: %w", d.pkgDir, err)
	}
	var am artifactManifest
	if err := json.Unmarshal(raw, &am); err != nil {
		// A manifest that exists but cannot be read is not the same as one
		// that was never there: falling back here quietly reduced
		// verification to the entrypoint, which Verify then called a pass.
		return nil, fmt.Errorf("artifact manifest for %s is unreadable: %w", d.pkgDir, err)
	}
	files := make([]string, 0, len(am.Files))
	for _, f := range am.Files {
		p, err := confine(d.pkgDir, f.RelativePath)
		if err != nil {
			return nil, fmt.Errorf("artifact manifest for %s: %w", d.pkgDir, err)
		}
		// Symlinks inside the package (libfoo.so -> libfoo.so.0.4.1) resolve to
		// the same bytes; the manifest lists both, and hashing both is correct.
		if _, err := os.Stat(p); err != nil {
			// A file the package declares and does not have is a broken or
			// tampered package, not a file to skip.
			return nil, fmt.Errorf("%s declares %s, which is missing", d.pkgDir, f.RelativePath)
		}
		files = append(files, p)
	}
	if len(files) == 0 {
		return []string{d.Path()}, nil
	}
	return files, nil
}

// PackageDir is the package root, or "" for a runtime not from a package.
func (d *Definition) PackageDir() string { return d.pkgDir }

func (d *Definition) Provenance() *PackageProvenance { return d.provenance }

func (d *Definition) Dir() string {
	if d.pkgDir != "" {
		return d.pkgDir
	}
	return filepath.Dir(d.Path())
}

func (d *Definition) envList() []string {
	if len(d.Env) == 0 {
		return nil
	}
	keys := make([]string, 0, len(d.Env))
	for k := range d.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, k+"="+d.Env[k])
	}
	return out
}

func (d *Definition) Domains() []string { return d.domains }

func (d *Definition) VendorDirs() []string { return d.vendorDirs }

// LMStudioModelsDir is where an LM Studio install keeps downloaded models.
// Returns "" when there is no such directory.
func LMStudioModelsDir(root string) string {
	if root == "" {
		return ""
	}
	dir := filepath.Join(root, "models")
	if fi, err := os.Stat(dir); err == nil && fi.IsDir() {
		return dir
	}
	return ""
}

// minDriverCode reads a manifest's minimum driver requirement. LM Studio
// writes either a bare code ("12040") or a dotted version ("12.4"); the dotted
// form used to become 0, which reads as "no requirement" and offered a build
// the driver may be too old for.
func minDriverCode(s string) int {
	if s == "" {
		return 0
	}
	if n := atoiOr0(s); n > 0 {
		return n
	}
	return cudaVersionCode(s)
}

func atoiOr0(s string) int {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0
		}
		n = n*10 + int(c-'0')
	}
	return n
}
