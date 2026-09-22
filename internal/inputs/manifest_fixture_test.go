package inputs

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// Snapshot enumeration is currently only needed to construct verification fixtures.
// buildManifestFromDir walks root and manifests every file. Use it for a
// Hugging Face snapshot, where tokenizer, processor and config files must all
// be covered — not just the weights.
//
// Symlinks are resolved rather than skipped. A Hugging Face snapshot is built
// almost entirely from symlinks into the shared blob cache, so skipping them
// would silently produce a manifest that blesses nothing. The recorded path is
// the one inside root; the hashed bytes come from the link target.
func buildManifestFromDir(root string) ([]byte, error) {
	var files []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		// Stat follows symlinks; a link to a directory is skipped, a link to a
		// regular file is captured by content.
		info, err := os.Stat(path)
		if err != nil {
			if d.Type()&fs.ModeSymlink != 0 {
				return fmt.Errorf("%w: broken symlink %s", ErrInputVerification, path)
			}
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		files = append(files, path)
		return nil
	})
	if err != nil {
		if errors.Is(err, ErrInputVerification) {
			return nil, err
		}
		return nil, fmt.Errorf("%w: %v", ErrInputVerification, err)
	}
	return BuildManifest(root, files)
}
