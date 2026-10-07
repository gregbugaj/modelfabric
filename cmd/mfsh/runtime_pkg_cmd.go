package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/gregbugaj/modelfabric/internal/config"
	"github.com/gregbugaj/modelfabric/internal/rtpkg"
	"github.com/gregbugaj/modelfabric/internal/runtime"
)

type runtimeInfo struct {
	Name       string   `json:"name"`
	Origin     string   `json:"origin"`
	Backend    string   `json:"backend"`
	Default    bool     `json:"default"`
	Fit        string   `json:"fit"`
	LlamaBuild int      `json:"llama_build"`
	PackageDir string   `json:"package_dir"`
	Managed    bool     `json:"managed"`
	InUse      []string `json:"in_use"`
}

func shortDigest(sum string) string {
	switch {
	case sum == "":
		return "(none published)"
	case len(sum) <= 16:
		return sum
	default:
		return sum[:16] + "…"
	}
}

func runtimePkgCmd(sub string, args []string) error {
	fs := flag.NewFlagSet("runtime "+sub, flag.ExitOnError)
	addr := fs.String("addr", defaultAddr, "address of the local ModelFabric node")
	yes := fs.Bool("y", false, "install without asking")
	build := fs.String("build", "", "upstream build to install, e.g. b11040 (default: newest complete)")
	cuda := fs.String("cuda", "", "CUDA toolkit version of the build, e.g. 12.8 (default: newest the driver supports)")
	positional, err := parsePositional(fs, args)
	if err != nil {
		return err
	}
	cfg, err := config.Load(defaultConfigPath())
	if err != nil {
		return fmt.Errorf("read config: %w", err)
	}
	root := runtimesRoot(cfg)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	switch sub {
	case "get":
		if len(positional) > 1 {
			return fmt.Errorf("usage: mfsh runtime get [cpu|cuda|vulkan|rocm] [-build bN] [-cuda X.Y] [-y]")
		}
		want := rtpkg.Want{Build: *build, CUDA: *cuda}
		if len(positional) == 1 {
			want.Backend = positional[0]
		}
		name, err := installRuntime(ctx, root, want, *yes)
		if err != nil || name == "" {
			return err
		}
		return announceRuntime(ctx, *addr, name)

	case "update":
		if len(positional) > 0 {
			return fmt.Errorf("usage: mfsh runtime update [-y]   (it updates every installed runtime)")
		}
		return updateRuntimes(ctx, *addr, root, *yes)

	case "remove":
		if len(positional) != 1 {
			return fmt.Errorf("usage: mfsh runtime remove <name>   (one at a time)")
		}
		return removeRuntime(ctx, *addr, positional)
	}
	return fmt.Errorf("unknown runtime command %q", sub)
}

// installRuntime resolves, shows and installs one build. It returns the new
// runtime's name, or "" when the operator declined.
func installRuntime(ctx context.Context, root string, want rtpkg.Want, yes bool) (string, error) {
	fmt.Fprintf(os.Stderr, "%s upstream llama.cpp builds...\n", dim("checking"))
	client := rtpkg.NewClient()
	lctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	builds, err := client.Builds(lctx, 30)
	cancel()
	if err != nil {
		return "", err
	}
	hw := runtime.Survey(ctx)
	plan, err := rtpkg.Choose(builds, want, hw)
	if err != nil {
		return "", err
	}

	fmt.Printf("\n%s  %s\n", bold(plan.RuntimeName()), dim("("+plan.Reason+")"))
	t := newTable("FILE", "SIZE", "SHA256")
	t.indent = "  "
	t.add(plan.Engine.Name, humanBytes(plan.Engine.Size), dim(shortDigest(plan.Engine.SHA256)))
	download := plan.Engine.Size
	if plan.Vendor != nil {
		if _, err := os.Stat(filepath.Join(root, "vendor", plan.VendorName)); err == nil {
			t.add(plan.Vendor.Name, dim("installed"), dim("shared with other CUDA "+plan.BackendVersion+" builds"))
		} else {
			t.add(plan.Vendor.Name, humanBytes(plan.Vendor.Size), dim(shortDigest(plan.Vendor.SHA256)))
			download += plan.Vendor.Size
		}
	}
	fmt.Println()
	t.print()
	fmt.Printf("\n  %s to download, into %s\n\n", bold(humanBytes(download)), dim(root))

	if _, err := os.Stat(filepath.Join(root, plan.Dir())); err == nil {
		fmt.Printf("%s is already installed.\n", plan.RuntimeName())
		return "", nil
	}
	if !yes {
		if !isTTY() {
			return "", fmt.Errorf("not a terminal; pass -y to install without asking")
		}
		if !confirm("Install?") {
			return "", ErrCancelled
		}
	}

	start, last := time.Now(), time.Time{}
	dir, err := client.Install(ctx, plan, root, hw, func(file string, done, total int64) {
		if time.Since(last) < 150*time.Millisecond && done != total {
			return
		}
		last = time.Now()
		pct := float64(done) / float64(max(total, 1)) * 100
		fmt.Fprintf(os.Stderr, "\r\x1b[K  %s %5.1f%%  %s / %s  %s", progressBar(pct, 24), pct,
			humanBytes(done), humanBytes(total), dim(shortName(file)))
	})
	fmt.Fprintln(os.Stderr)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return "", fmt.Errorf("interrupted; run the same command again to resume")
		}
		return "", err
	}
	fmt.Printf("%s %s in %s — verified against upstream's SHA-256, and it runs here\n",
		green("✓ installed"), bold(plan.RuntimeName()), time.Since(start).Round(time.Second))
	fmt.Println(dim("  " + dir))
	return plan.RuntimeName(), nil
}

func shortName(file string) string {
	if strings.HasPrefix(file, "cudart-") {
		return "CUDA runtime"
	}
	return "engine"
}

func announceRuntime(ctx context.Context, addr, installed string) error {
	// Preserve the caller's signal-aware context so Ctrl-C cancels the request.
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	var body struct {
		Runtimes []runtimeInfo `json:"runtimes"`
	}
	if err := call(ctx, addr, http.MethodPost, "/api/v1/runtimes/rescan", map[string]any{}, &body); err != nil {
		return err
	}
	for _, r := range body.Runtimes {
		if !r.Default {
			continue
		}
		switch {
		case r.Name == installed:
			fmt.Printf("\n  New loads use it: it is the newest compatible build.\n")
		case installed != "":
			fmt.Printf("\n  New loads still use %s. Switch with %s\n", bold(r.Name), bold("mfsh runtime select "+installed))
		}
	}
	return nil
}

// updateRuntimes installs the newest upstream build of each installed variant and retains old builds for rollback.
func updateRuntimes(ctx context.Context, addr, root string, yes bool) error {
	lctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	builds, err := rtpkg.NewClient().Builds(lctx, 30)
	cancel()
	if err != nil {
		return err
	}
	updates, err := rtpkg.Updates(root, builds, runtime.Survey(ctx))
	if err != nil {
		return err
	}
	if len(updates) == 0 {
		fmt.Println("No ModelFabric runtimes installed to update.")
		fmt.Printf("\n  Install one with %s\n", bold("mfsh runtime get"))
		return nil
	}
	var installed []string
	for _, u := range updates {
		switch {
		case u.Err != "":
			fmt.Printf("%s %s: %s\n", yellow("!"), u.Family, u.Err)
			continue
		case !u.Available:
			fmt.Printf("%s %s is up to date (b%d)\n", green("✓"), u.Family, u.Installed)
			continue
		}
		got, err := installRuntime(ctx, root, rtpkg.WantFromName(u.Family), yes)
		if err != nil {
			return err
		}
		if got != "" {
			installed = append(installed, got)
		}
	}
	if len(installed) == 0 {
		return nil
	}
	fmt.Printf("\n  Older builds are kept; remove one with %s\n", bold("mfsh runtime remove <name>"))
	return announceRuntime(ctx, addr, installed[len(installed)-1])
}

// removeRuntime deletes unused ModelFabric runtimes; LM Studio packages and loaded runtimes are protected.
func removeRuntime(ctx context.Context, addr string, positional []string) error {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	var body struct {
		Runtimes []runtimeInfo `json:"runtimes"`
	}
	if err := call(ctx, addr, http.MethodGet, "/api/v1/runtimes", nil, &body); err != nil {
		return err
	}

	name := ""
	if len(positional) == 1 {
		name = positional[0]
	} else {
		var choices []Choice
		for _, r := range body.Runtimes {
			if r.Managed {
				note := r.Backend
				if len(r.InUse) > 0 {
					note += "  (in use)"
				}
				choices = append(choices, Choice{Value: r.Name, Label: r.Name, Note: note})
			}
		}
		if len(choices) == 0 {
			fmt.Println("No ModelFabric-installed runtimes to remove.")
			return nil
		}
		var err error
		if name, err = selectOne("Select a runtime to remove", choices); err != nil {
			return err
		}
	}

	// The node owns the checks (ModelFabric-installed only, not in use) and the
	// registry, so removal goes through it rather than deleting files here.
	if err := call(ctx, addr, http.MethodPost, "/api/v1/runtimes/remove", map[string]any{"name": name}, nil); err != nil {
		return err
	}
	fmt.Printf("%s %s\n", green("✓ removed"), bold(name))
	return nil
}
