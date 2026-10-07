package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gregbugaj/modelfabric/internal/config"
)

func importCmd(args []string) error {
	fs := flag.NewFlagSet("import", flag.ExitOnError)
	addr := fs.String("addr", defaultAddr, "address of the local ModelFabric node")
	as := fs.String("as", "", "place it as user/repo (default: local/<file name>)")
	mode := fs.String("mode", "link", "how to adopt it: link (hard link, falls back to copy), copy, symlink, move")
	positional, err := parsePositional(fs, args)
	if err != nil {
		return err
	}
	if len(positional) != 1 {
		return fmt.Errorf("usage: mfsh import <file.gguf> [-as user/repo] [-mode link|copy|symlink|move]")
	}
	src, err := filepath.Abs(positional[0])
	if err != nil {
		return err
	}
	if err := checkGGUF(src); err != nil {
		return err
	}

	stem := strings.TrimSuffix(filepath.Base(src), filepath.Ext(src))
	repo := *as
	if repo == "" {
		repo = "local/" + stem
	}
	if parts := strings.Split(repo, "/"); len(parts) != 2 || parts[0] == "" || parts[1] == "" ||
		strings.Contains(repo, "..") {
		return fmt.Errorf("-as must be user/repo, got %q", repo)
	}

	cfg, err := config.Load(defaultConfigPath())
	if err != nil {
		return fmt.Errorf("read config: %w", err)
	}
	root := cfg.ModelsRoot
	if root == "" {
		root = defaultModelsRoot()
	}
	dest := filepath.Join(root, filepath.FromSlash(repo))
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return err
	}

	files := []string{src}
	// Include the projector; without it a vision model loads but only serves text.
	if !strings.HasPrefix(strings.ToLower(filepath.Base(src)), "mmproj") {
		matches, _ := filepath.Glob(filepath.Join(filepath.Dir(src), "mmproj*.gguf"))
		switch len(matches) {
		case 0:
		case 1:
			files = append(files, matches[0])
		default:
			// Do not choose by sort order: another model's projector cannot provide vision support.
			return fmt.Errorf("%s holds %d projectors; import the model and its own projector by name:\n  mfsh import %s\n  mfsh import <the matching mmproj file>",
				filepath.Dir(src), len(matches), src)
		}
	}

	for _, f := range files {
		target := filepath.Join(dest, filepath.Base(f))
		if _, err := os.Stat(target); err == nil {
			return fmt.Errorf("%s already exists", target)
		}
		how, err := adopt(f, target, *mode)
		if err != nil {
			return fmt.Errorf("%s: %w", filepath.Base(f), err)
		}
		fmt.Printf("%s %s %s\n", green("✓ "+how), filepath.Base(f), dim("→ "+target))
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := call(ctx, *addr, http.MethodPost, "/api/v1/models/rescan", map[string]any{}, nil); err == nil {
		fmt.Printf("\n  Load it with %s\n", bold("mfsh load "+repo))
	}
	return nil
}

func checkGGUF(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	var magic [4]byte
	if _, err := io.ReadFull(f, magic[:]); err != nil || string(magic[:]) != "GGUF" {
		return fmt.Errorf("%s is not a GGUF file", path)
	}
	return nil
}

func adopt(src, dst, mode string) (string, error) {
	switch mode {
	case "link":
		if err := os.Link(src, dst); err == nil {
			return "linked", nil
		} else if !errors.Is(err, os.ErrExist) {
			// Most likely a different filesystem; a copy is the safe fallback.
			if err := copyFile(src, dst); err != nil {
				return "", err
			}
			return "copied (hard link not possible across filesystems)", nil
		} else {
			return "", err
		}
	case "copy":
		return "copied", copyFile(src, dst)
	case "symlink":
		return "symlinked", os.Symlink(src, dst)
	case "move":
		if err := os.Rename(src, dst); err == nil {
			return "moved", nil
		}
		if err := copyFile(src, dst); err != nil {
			return "", err
		}
		return "moved", os.Remove(src)
	default:
		return "", fmt.Errorf("unknown -mode %q", mode)
	}
}

// copyFile copies via a temporary name so an interrupted copy never looks like
// a complete model to the catalog.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	// A named "<dst>.part" was predictable and os.Create follows a symlink
	// left at that path, so another process could choose where this landed.
	out, err := os.CreateTemp(filepath.Dir(dst), filepath.Base(dst)+".part-*")
	if err != nil {
		return err
	}
	tmp := out.Name()
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	// Use Link because it rejects an existing destination; Rename could overwrite a concurrent import after the existence check.
	if err := os.Link(tmp, dst); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Remove(tmp)
}
