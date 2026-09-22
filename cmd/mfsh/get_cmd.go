package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/gregbugaj/modelfabric/internal/catalog"
	"github.com/gregbugaj/modelfabric/internal/config"
	"github.com/gregbugaj/modelfabric/internal/hub"
	"github.com/gregbugaj/modelfabric/internal/runtime"
)

// getCmd downloads a model from Hugging Face, like `lms get`. It accepts LM
// Studio hub ids (qwen/qwen3.8-27b) as well as repositories and URLs. With
// -from it copies a model another node in the mesh already has instead.
func getCmd(args []string) error {
	fs := flag.NewFlagSet("get", flag.ExitOnError)
	addr := fs.String("addr", defaultAddr, "address of the local ModelFabric node")
	yes := fs.Bool("y", false, "download without asking")
	raw := fs.Bool("repo", false, "treat the argument as a Hugging Face repository, never a hub id")
	from := fs.String("from", "", "copy the model from this mesh node instead of Hugging Face")
	format := fs.String("format", "", "with -from: which weights when the node has both (gguf or mlx)")
	positional, err := parsePositional(fs, args)
	if err != nil {
		return err
	}
	if len(positional) != 1 {
		return fmt.Errorf("usage: mfsh get <hub id | user/repo>[@quant] | <huggingface URL>  [-y]\n       mfsh get <model> -from <node>  [-format gguf|mlx] [-y]")
	}
	if *format != "" && *from == "" {
		return fmt.Errorf("-format chooses between a node's copies; use it with -from")
	}

	// Downloads land in ModelFabric's own models root. An LM Studio install's tree
	// is read-only to ModelFabric, so it is never a destination.
	// config.Load only forgives a missing file; an unreadable or malformed one
	// is an error. Discarding it downloaded into the default root instead of
	// the configured one, which is a different disk on some nodes.
	cfg, err := config.Load(defaultConfigPath())
	if err != nil {
		return fmt.Errorf("read config: %w", err)
	}
	root := cfg.ModelsRoot
	if root == "" {
		root = defaultModelsRoot()
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if *from != "" {
		return getFromPeer(ctx, strings.TrimRight(*addr, "/"), *from, strings.TrimSpace(positional[0]), *format, root, *yes)
	}
	ref, err := hub.ParseRef(positional[0])
	if err != nil {
		return err
	}

	client := hub.NewClient()
	fmt.Fprintf(os.Stderr, "%s %s...\n", dim("resolving"), bold(ref.Repo))
	requested := ref.Repo
	loadAs := strings.TrimSpace(positional[0])
	var nearMisses []string
	if !*raw {
		mapped, via, near := resolveHubID(ctx, client, cfg, ref.Repo)
		if mapped != "" {
			fmt.Fprintf(os.Stderr, "  %s is an LM Studio hub id %s %s\n", requested, dim("→ "+via+" →"), bold(mapped))
			ref.Repo = mapped
			if via != "local hub catalog" {
				// Without a local manifest the catalog names the model from
				// its GGUF metadata, which need not match the hub id; the
				// repository path always resolves.
				loadAs = mapped
			}
		}
		nearMisses = near
	}
	plan, err := client.Resolve(ctx, ref)
	if err != nil {
		if len(nearMisses) > 0 {
			return fmt.Errorf("%w\n\n  %s is not a known LM Studio hub id either. Close matches:\n    %s",
				err, requested, strings.Join(nearMisses, "\n    "))
		}
		return err
	}

	t := newTable("FILE", "SIZE", "SHA256")
	t.indent = "  "
	for _, f := range plan.Files {
		sum := dim("(none published)")
		if len(f.SHA256) >= 16 {
			sum = dim(f.SHA256[:16] + "…")
		} else if f.SHA256 != "" {
			sum = dim(f.SHA256) // shorter than expected: show what there is
		}
		t.add(f.Name, humanBytes(f.Size), sum)
	}
	rev := plan.Revision
	if len(rev) > 12 {
		rev = rev[:12] // a short revision is not always 12 characters
	}
	fmt.Printf("\n%s @ %s  (%s, %s)\n\n", bold(plan.Repo), dim(rev), plan.Quant, bold(humanBytes(plan.TotalBytes())))
	t.print()
	fmt.Printf("\n  into %s\n\n", dim(root))

	if !*yes {
		if !isTTY() {
			return fmt.Errorf("not a terminal; pass -y to download without asking")
		}
		if !confirm("Download?") {
			return ErrCancelled
		}
	}

	start := time.Now()
	dir, err := client.Download(ctx, plan, root, progressLine(start))
	fmt.Fprintln(os.Stderr)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return fmt.Errorf("interrupted; run the same command again to resume")
		}
		return err
	}
	// The plan itself shows "(none published)" for files the hub gives no
	// hash for, and those are not checksum-verified. Claiming every download
	// was verified overstated exactly the case the operator needs to know.
	unverified := 0
	for _, f := range plan.Files {
		if f.SHA256 == "" {
			unverified++
		}
	}
	note := "verified against the published SHA-256"
	if unverified > 0 {
		note = fmt.Sprintf("%d of %d files verified against the published SHA-256; %s published none",
			len(plan.Files)-unverified, len(plan.Files), plural(unverified, "file"))
	}
	fmt.Printf("%s %s in %s — %s\n",
		green("✓ downloaded"), bold(plan.Repo), time.Since(start).Round(time.Second), note)
	fmt.Println(dim("  " + dir))

	// Tell a running node about it; if none is running, the next start sees it.
	rctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := call(rctx, *addr, http.MethodPost, "/api/v1/models/rescan", map[string]any{}, nil); err == nil {
		fmt.Printf("\n  Load it with %s\n", bold("mfsh load "+loadAs))
	}
	return nil
}

// resolveHubID maps an LM Studio hub id to the GGUF repository behind it: the
// local hub catalog first (authoritative, offline), then lmstudio-community's
// public repositories. It returns "" when the reference should be used as a
// Hugging Face repository as written, plus any near misses to suggest if that
// fails too.
func resolveHubID(ctx context.Context, client *hub.Client, cfg config.Config, id string) (repo, via string, near []string) {
	if !cfg.DisableLMStudioModels {
		if e, ok := catalog.LoadHub(runtime.LMStudioRoot()).Lookup(id); ok {
			for _, r := range e.SourceRepos {
				if strings.HasSuffix(strings.ToLower(r), "-gguf") {
					return r, "local hub catalog", nil
				}
			}
		}
	}
	sctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	repo, near, err := client.HubGGUFRepo(sctx, id)
	if err != nil {
		return "", "", nil // the search is a convenience; fall through to the repo
	}
	if repo != "" {
		return repo, hub.LMStudioCommunity, nil
	}
	return "", "", near
}

func progressBar(pct float64, width int) string {
	filled := int(pct / 100 * float64(width))
	if filled > width {
		filled = width
	}
	return cyan(strings.Repeat("█", filled)) + dim(strings.Repeat("░", width-filled))
}
