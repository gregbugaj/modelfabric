package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/gregbugaj/modelfabric/internal/hub"
)

type sharedModel struct {
	Key      string `json:"key"`
	Format   string `json:"format"`
	Dir      string `json:"dir"`
	Repo     string `json:"repo"`
	Revision string `json:"revision"`
	Files    []struct {
		Name   string `json:"name"`
		Size   int64  `json:"size"`
		SHA256 string `json:"sha256"`
	} `json:"files"`
}

// getFromPeer copies a model through the local node proxy. The peer authorizes
// access by Tailscale owner. Downloads resume and verify the peer's SHA-256
// before renaming files into place.
func getFromPeer(ctx context.Context, addr, node, ref, format, root string, yes bool) error {
	base := "/api/v1/nodes/" + url.PathEscape(node) + "/share"
	q := url.Values{"model": {ref}}
	if format != "" {
		q.Set("format", format)
	}
	fmt.Fprintf(os.Stderr, "%s %s on %s %s\n", dim("asking for"), bold(ref), bold(node),
		dim("(a model it has not shared before is hashed first)"))
	var offer sharedModel
	if err := call(ctx, addr, http.MethodGet, base+"/model?"+q.Encode(), nil, &offer); err != nil {
		return fmt.Errorf("%s: %w", node, err)
	}
	if len(offer.Files) == 0 {
		return fmt.Errorf("%s lists no files for %s", node, ref)
	}
	// Confine peer-provided destination paths to the models root.
	if !filepath.IsLocal(filepath.FromSlash(offer.Dir)) {
		return fmt.Errorf("%s offered %s at %q, which is outside a models root", node, ref, offer.Dir)
	}
	dir := filepath.Join(root, filepath.FromSlash(offer.Dir))

	plan := &hub.Plan{Repo: offer.Repo, Revision: offer.Revision}
	var total int64
	t := newTable("FILE", "SIZE", "SHA256")
	t.indent = "  "
	for _, f := range offer.Files {
		if len(f.SHA256) != 64 {
			// Require peer digests: the hub client skips verification when a digest is absent.
			return fmt.Errorf("%s gave no SHA-256 for %s", node, f.Name)
		}
		if !filepath.IsLocal(filepath.FromSlash(f.Name)) {
			return fmt.Errorf("%s offered a file outside the model directory: %q", node, f.Name)
		}
		plan.Files = append(plan.Files, hub.File{Name: f.Name, Size: f.Size, SHA256: f.SHA256})
		total += f.Size
		t.add(f.Name, humanBytes(f.Size), dim(f.SHA256[:16]+"…"))
	}
	have, err := checkNoClobber(dir, plan.Files)
	if err != nil {
		return err
	}
	if have == total {
		fmt.Printf("%s %s is already here, identical to %s's copy\n", green("✓"), bold(offer.Key), node)
		fmt.Println(dim("  " + dir))
		return nil
	}

	fmt.Printf("\n%s from %s  (%s, %s)\n\n", bold(offer.Key), bold(node), offer.Format, bold(humanBytes(total)))
	t.print()
	fmt.Printf("\n  into %s\n\n", dim(dir))
	if !yes {
		if !isTTY() {
			return fmt.Errorf("not a terminal; pass -y to copy without asking")
		}
		if !confirm("Copy?") {
			return ErrCancelled
		}
	}

	client := &hub.Client{
		BaseURL: addr + base + "/files",
		// No overall timeout, as for Hugging Face: a large file takes minutes.
		// And no token: HF_TOKEN is for Hugging Face, never for a peer.
		HTTP: &http.Client{Transport: &http.Transport{ResponseHeaderTimeout: 60 * time.Second}},
	}
	// A resumed file's .part counts as already here: the rate and the
	// summary are about what crossed the tailnet this time.
	for _, f := range plan.Files {
		if info, err := os.Stat(filepath.Join(dir, filepath.FromSlash(f.Name)) + ".part"); err == nil && info.Size() < f.Size {
			have += info.Size()
		}
	}
	start := time.Now()
	err = client.DownloadInto(ctx, plan, dir, progressLine(start))
	fmt.Fprintln(os.Stderr)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return fmt.Errorf("interrupted; run the same command again to resume")
		}
		return err
	}
	moved := total - have
	rate := float64(moved) / max(time.Since(start).Seconds(), 0.001)
	note := ""
	if have > 0 {
		note = fmt.Sprintf(", %s was already here", humanBytes(have))
	}
	fmt.Printf("%s %s of %s from %s in %s (%s/s%s), every file checked against %s's SHA-256\n",
		green("✓ copied"), humanBytes(moved), bold(offer.Key), node, time.Since(start).Round(time.Second),
		humanBytes(int64(rate)), note, node)
	fmt.Println(dim("  " + dir))

	rctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := call(rctx, addr, http.MethodPost, "/api/v1/models/rescan", map[string]any{}, nil); err != nil {
		return nil // no node running: the next start scans it
	}
	models, err := fetchModels(rctx, addr)
	if err == nil && !slices.ContainsFunc(models, func(m apiModel) bool { return m.Key == offer.Key }) {
		fmt.Printf("\n  %s is not listed under that name here; %s shows what it is called on this machine\n",
			offer.Key, bold("mfsh ls"))
		return nil
	}
	fmt.Printf("\n  Load it with %s\n", bold("mfsh load "+offer.Key))
	return nil
}

// checkNoClobber rejects copies that would overwrite different local files and
// returns the byte count of identical files. The hub client would otherwise
// replace mismatched files, including existing models.
func checkNoClobber(dir string, files []hub.File) (int64, error) {
	var have int64
	for _, f := range files {
		p := filepath.Join(dir, filepath.FromSlash(f.Name))
		info, err := os.Stat(p)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return 0, err
		}
		if info.Size() == f.Size {
			sum, err := sha256File(p)
			if err != nil {
				return 0, err
			}
			if sum == f.SHA256 {
				have += f.Size // already here and identical; the copy skips it
				continue
			}
		}
		return 0, fmt.Errorf("%s already exists here with different contents; move it aside to copy this model", p)
	}
	return have, nil
}

func sha256File(p string) (string, error) {
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

// progressLine draws one line of download progress, throttled: a 1 MiB buffer
// would otherwise repaint constantly.
func progressLine(start time.Time) hub.Progress {
	last := time.Time{}
	return func(_ string, done, total int64) {
		if time.Since(last) < 150*time.Millisecond && done != total {
			return
		}
		last = time.Now()
		pct := float64(done) / float64(max(total, 1)) * 100
		rate := float64(done) / max(time.Since(start).Seconds(), 0.001)
		fmt.Fprintf(os.Stderr, "\r\x1b[K  %s %5.1f%%  %s / %s  %s/s",
			progressBar(pct, 24), pct, humanBytes(done), humanBytes(total), humanBytes(int64(rate)))
	}
}
