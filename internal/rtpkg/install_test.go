package rtpkg

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"testing"
)

// Regression: symlinked destination directories must not redirect archive
// extraction outside the runtime package.
func TestExtractRefusesToWriteThroughASymlink(t *testing.T) {
	outside := t.TempDir()
	dest := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(dest, "bin")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	body := []byte("pwned")
	for _, h := range []*tar.Header{
		{Name: "top/", Typeflag: tar.TypeDir, Mode: 0o755},
		{Name: "top/bin/file", Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(body))},
	} {
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if h.Typeflag == tar.TypeReg {
			if _, err := tw.Write(body); err != nil {
				t.Fatal(err)
			}
		}
	}
	tw.Close()
	gz.Close()

	err := extractTarGz(bytes.NewReader(buf.Bytes()), dest)
	if err == nil {
		t.Fatal("extraction through a symlinked directory was allowed")
	}
	if _, statErr := os.Stat(filepath.Join(outside, "file")); statErr == nil {
		t.Fatal("the archive wrote outside the destination")
	}
}
