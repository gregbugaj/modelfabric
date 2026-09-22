package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/gregbugaj/modelfabric/internal/discovery"
	"github.com/gregbugaj/modelfabric/internal/llmd"
	"github.com/gregbugaj/modelfabric/internal/mesh"
)

// llmdCmd groups llm-d integration commands.
func llmdCmd(args []string) error {
	if len(args) > 0 {
		switch args[0] {
		case "install":
			return llmdInstall()
		case "enable", "disable", "status", "profiles":
			return llmdManage(args[0], args[1:])
		}
	}
	if len(args) == 0 || args[0] != "init" {
		return fmt.Errorf("usage: mfsh llmd init [-dir DIR] [-prefix-cache=false] [-listen 8080]")
	}
	return llmdInit(args[1:])
}

// llmdInit writes everything needed to put llm-d's EPP in front of this mesh:
// an EndpointPickerConfig mapped to llama.cpp's metrics, an Envoy bootstrap
// that routes through the EPP, and a first snapshot of the endpoint list.
//
// It edits no configuration of its own. Keeping the endpoint list current is
// the node's job once `llmd_endpoints_file` points at the same path.
func llmdInit(args []string) error {
	fs := flag.NewFlagSet("llmd init", flag.ExitOnError)
	addr := fs.String("addr", defaultAddr, "address of the local ModelFabric node")
	defDir := filepath.Join(defaultStateDir(), "llmd")
	dir := fs.String("dir", defDir, "directory to write the configs into")
	prefix := fs.Bool("prefix-cache", true, "enable the approximate prefix-cache scorer")
	listen := fs.Int("listen", 8080, "port Envoy listens on for clients")
	eppPort := fs.Int("epp-port", 9002, "EPP ext_proc gRPC port")
	if err := fs.Parse(args); err != nil {
		return err
	}
	// These are written into the Envoy bootstrap and the EPP's command line,
	// where an out-of-range value produces a config that will not load — or,
	// worse, one that loads with a port nobody expects.
	for name, port := range map[string]int{"-listen": *listen, "-epp-port": *eppPort} {
		if port < 1 || port > 65535 {
			return fmt.Errorf("%s must be a port between 1 and 65535 (got %d)", name, port)
		}
	}
	if *listen == *eppPort {
		return fmt.Errorf("-listen and -epp-port cannot both be %d", *listen)
	}
	abs, err := filepath.Abs(*dir)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return err
	}
	endpoints := filepath.Join(abs, "endpoints.yaml")

	files := map[string][]byte{
		"epp.yaml": discovery.EPPConfig(discovery.EPPOptions{EndpointsPath: endpoints, PrefixCache: *prefix}),
		"envoy.yaml": discovery.EnvoyConfig(discovery.EnvoyOptions{
			ListenPort: *listen, EPPPort: *eppPort, AdminPort: 19000,
		}),
	}

	// Seed the endpoint list from the running mesh, so the EPP has something to
	// serve before the node's own writer takes over.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := ensureNode(*addr); err == nil {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(*addr, "/")+"/z/endpoints.yaml", nil)
		if resp, err := http.DefaultClient.Do(req); err == nil {
			if b, err := io.ReadAll(resp.Body); err == nil && resp.StatusCode == http.StatusOK {
				files["endpoints.yaml"] = b
			}
			resp.Body.Close()
		}
	}
	if _, ok := files["endpoints.yaml"]; !ok {
		b, _ := discovery.Render(nil)
		files["endpoints.yaml"] = b
	}

	for name, data := range files {
		if err := os.WriteFile(filepath.Join(abs, name), data, 0o644); err != nil {
			return err
		}
		fmt.Printf("%s %s\n", green("✓ wrote"), filepath.Join(abs, name))
	}

	// Suggest the tailnet address, never 0.0.0.0: a wildcard bind would put
	// unauthenticated engines on every interface, the LAN included.
	bindHint := "<tailnet-ip>"
	sctx, scancel := context.WithTimeout(context.Background(), 5*time.Second)
	if _, addr, err := mesh.SelfIdentity(sctx); err == nil && addr != "" {
		bindHint = addr
	}
	scancel()

	fmt.Printf(`
%s
  1. Keep the endpoint list current — add to %s:
       "llmd_endpoints_file": %q,
       "engine_bind": %q          %s

  2. Start the EPP (build it from github.com/llm-d/llm-d-router, ./cmd/epp):
       epp --config-file %s \
           --pool-name mfsh --pool-namespace mfsh \
           --grpc-port %d --grpc-health-port %d --metrics-port 9090

  3. Start Envoy:
       docker run --rm --network host -v %s:/etc/envoy/envoy.yaml:ro \
           envoyproxy/envoy:v1.33-latest -c /etc/envoy/envoy.yaml

  4. Send OpenAI requests to http://localhost:%d/v1 — the EPP picks the endpoint.
`,
		bold("Next:"), dim(defaultConfigPath()), endpoints,
		bindHint, dim("# this node's tailnet IP; only needed when the EPP runs on another host"),
		filepath.Join(abs, "epp.yaml"), *eppPort, *eppPort+1,
		filepath.Join(abs, "envoy.yaml"), *listen)
	return nil
}

// llmdManage runs llm-d under the node for one model and points the front door
// at it. llm-d is designed around one model per pool, so it serves one.
func llmdManage(sub string, args []string) error {
	fs := flag.NewFlagSet("llmd "+sub, flag.ExitOnError)
	addr := fs.String("addr", defaultAddr, "address of the local ModelFabric node")
	prefix := fs.Bool("prefix-cache", true, "with enable -profile load-aware: score engines by prefix-cache locality")
	profile := fs.String("profile", discovery.ProfileLoadAware, "with enable: scheduling profile (mfsh llmd profiles)")
	room := fs.Bool("room-filter", true, "with enable: never pick an engine with no free slot while another has room (uses an Alpha llm-d plugin)")
	kvCeiling := fs.Float64("kv-ceiling", 0, "with enable: refuse engines whose KV cache is fuller than this share (0..1); a thrash guard, not load balancing — on llama.cpp a full cache means warm")
	kvScorer := fs.Int("kv-scorer", 0, "with enable: weight for llm-d's kv-cache-utilization-scorer, which prefers the emptier cache; pulls against prefix affinity on llama.cpp")
	positional, err := parsePositional(fs, args)
	if err != nil {
		return err
	}
	// Enable waits for llm-d to become ready on the server side, which is
	// allowed three minutes; a 60s client deadline reported a timeout for an
	// operation that was still on its way to succeeding.
	timeout := 60 * time.Second
	if sub == "enable" {
		timeout = 4 * time.Minute
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	var st llmd.Status
	switch sub {
	case "enable":
		if len(positional) != 1 {
			return fmt.Errorf("usage: mfsh llmd enable <model> [-profile NAME] [-room-filter=false] [-prefix-cache=false] [-kv-ceiling 0.97] [-kv-scorer N]\n  llm-d serves one model; every other model keeps its direct route")
		}
		if err := call(ctx, *addr, http.MethodPost, "/api/v1/llmd/enable",
			map[string]any{"model": positional[0], "profile": *profile, "prefix_cache": *prefix, "room_filter": *room,
				"kv_ceiling": *kvCeiling, "kv_scorer": *kvScorer}, &st); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "%s llm-d (EPP + Envoy) for %s...\n", dim("starting"), bold(st.Model))
		deadline := time.Now().Add(3 * time.Minute)
		for time.Now().Before(deadline) {
			time.Sleep(time.Second)
			if err := call(ctx, *addr, http.MethodGet, "/api/v1/llmd", nil, &st); err != nil {
				return err
			}
			switch st.State {
			case "running":
				fmt.Printf("%s llm-d serves %s with the %s profile, at %s across %d engine(s)\n", green("✓"),
					bold(st.Model), bold(st.Profile), st.URL, len(st.Endpoints))
				if len(st.Endpoints) == 0 {
					fmt.Printf("  %s no engine serves it yet; load it with %s\n", yellow("!"), bold("mfsh load "+st.Model))
				}
				fmt.Println(dim("  This node routes it through llm-d; other models use ModelFabric's router."))
				return nil
			case "restarting":
				return fmt.Errorf("llm-d did not start: %s", st.Error)
			}
		}
		return fmt.Errorf("llm-d still starting; check mfsh llmd status")
	case "profiles":
		var body struct {
			Profiles []discovery.ProfileInfo `json:"profiles"`
		}
		if err := call(ctx, *addr, http.MethodGet, "/api/v1/llmd/profiles", nil, &body); err != nil {
			return err
		}
		t := newTable("PROFILE", "", "")
		for _, p := range body.Profiles {
			if !p.Available {
				continue
			}
			note := ""
			if p.Experimental {
				note = yellow("uses Alpha llm-d plugins")
			}
			t.add(bold(p.Name), p.Title, note)
		}
		t.print()
		fmt.Println()
		for _, p := range body.Profiles {
			if p.Available {
				fmt.Printf("  %s  %s\n", bold(p.Name), dim(p.Summary))
			}
		}
		fmt.Printf("\n%s\n", dim("llm-d well-lit paths llama.cpp cannot run:"))
		for _, p := range body.Profiles {
			if !p.Available {
				fmt.Printf("  %s %s — %s\n", dim("·"), p.Title, dim(p.Reason))
			}
		}
		fmt.Printf("\n  %s\n", bold("mfsh llmd enable <model> -profile <name>"))
		return nil
	case "disable":
		if err := call(ctx, *addr, http.MethodPost, "/api/v1/llmd/disable", map[string]any{}, &st); err != nil {
			return err
		}
		fmt.Printf("%s llm-d stopped; ModelFabric's own router serves the model again\n", green("✓"))
		return nil
	default:
		if err := call(ctx, *addr, http.MethodGet, "/api/v1/llmd", nil, &st); err != nil {
			return err
		}
		l := &st
		if l.State == "" || l.State == "disabled" {
			fmt.Println("llm-d is off.")
			fmt.Printf("\n  Run it for one model with %s\n", bold("mfsh llmd enable <model>"))
			return nil
		}
		t := newTable("LLM-D", "")
		t.indent = "  "
		t.add(dim("state"), l.State)
		t.add(dim("model"), bold(l.Model))
		t.add(dim("profile"), l.Profile)
		cal := "default (the engines had no traffic to measure)"
		switch l.PrefillSource {
		case "engines":
			cal = "measured from the engines"
		case "saved":
			cal = "measured in an earlier run"
		}
		t.add(dim("peak prefill"), fmt.Sprintf("%.0f tok/s, %s", l.PeakPrefill, cal))
		if l.MeasuredPrefill > 0 && l.MeasuredPrefill != l.PeakPrefill {
			t.add(dim("measured now"), fmt.Sprintf("%.0f tok/s, used from the next %s", l.MeasuredPrefill, bold("mfsh llmd enable")))
		}
		t.add(dim("url"), l.URL)
		t.add(dim("prefix cache"), fmt.Sprint(l.PrefixCache))
		room := "off: pure llm-d profile"
		if l.RoomFilter {
			room = "on: engines with no free slot are skipped while another has room (Alpha plugin)"
		}
		t.add(dim("room filter"), room)
		t.add(dim("engines"), strings.Join(l.Endpoints, ", "))
		if l.Restarts > 0 {
			t.add(dim("restarts"), fmt.Sprint(l.Restarts))
		}
		if l.Error != "" {
			t.add(dim("error"), l.Error)
		}
		t.print()
		return nil
	}
}

// llmdInstall fetches the pinned EPP (from llm-d's image, over HTTPS — no
// container runtime) and Envoy (its official release binary).
func llmdInstall() error {
	cfg := llmd.Config{Tools: filepath.Join(fabricHome(), "tools")}
	if cfg.Installed() {
		fmt.Printf("%s llm-d EPP %s and Envoy %s are installed\n", green("✓"), llmd.EPPVersion, llmd.EnvoyVersion)
		return nil
	}
	fmt.Printf("Installing llm-d: EPP %s (from its image, digest-pinned) and Envoy %s (release binary, SHA-256-pinned)\n",
		llmd.EPPVersion, llmd.EnvoyVersion)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	start, last := time.Now(), time.Time{}
	err := cfg.Install(ctx, func(what string, done, total int64) {
		if time.Since(last) < 150*time.Millisecond && done != total {
			return
		}
		last = time.Now()
		pct := float64(done) / float64(max(total, 1)) * 100
		fmt.Fprintf(os.Stderr, "\r\x1b[K  %s %5.1f%%  %s", progressBar(pct, 24), pct, dim(what))
	})
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return err
	}
	fmt.Printf("%s in %s — no containers; both run as processes under the node\n", green("✓ installed"), time.Since(start).Round(time.Second))
	fmt.Printf("\n  Serve a model through it with %s\n", bold("mfsh llmd enable <model>"))
	return nil
}
