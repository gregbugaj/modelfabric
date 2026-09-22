package inputs

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func digestOf(manifest []byte) string {
	sum := sha256.Sum256(manifest)
	return hex.EncodeToString(sum[:])
}

func write(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	return path
}

// The spec's own acceptance case: a model that changed on disk after the
// manifest was built must be rejected.
func TestChangedModelIsRejected(t *testing.T) {
	dir := t.TempDir()
	model := write(t, dir, "model.gguf", "first")

	manifest, err := BuildManifest(dir, []string{model})
	if err != nil {
		t.Fatalf("BuildManifest: %v", err)
	}
	expected := digestOf(manifest)

	if _, err := VerifyInput(dir, manifest, expected, ""); err != nil {
		t.Fatalf("verify before change: %v", err)
	}

	if err := os.WriteFile(model, []byte("changed"), 0o644); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	_, err = VerifyInput(dir, manifest, expected, "")
	if !errors.Is(err, ErrInputVerification) {
		t.Fatalf("error = %v, want ErrInputVerification", err)
	}
}

// A same-size edit must still be caught — size alone is only a fast pre-filter.
func TestSameSizeEditIsRejected(t *testing.T) {
	dir := t.TempDir()
	model := write(t, dir, "model.gguf", "aaaaa")
	manifest, _ := BuildManifest(dir, []string{model})
	expected := digestOf(manifest)

	if err := os.WriteFile(model, []byte("bbbbb"), 0o644); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	if _, err := VerifyInput(dir, manifest, expected, ""); !errors.Is(err, ErrInputVerification) {
		t.Fatalf("a same-size content change was not detected: %v", err)
	}
}

func TestMissingFileIsRejected(t *testing.T) {
	dir := t.TempDir()
	model := write(t, dir, "model.gguf", "weights")
	manifest, _ := BuildManifest(dir, []string{model})
	expected := digestOf(manifest)

	if err := os.Remove(model); err != nil {
		t.Fatalf("remove: %v", err)
	}
	err := VerifyMust(t, dir, manifest, expected)
	if !strings.Contains(err.Error(), "missing") {
		t.Fatalf("error = %v, want a missing-file error", err)
	}
}

func VerifyMust(t *testing.T, dir string, manifest []byte, expected string) error {
	t.Helper()
	_, err := VerifyInput(dir, manifest, expected, "")
	if err == nil {
		t.Fatal("expected verification to fail")
	}
	return err
}

func TestTamperedManifestIsRejected(t *testing.T) {
	dir := t.TempDir()
	model := write(t, dir, "model.gguf", "weights")
	manifest, _ := BuildManifest(dir, []string{model})

	// Right files, wrong expected digest: the manifest is not the one we agreed on.
	if _, err := VerifyInput(dir, manifest, strings.Repeat("0", 64), ""); !errors.Is(err, ErrInputVerification) {
		t.Fatal("a mismatched manifest digest must be rejected")
	}

	// Manifest edited to describe a file that is not what is on disk.
	swapped := []byte(strings.Replace(string(manifest), "model.gguf", "other.gguf", 1))
	if _, err := VerifyInput(dir, swapped, digestOf(swapped), ""); !errors.Is(err, ErrInputVerification) {
		t.Fatal("a manifest naming a missing file must be rejected")
	}
}

func TestEmptyManifestIsRejected(t *testing.T) {
	if _, err := BuildManifest(t.TempDir(), nil); !errors.Is(err, ErrInputVerification) {
		t.Fatal("an empty file list must not produce a manifest")
	}
	if _, err := VerifyInput(t.TempDir(), []byte("  \n"), digestOf([]byte("  \n")), ""); !errors.Is(err, ErrInputVerification) {
		t.Fatal("an empty manifest must be rejected")
	}
}

func TestPathEscapeIsRejected(t *testing.T) {
	dir := t.TempDir()
	for _, bad := range []string{"../escape", "/etc/passwd", `windows\path`, "a/../../b"} {
		line := strings.Repeat("a", 64) + "  5  " + bad + "\n"
		_, err := VerifyInput(dir, []byte(line), digestOf([]byte(line)), "")
		if !errors.Is(err, ErrInputVerification) {
			t.Fatalf("path %q was accepted; it must be rejected", bad)
		}
	}
}

// The canonical encoding must be stable regardless of the order files are
// listed in, or another implementation could not reproduce the digest.
func TestManifestEncodingIsCanonical(t *testing.T) {
	dir := t.TempDir()
	a := write(t, dir, "a.txt", "alpha")
	b := write(t, dir, "sub/b.txt", "beta")
	c := write(t, dir, "c.txt", "gamma")

	m1, err := BuildManifest(dir, []string{a, b, c})
	if err != nil {
		t.Fatalf("BuildManifest: %v", err)
	}
	m2, err := BuildManifest(dir, []string{c, a, b})
	if err != nil {
		t.Fatalf("BuildManifest: %v", err)
	}
	if string(m1) != string(m2) {
		t.Fatalf("manifest depends on input order:\n%s\nvs\n%s", m1, m2)
	}

	lines := strings.Split(strings.TrimRight(string(m1), "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(lines))
	}
	// Sorted bytewise: a.txt, c.txt, sub/b.txt
	for i, want := range []string{"a.txt", "c.txt", "sub/b.txt"} {
		if !strings.HasSuffix(lines[i], "  "+want) {
			t.Fatalf("line %d = %q, want it to end with %q", i, lines[i], want)
		}
	}
	// The documented shape: "<64 hex>  <size>  <path>"
	parts := strings.SplitN(lines[0], "  ", 3)
	if len(parts) != 3 || !isHex64(parts[0]) || parts[1] != "5" {
		t.Fatalf("line %q does not match the canonical encoding", lines[0])
	}
}

// A snapshot must cover tokenizer/processor/config files, not just weights.
func TestManifestVerificationCoversWholeSnapshot(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "model.safetensors", "weights")
	write(t, dir, "tokenizer.json", "tok")
	write(t, dir, "config.json", "cfg")
	write(t, dir, "nested/preprocessor_config.json", "pre")

	manifest, err := buildManifestFromDir(dir)
	if err != nil {
		t.Fatalf("buildManifestFromDir: %v", err)
	}
	for _, want := range []string{"model.safetensors", "tokenizer.json", "config.json", "nested/preprocessor_config.json"} {
		if !strings.Contains(string(manifest), want) {
			t.Fatalf("manifest omits %s", want)
		}
	}
	if _, err := VerifyInput(dir, manifest, digestOf(manifest), ""); err != nil {
		t.Fatalf("verify snapshot: %v", err)
	}
}

func TestVerifyTreePinsRuntimeByContent(t *testing.T) {
	dir := t.TempDir()
	bin := write(t, dir, "llama-server", "#!/bin/sh\necho hi\n")

	vi, manifest, err := VerifyTree(dir, []string{bin}, "")
	if err != nil {
		t.Fatalf("VerifyTree: %v", err)
	}
	// Revalidate needs these bytes; returning only the VerifiedInput handed
	// the caller something it could never check again.
	if len(manifest) == 0 {
		t.Error("no manifest returned, so this runtime cannot be revalidated")
	}
	if !isHex64(vi.ManifestSHA256) {
		t.Fatalf("runtime digest %q is not a sha256", vi.ManifestSHA256)
	}

	// Swapping the binary must change its identity — a mutable system
	// environment cannot be reported as a pinned runtime.
	if err := os.WriteFile(bin, []byte("#!/bin/sh\necho different\n"), 0o644); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	vi2, _, err := VerifyTree(dir, []string{bin}, "")
	if err != nil {
		t.Fatalf("VerifyTree: %v", err)
	}
	if vi.ManifestSHA256 == vi2.ManifestSHA256 {
		t.Fatal("runtime digest did not change when the binary changed")
	}
}

func TestPreparedInputsValidate(t *testing.T) {
	model := VerifiedInput{Root: "/m", ManifestSHA256: strings.Repeat("a", 64)}
	runtime := &VerifiedInput{Root: "/r", ManifestSHA256: strings.Repeat("b", 64)}
	image := &OCIImageRef{Reference: "repo:tag", Digest: "sha256:" + strings.Repeat("c", 64)}

	if err := (PreparedInputs{Runtime: runtime, Model: model}).Validate(); err != nil {
		t.Fatalf("native inputs should be valid: %v", err)
	}
	if err := (PreparedInputs{Model: model}).Validate(); err == nil {
		t.Fatal("inputs with no runtime must be rejected")
	}
	// Runtime is a verified tree OR an image, never both.
	if err := (PreparedInputs{Runtime: runtime, RuntimeImage: image, Model: model}).Validate(); err == nil {
		t.Fatal("a runtime that is both a tree and an image must be rejected")
	}
	if err := (PreparedInputs{Runtime: runtime}).Validate(); err == nil {
		t.Fatal("an unverified model must be rejected")
	}
}

func TestVerifyTreeRoundTrip(t *testing.T) {
	dir := t.TempDir()
	f := write(t, dir, "model.gguf", "weights")
	vi, manifest, err := VerifyTree(dir, []string{f}, "main")
	if err != nil {
		t.Fatalf("VerifyTree: %v", err)
	}
	if vi.Revision != "main" {
		t.Fatalf("revision = %q, want main", vi.Revision)
	}
	if _, err := VerifyInput(dir, manifest, vi.ManifestSHA256, "main"); err != nil {
		t.Fatalf("re-verify with the returned manifest: %v", err)
	}
}

// "duplicate entries" — a manifest naming the same path twice is ambiguous:
// one entry could verify while a conflicting one is never reached.
func TestDuplicateManifestEntriesAreRejected(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "model.gguf", "weights")
	line := strings.Repeat("a", 64) + "  7  model.gguf\n"
	dup := []byte(line + line)
	if _, err := VerifyInput(dir, dup, digestOf(dup), ""); !errors.Is(err, ErrInputVerification) {
		t.Fatal("a manifest with duplicate entries must be rejected")
	}

	// And we must never emit one either.
	f := filepath.Join(dir, "model.gguf")
	if _, err := BuildManifest(dir, []string{f, f}); !errors.Is(err, ErrInputVerification) {
		t.Fatal("BuildManifest must not emit duplicate entries")
	}
}

// A Hugging Face snapshot is built almost entirely from symlinks into the
// shared blob cache. Skipping them would produce a manifest that blesses
// nothing, so they must be resolved by content.
func TestHuggingFaceStyleSymlinksAreResolved(t *testing.T) {
	root := t.TempDir()
	blobs := filepath.Join(root, "blobs")
	snapshot := filepath.Join(root, "snapshot")
	if err := os.MkdirAll(blobs, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(snapshot, 0o755); err != nil {
		t.Fatal(err)
	}
	blob := filepath.Join(blobs, "deadbeef")
	if err := os.WriteFile(blob, []byte("model weights"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(snapshot, "model.gguf")
	if err := os.Symlink(blob, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	manifest, err := buildManifestFromDir(snapshot)
	if err != nil {
		t.Fatalf("buildManifestFromDir: %v", err)
	}
	if !strings.Contains(string(manifest), "model.gguf") {
		t.Fatalf("symlinked snapshot file was skipped; manifest = %q", manifest)
	}
	if _, err := VerifyInput(snapshot, manifest, digestOf(manifest), ""); err != nil {
		t.Fatalf("verify symlinked snapshot: %v", err)
	}

	// Changing the blob behind the link must invalidate the snapshot.
	if err := os.WriteFile(blob, []byte("tampered....."), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyInput(snapshot, manifest, digestOf(manifest), ""); !errors.Is(err, ErrInputVerification) {
		t.Fatal("a changed blob behind a snapshot symlink must be detected")
	}
}

func TestBrokenSymlinkIsRejected(t *testing.T) {
	dir := t.TempDir()
	if err := os.Symlink(filepath.Join(dir, "gone"), filepath.Join(dir, "model.gguf")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := buildManifestFromDir(dir); !errors.Is(err, ErrInputVerification) {
		t.Fatal("a broken symlink must be rejected, not silently skipped")
	}
}

// "A manifest must not bless mutable files that are replaced between
// verification and startup."
func TestRevalidateClosesTheTOCTOUWindow(t *testing.T) {
	dir := t.TempDir()
	model := write(t, dir, "model.gguf", "weights")
	mv, manifest, err := VerifyTree(dir, []string{model}, "")
	if err != nil {
		t.Fatalf("VerifyTree: %v", err)
	}
	rdir := t.TempDir()
	rbin := write(t, rdir, "llama-server", "#!/bin/sh\n")
	rv, rmanifest, err := VerifyTree(rdir, []string{rbin}, "")
	if err != nil {
		t.Fatalf("VerifyTree runtime: %v", err)
	}

	prepared := PreparedInputs{Runtime: rv, Model: *mv}
	manifests := Manifests{mv.ManifestSHA256: manifest, rv.ManifestSHA256: rmanifest}

	if err := prepared.Revalidate(manifests); err != nil {
		t.Fatalf("revalidate before any change: %v", err)
	}

	// Swap the model after verification but before launch.
	if err := os.WriteFile(model, []byte("swapped"), 0o644); err != nil {
		t.Fatal(err)
	}
	err = prepared.Revalidate(manifests)
	if !errors.Is(err, ErrInputVerification) {
		t.Fatalf("revalidate error = %v, want ErrInputVerification", err)
	}
	if !strings.Contains(err.Error(), "model changed since verification") {
		t.Fatalf("error should name the model as the thing that changed: %v", err)
	}
}

// The manifest is line-oriented, so a file name holding a newline used to be
// written as several records that could not be read back.
func TestBuildManifestRejectsALineBreakInAName(t *testing.T) {
	dir := t.TempDir()
	name := "weird\nname.gguf"
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
		t.Skipf("this filesystem will not take a newline in a name: %v", err)
	}
	if _, err := BuildManifest(dir, []string{name}); err == nil {
		t.Fatal("a file name with a newline was written into the manifest")
	}
}

// Verified inputs with no root would send Revalidate to the process working
// directory, verifying whatever happened to be there.
func TestValidateRequiresARoot(t *testing.T) {
	p := PreparedInputs{
		Runtime: &VerifiedInput{Root: "/opt/rt", ManifestSHA256: "abc"},
		Model:   VerifiedInput{ManifestSHA256: "def"},
	}
	if err := p.Validate(); err == nil {
		t.Fatal("prepared inputs with no model root were accepted")
	}
	p.Model.Root = "/models/x"
	if err := p.Validate(); err != nil {
		t.Fatalf("complete inputs were rejected: %v", err)
	}
}

// BuildManifest and VerifyTree resolve a relative file under root; stamps did
// not, so they were statted against the process working directory. With an
// absolute root and relative names, filepath.Rel then failed and the model got
// no stamps at all — meaning every load re-hashed it.
func TestStampsResolveRelativeFilesUnderRoot(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "model.gguf"), []byte("weights"), 0o644); err != nil {
		t.Fatal(err)
	}
	stamps := stampsFor(dir, []string{"model.gguf"})
	if len(stamps) != 1 {
		t.Fatalf("got %d stamps, want 1 (a relative name under root)", len(stamps))
	}
	if stamps[0].Path != "model.gguf" {
		t.Errorf("stamp path %q, want %q", stamps[0].Path, "model.gguf")
	}
	if stamps[0].Size != int64(len("weights")) {
		t.Errorf("stamp size %d, want %d", stamps[0].Size, len("weights"))
	}
}
