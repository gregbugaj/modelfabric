// Command mfsh runs a ModelFabric mesh node.
//
// Every machine runs one. Apps point at its loopback address and see every
// model in the mesh as though it were local.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/gregbugaj/modelfabric/internal/config"
	"github.com/gregbugaj/modelfabric/internal/discovery"
	"github.com/gregbugaj/modelfabric/internal/doctor"
	"github.com/gregbugaj/modelfabric/internal/inputs"
	"github.com/gregbugaj/modelfabric/internal/llmd"
	"github.com/gregbugaj/modelfabric/internal/mesh"
	"github.com/gregbugaj/modelfabric/internal/nodekey"
	"github.com/gregbugaj/modelfabric/internal/ops"
	"github.com/gregbugaj/modelfabric/internal/process"
	"github.com/gregbugaj/modelfabric/internal/router"
	"github.com/gregbugaj/modelfabric/internal/runtime"
	"github.com/gregbugaj/modelfabric/internal/server"
	"github.com/gregbugaj/modelfabric/internal/slotcache"
	"github.com/gregbugaj/modelfabric/internal/supervisor"
	"github.com/gregbugaj/modelfabric/internal/tsid"
)

// defaultAddr is the node the CLI talks to. MFSH_ADDR points it at another
// one, alongside MFSH_HOME, MFSH_MODELS and MFSH_CONFIG. Without it, the CLI
// follows the address a running node recorded, then the config's: a
// fixed :1234 had `mfsh ls` reporting no node while one ran on -port 3000.
//
// It exists because -addr's position matters: after a subcommand's own
// positional argument Go's flag package stops parsing, so `llmd enable <model>
// -addr X` reads the flag as two more arguments and fails with a usage error.
// Threading it into the right slot for every subcommand is fragile; an
// environment variable applies to all of them and cannot land in the wrong
// place. A -addr flag still wins where one is given.
var defaultAddr = func() string {
	listen, _ := loadListen(defaultConfigPath())
	return pickAddr(os.Getenv("MFSH_ADDR"), runningListen(), listen)
}()

const usage = `ModelFabric — a peer-to-peer model mesh over Tailscale

Models on disk
  mfsh get <user/repo>[@quant] download a GGUF model from Hugging Face
  mfsh get … -from <node>      copy a model another node has, over the tailnet
  mfsh import <file.gguf>      adopt a GGUF already on disk (-as user/repo)
  mfsh ls                      models available to load on this node
  mfsh ps                      models currently loaded in memory
  mfsh load [model]            load a model; prompts if not named
                               every LM Studio load/inference setting is a flag (mfsh load -h)
  mfsh defaults <model>        a model's saved settings, applied on every load (LM Studio's gear)
  mfsh preset [ls|save|rm|import]   named inference settings; imports LM Studio presets
  mfsh unload [instance-id]    unload one instance, or -all; prompts if not named
  mfsh repin <model>           accept changed model files as the new pinned state
  mfsh recover -clear <model>  settle an unresolved launch window after a crash

Mesh
  mfsh status                  nodes in the mesh and what they serve
  mfsh models                  every model in the mesh, and who holds it
  mfsh ops                     recent load/unload operations
  mfsh chat [model]            talk to the mesh from here; every reply says which node answered
  mfsh bench [-model M]        the standard benchmark: 1K-32K prompts, 1-8 at once  [-node N | -fleet] [-quick]
  mfsh tune [model]            measure how many slots this machine should run
  mfsh log                     stream routed requests (bodies only while capture is on)
  mfsh log engine [instance]   show an engine's own output  [-f -n N]
  mfsh endpoints               mesh model servers in llm-d file-discovery format
  mfsh prefer [node]           prefer a node when several hold the same model
  mfsh llmd init               write EPP + Envoy configs to put llm-d in front
  mfsh key                     this node's API key, for apps that call it
  mfsh key create <name>       a named token for one app, shown once; mfsh key ls / rm <name>
  mfsh key rotate              replace the API key; clients holding the old one are refused, named tokens still work

Runtime
  mfsh runtime ls              inference engines this node can launch
  mfsh runtime survey          hardware, and which runtimes can use it
  mfsh runtime select [name]   choose the default runtime
  mfsh runtime get|update|remove   manage ModelFabric's own upstream llama.cpp builds

Node
  mfsh serve                   run in the foreground  [-config FILE -listen ADDR -port N -node NAME -v]
  mfsh up                      start the node in the background  [-port N]
  mfsh down                    stop the background node
  mfsh doctor                  check everything ModelFabric depends on, with fixes  [-json]
  mfsh service install         run as a systemd service (headless)  [-enable]

Commands that need a node will start one automatically.
Every node serves the union of all models in the mesh at its own loopback
address; there is no central server. Run with -h for a command's flags.`

// version is set at build time (-X main.version); "dev" otherwise.
var version = "dev"

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	migrateRename()
	var err error
	switch os.Args[1] {
	case "serve":
		err = serve(os.Args[2:])
	case "status":
		err = status(os.Args[2:], false)
	case "models":
		err = status(os.Args[2:], true)
	case "ls":
		err = lsCmd(os.Args[2:])
	case "ps":
		err = psCmd(os.Args[2:])
	case "load":
		err = loadCmd(os.Args[2:])
	case "unload":
		err = unloadCmd(os.Args[2:])
	case "ops":
		err = opsCmd(os.Args[2:])
	case "endpoints":
		err = endpointsCmd(os.Args[2:])
	case "runtime":
		err = runtimeCmd(os.Args[2:])
	case "prefer":
		err = preferCmd(os.Args[2:])
	case "llmd":
		err = llmdCmd(os.Args[2:])
	case "key":
		err = keyCmd(os.Args[2:])
	case "defaults":
		err = defaultsCmd(os.Args[2:])
	case "preset", "presets":
		err = presetCmd(os.Args[2:])
	case "doctor":
		err = doctorCmd(os.Args[2:])
	case "version", "--version", "-v":
		fmt.Println("mfsh", version)
		return
	case "get":
		err = getCmd(os.Args[2:])
	case "chat":
		err = chatCmd(os.Args[2:])
	case "bench":
		err = benchCmd(os.Args[2:])
	case "tune":
		err = tuneCmd(os.Args[2:])
	case "log":
		err = logCmd(os.Args[2:])
	case "import":
		err = importCmd(os.Args[2:])
	case "service":
		err = serviceCmd(os.Args[2:])
	case "repin":
		err = repinCmd(os.Args[2:])
	case "recover":
		err = recoverCmd(os.Args[2:])
	case "up":
		err = upCmd(os.Args[2:])
	case "down":
		err = downCmd(os.Args[2:])
	case "-h", "--help", "help":
		fmt.Println(usage)
		return
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s\n", os.Args[1], usage)
		os.Exit(2)
	}
	if err != nil {
		if errors.Is(err, ErrCancelled) {
			os.Exit(130) // cancelling a prompt is not a failure to report
		}
		fmt.Fprintln(os.Stderr, red("error:"), err)
		os.Exit(1)
	}
}

func serve(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	cfgPath := fs.String("config", defaultConfigPath(), "path to config file")
	listen := fs.String("listen", "", "override listen address")
	port := fs.Int("port", 0, "override only the listen port, keeping the configured address")
	node := fs.String("node", "", "override node name (default: Tailscale hostname)")
	verbose := fs.Bool("v", false, "verbose logging")
	if err := fs.Parse(args); err != nil {
		return err
	}

	level := slog.LevelInfo
	if *verbose {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	migrateLegacyState(log)

	if err := requireExplicitConfig(fs, *cfgPath); err != nil {
		return err
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	if *listen != "" {
		cfg.Listen = *listen
	}
	if *port != 0 {
		if cfg.Listen, err = withPort(cfg.Listen, *port); err != nil {
			return err
		}
	}
	if *node != "" {
		cfg.Node = *node
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	name, selfAddr, err := mesh.SelfIdentity(ctx)
	if err != nil {
		hn, _ := os.Hostname()
		if hn == "" {
			return fmt.Errorf("could not determine node identity: %w", err)
		}
		log.Warn("falling back to OS hostname for node name", "name", hn, "err", err)
		name = hn
	}
	if cfg.Node != "" {
		name = cfg.Node
	}

	m := mesh.New(cfg, name)
	m.SetSelfAddr(selfAddr)
	// A persisted choice beats the config default, since it was set later.
	preferred := cfg.PreferredNode
	// stateDir, not defaultStateDir: a node configured with state_dir kept its
	// instances and journal there while this one file was read from the
	// default location, so the preference silently did not persist.
	if p := server.LoadPreferred(filepath.Join(stateDirOf(cfg), "preferred.json")); p != "" {
		preferred = p
	}
	m.SetPreferred(preferred)
	rt := router.New(m, log)
	// A request that makes no progress at all is abandoned and its slot
	// released; see router.Router.Stall for the ninety-minute deadlock that
	// motivated it.
	rt.Stall = cfg.Stall()
	// And an output ceiling for requests that name none, so "no limit" cannot
	// mean "the whole context window" on a machine other people share.
	rt.MaxOutputTokens = cfg.MaxOutputTokens
	m.SetHostMemoryProvider(runtime.HostMemory)
	// What the router has sent where, and what it is holding: published with
	// the node's state so any node's dashboard can show it.
	m.SetRouterViewProvider(func() ([]mesh.HeldRequest, []mesh.RoutedTo) { return rt.Holding(), rt.Routed() })
	m.SetEngineGoneHook(rt.EngineGone)
	if cfg.PrefixAffinity == nil || *cfg.PrefixAffinity {
		rt.EnablePrefixAffinity()
		switch cfg.Placement {
		case "":
		case "home-slot":
			rt.UseHomeSlotPlacement()
		case "no-room-rule":
			rt.UseNoRoomRulePlacement()
		default:
			log.Warn("unknown placement in config, using the default", "placement", cfg.Placement, "known", "home-slot, no-room-rule")
		}
		// Inside, because the queue asks affinity whose slot a slot is.
		if grace, maxWait, on := cfg.Queue(); on {
			rt.EnableQueue(grace, maxWait)
			// Published with the node's state, so any node's dashboard can
			// say how many requests are waiting here and not on an engine.
			m.SetQueuedProvider(func() int64 { return int64(rt.Queued()) })
			log.Info("router queue on", "grace", grace, "max_wait", maxWait)
		}
	}

	// "tailnet" is Tailscale's address for this node, known only now.
	bindAs := cfg.EngineBind
	var bindWarn string
	cfg.EngineBind, bindWarn = cfg.ResolveEngineBind(selfAddr)
	if bindWarn != "" {
		log.Warn(bindWarn)
	}
	sup, err := buildSupervisor(ctx, cfg, selfAddr, m, log)
	// The disk tier of the prompt cache. Off unless a size is configured: what
	// it writes contains the conversations it saves.
	if sup != nil && cfg.CacheDiskMiB > 0 {
		dir := cfg.CacheDiskDir
		if dir == "" {
			dir = filepath.Join(stateDirOf(cfg), "slots")
		}
		// Its own client, with no response timeout: the mesh's gives up after
		// two minutes without headers, and saving a long conversation's slot
		// can take longer than that. Each call carries its own deadline.
		if cold, cerr := slotcache.Open(dir, int64(cfg.CacheDiskMiB)<<20, &http.Client{}, log); cerr != nil {
			log.Error("disk prompt cache is off", "err", cerr)
		} else {
			// Through each engine's shim, which every routing method passes:
			// ModelFabric's router, a peer's forwarded request, and llm-d.
			sup.SetSlotCache(cold)
			st := cold.Stats()
			log.Info("disk prompt cache on", "dir", dir, "cap_mib", cfg.CacheDiskMiB,
				"snapshots", st.Snapshots, "mib", st.Bytes>>20)
		}
	}
	cfg.EngineBind = bindAs // as configured, for Server settings
	if err != nil {
		// A node that cannot supervise can still route for its peers, so this
		// is a warning rather than a fatal error.
		log.Warn("model management disabled", "err", err)
	}
	var ui http.Handler
	if cfg.WebUIEnabled() {
		ui = uiHandler(log)
	}
	srv := server.New(m, rt, sup, log, ui)
	// The node runs the same checks the CLI does, against its own paths, so a
	// dashboard can ask a peer what is wrong with it — which is the case no
	// amount of SSH-free tooling could reach before.
	srv.Doctor = func() []doctor.Check {
		return doctor.Run(doctor.Opts{
			Addr:         httpBase(cfg.Listen),
			Version:      version,
			ConfigPath:   *cfgPath,
			ModelsRoot:   cfg.ModelsRoot,
			StateDir:     stateDirOf(cfg),
			Home:         fabricHome(),
			LogDir:       logDir(),
			NodeLogPath:  nodeLogPath(),
			RuntimesRoot: runtimesRoot,
			Runtimes: func(c config.Config) []*runtime.Definition {
				return discoverRuntimes(c, slog.New(slog.NewTextHandler(io.Discard, nil)))
			},
		})
	}
	if err := srv.Capture(cfg.LogBodies, cfg.LogBodiesMax, cfg.LogBodiesKeep, cfg.LogBodiesFile); err != nil {
		return fmt.Errorf("body log file: %w", err)
	}
	if cfg.LogBodies || cfg.LogBodiesFile != "" {
		// Loud on purpose: whoever restarts this node should not have to read
		// the config to discover prompts are being kept.
		log.Warn("request body capture is on",
			"in_memory", cfg.LogBodies, "file", cfg.LogBodiesFile)
	}
	// The node's API key: what require_api_key and the public listener
	// check.
	srv.SetAuth(func() (string, error) { return nodekey.Key(fabricHome()) }, cfg.RequireAPIKey)
	srv.SetTokens(nodekey.Tokens(fabricHome()))
	srv.SetKeyHome(fabricHome())
	srv.SetMCP(filepath.Join(fabricHome(), "mcp.json"), cfg.MCPAllowEphemeral, cfg.MCPAllowConfigured)
	srv.SetCORS(cfg.CORSOrigins)
	srv.SetConfig(*cfgPath, cfg)
	srv.SetFrontDoor(cfg.Listen, cfg.PublicListen)
	srv.SetMeshListen(peerListenAddr(cfg.Listen, selfAddr, cfg.MeshPort))
	srv.SetMeshAdmin(tsid.New(), cfg.MeshAdmin)
	srv.SetSupervisorError(err)
	srv.SetVersion(version)
	srv.SetPreferredStore(filepath.Join(stateDirOf(cfg), "preferred.json"))
	srv.SetRuntimeStore(filepath.Join(stateDirOf(cfg), "runtime.json"))
	if sup != nil && sup.Runtimes() != nil {
		reg := sup.Runtimes()
		srv.SetRuntimesRoot(runtimesRoot(cfg))
		srv.SetRuntimeReloader(func() error {
			for _, err := range reg.Reload(discoverRuntimes(cfg, log)) {
				log.Debug("runtime unavailable", "err", err)
			}
			return nil
		})
	}

	var llmdSup *llmd.LLMD
	if sup != nil {
		llmdSup = llmd.New(llmd.Config{
			Dir:       filepath.Join(fabricHome(), "llmd"),
			Tools:     filepath.Join(fabricHome(), "tools"),
			LogDir:    logDir(),
			Listen:    llmdListen(cfg),
			EPPPort:   9002,
			Metrics:   9092,
			AdminPort: 19000,
			Endpoints: srv.Endpoints,
			Self:      func() string { return m.State().Node },
		}, log)
		llmdState := filepath.Join(stateDirOf(cfg), "llmd.json")
		srv.SetLLMD(llmdSup, llmdState)
		if on, model, opts := server.LLMDEnabled(llmdState); on {
			llmdSup.Start(model, opts)
		}
	}

	go m.Run(ctx)
	if cfg.LLMDEndpointsFile != "" {
		go exportLLMDEndpoints(ctx, srv, cfg.LLMDEndpointsFile, log)
	}

	hs := &http.Server{
		Addr:    cfg.Listen,
		Handler: srv.FrontHandler(),
		// No WriteTimeout: responses stream for as long as generation takes.
		ReadHeaderTimeout: 20 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	log.Info("ModelFabric node up",
		"node", name, "listen", cfg.Listen,
		"mesh_port", cfg.MeshPort, "engines", len(cfg.Engines))
	if len(cfg.Engines) == 0 {
		log.Warn("no local engines configured; this node can route but not serve",
			"config", *cfgPath)
	}

	errc := make(chan error, 1)
	// Bound here rather than in ListenAndServe, so the address is recorded
	// only once this process holds it. A failure goes through errc like any
	// other listener's, which keeps the teardown below.
	if ln, err := net.Listen("tcp", cfg.Listen); err != nil {
		errc <- err
	} else {
		removeRecord, rerr := writeListenRecord(cfg.Listen)
		if rerr != nil {
			log.Warn("could not record the listen address; other mfsh commands will look at the config's",
				"err", rerr)
		}
		defer removeRecord()
		go func() {
			if err := hs.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errc <- err
			}
		}()
	}

	// Peers find each other by probing <tailnet-ip>:mesh_port. A node bound
	// only to loopback — the default, and the right default for apps — is
	// invisible to them, so the mesh port gets its own listener on the
	// tailnet address, serving only what peers need.
	var peer *http.Server
	if meshAddr := peerListenAddr(cfg.Listen, selfAddr, cfg.MeshPort); meshAddr != "" {
		peer = &http.Server{
			Addr:              meshAddr,
			Handler:           srv.PeerHandler(),
			ReadHeaderTimeout: 20 * time.Second,
			IdleTimeout:       120 * time.Second,
		}
		log.Info("mesh listener up", "addr", meshAddr)
		go func() {
			if err := peer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errc <- fmt.Errorf("mesh listener %s: %w", meshAddr, err)
			}
		}()
	} else if selfAddr == "" {
		log.Warn("no tailnet address; this node cannot be discovered by peers")
	}

	// The public front door, for exposure behind Funnel or a TLS proxy.
	var public *http.Server
	if cfg.PublicListen != "" {
		public = &http.Server{
			Addr:              cfg.PublicListen,
			Handler:           srv.PublicHandler(),
			ReadHeaderTimeout: 20 * time.Second,
			IdleTimeout:       120 * time.Second,
		}
		log.Info("public listener up (inference only, API key required)", "addr", cfg.PublicListen)
		go func() {
			if err := public.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errc <- fmt.Errorf("public listener %s: %w", cfg.PublicListen, err)
			}
		}()
	}

	// A listener failure took this straight out of the function, skipping the
	// teardown below — so llm-d and every engine were left running
	// by the one path where something had already gone wrong.
	var listenErr error
	select {
	case listenErr = <-errc:
		log.Error("listener failed; shutting the node down", "err", listenErr)
	case <-ctx.Done():
	}

	log.Info("shutting down")
	if llmdSup != nil {
		llmdSup.Stop()
	}
	if sup != nil {
		sup.Shutdown()
	}
	sctx, scancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer scancel()
	if peer != nil {
		_ = peer.Shutdown(sctx)
	}
	if public != nil {
		_ = public.Shutdown(sctx)
	}
	if err := hs.Shutdown(sctx); err != nil && listenErr == nil {
		return err
	}
	// The listener failure is still the reason this node is stopping, so it is
	// what the caller hears about.
	return listenErr
}

func status(args []string, modelsOnly bool) error {
	name := "status"
	if modelsOnly {
		name = "models"
	}
	fs := flag.NewFlagSet(name, flag.ExitOnError)
	addr := fs.String("addr", defaultAddr, "address of the local ModelFabric node")
	asJSON := fs.Bool("json", false, "emit raw JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// `mfsh models` is a query and should just work; `mfsh status` is a health
	// question, so answering "OFF" is more useful than starting a node behind
	// the user's back to prove it is on.
	if modelsOnly {
		if err := ensureNode(*addr); err != nil {
			return err
		}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(*addr, "/")+"/z/mesh", nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		if modelsOnly {
			return err
		}
		fmt.Printf("Node:  %s\n\n", yellow("OFF"))
		fmt.Printf("%s To start it, run:\n\n    %s\n", dim("(i)"), bold("mfsh up"))
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected status %s", resp.Status)
	}

	var view server.MeshView
	if err := json.NewDecoder(resp.Body).Decode(&view); err != nil {
		return err
	}
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(view)
	}

	if modelsOnly {
		if len(view.Models) == 0 {
			fmt.Println("No models in the mesh.")
			return nil
		}
		t := newTable("MODEL", "NODES")
		for _, mv := range view.Models {
			t.add(mv.ID, dim(strings.Join(mv.Nodes, ", ")))
		}
		t.print()
		return nil
	}

	fmt.Printf("Node:  %s\n\n", green("ON"))

	nodes := newTable("NODE", "STATUS", "INFLIGHT", "MODELS")
	nodes.add(view.Self.Node+" "+dim("(self)"), statusDot(true, "up"),
		fmt.Sprint(view.Self.Inflight), fmt.Sprint(len(view.Self.Models)))
	peers := view.Peers
	sort.Slice(peers, func(i, j int) bool { return peers[i].Node < peers[j].Node })
	for _, p := range peers {
		label := "down"
		if p.Alive {
			label = "up"
		}
		nodes.add(p.Node, statusDot(p.Alive, label),
			fmt.Sprint(p.Inflight), fmt.Sprint(len(p.Models)))
	}
	nodes.print()

	if len(view.Models) > 0 {
		fmt.Println()
		models := newTable("MODEL", "NODES")
		for _, mv := range view.Models {
			models.add(mv.ID, dim(strings.Join(mv.Nodes, ", ")))
		}
		models.print()
	}
	return nil
}

// peerListenAddr decides where peers reach this node. It returns "" when the
// main listener already covers the tailnet address, or when there is no
// tailnet address to bind.
func peerListenAddr(listen, selfAddr string, meshPort int) string {
	if selfAddr == "" {
		return ""
	}
	host, _, err := net.SplitHostPort(listen)
	if err != nil {
		return ""
	}
	// An unspecified or tailnet-addressed main listener already serves peers.
	if host == "" || host == "0.0.0.0" || host == "::" || host == selfAddr {
		return ""
	}
	return net.JoinHostPort(selfAddr, strconv.Itoa(meshPort))
}

// stateDirOf is the node's state directory: the configured one when set.
func stateDirOf(cfg config.Config) string {
	if cfg.StateDir != "" {
		return cfg.StateDir
	}
	return defaultStateDir()
}

// buildSupervisor wires model management and returns any initialization error.
func buildSupervisor(ctx context.Context, cfg config.Config, selfAddr string, m *mesh.Mesh, log *slog.Logger) (*supervisor.Supervisor, error) {
	if cfg.ModelsRoot == "" {
		cfg.ModelsRoot = defaultModelsRoot()
	}
	rts, rtErr := buildRuntimes(cfg, log)

	stateDir := stateDirOf(cfg)
	launcher, err := process.NewLauncher(filepath.Join(stateDir, "instances"), readinessProbe)
	if err != nil {
		return nil, err
	}
	journal, err := ops.NewJournal(filepath.Join(stateDir, "operations"))
	if err != nil {
		return nil, err
	}
	journal.SetLogger(log) // a journal write that fails must be heard

	startup, stop := cfg.LoadTimeouts()
	extraRoots := append([]string(nil), cfg.ExtraModelRoots...)
	// An LM Studio install already holds downloaded models. Reading them means
	// ModelFabric works on a machine that has one, without the operator moving files
	// or editing config. It is read-only: ModelFabric never writes into that tree.
	if !cfg.DisableLMStudioModels {
		if dir := runtime.LMStudioModelsDir(runtime.LMStudioRoot()); dir != "" && dir != cfg.ModelsRoot {
			extraRoots = append(extraRoots, dir)
		}
	}

	lmRoot := ""
	if !cfg.DisableLMStudioModels {
		lmRoot = runtime.LMStudioRoot()
	}

	jitTTL, jitEvict, err := cfg.JITPolicy()
	if err != nil {
		return nil, err
	}
	sup := supervisor.New(supervisor.Config{
		Entrypoint:      cfg.Entrypoint(),
		MaxOutputTokens: cfg.MaxOutputTokens,
		ModelsRoot:      cfg.ModelsRoot,
		ExtraRoots:      extraRoots,
		LMStudioRoot:    lmRoot,
		LogDir:          logDir(),
		DataDir:         fabricHome(),
		EngineBind:      cfg.EngineBind,
		SelfAddr:        selfAddr,
		PortMin:         cfg.PortMin,
		PortMax:         cfg.PortMax,
		StartupTimeout:  startup,
		StopTimeout:     stop,
		JIT:             cfg.JITLoad,
		JITTTL:          jitTTL,
		JITAutoEvict:    jitEvict,
	}, rts, launcher, journal, m, log)
	sup.SetRuntimeError(rtErr)

	pins, err := inputs.NewStore(filepath.Join(stateDir, "pins"))
	if err != nil {
		return nil, err
	}
	pins.VerifyFull = cfg.VerifyFull
	sup.SetPinStore(pins)

	engine := "none"
	if d, err := rts.Default(); err == nil {
		engine = d.Name
	}
	log.Info("model management enabled",
		"models_root", cfg.ModelsRoot,
		"runtime", engine,
		"runtimes", rts.Len(),
		"models", len(sup.Catalog().Models()))
	go journal.RunPruner(ctx)
	return sup, nil
}

// buildRuntimes assembles the runtimes this node can launch.
//
// Configured definitions come first, then engine packages discovered from an
// LM Studio installation. Discovery is what makes a fresh machine work without
// installing a second copy of llama.cpp: the spec's preferred reuse for
// llama.cpp is "evaluate its exact package for execution by the Marie host
// using public interfaces", and those packages declare a launch contract.
func buildRuntimes(cfg config.Config, log *slog.Logger) (*runtime.Registry, error) {
	defs := discoverRuntimes(cfg, log)

	// A runtime chosen with `mfsh runtime select` outranks the config default,
	// since it was set later and on purpose — unless it has since been
	// removed, in which case selection falls back to automatic rather than
	// leaving the node unable to load anything.
	def := cfg.DefaultRuntime
	if sel := server.LoadSelectedRuntime(filepath.Join(stateDirOf(cfg), "runtime.json")); sel != "" {
		found := false
		for _, d := range defs {
			found = found || d.Name == sel
		}
		if found {
			def = sel
		} else {
			log.Warn("selected runtime is no longer installed; selecting automatically", "runtime", sel)
		}
	}
	reg, errs := runtime.NewRegistry(defs, def)
	reg.SetHardware(runtime.Survey(context.Background()))
	for _, err := range errs {
		log.Debug("runtime unavailable", "err", err)
	}
	if reg.Len() == 0 {
		return reg, fmt.Errorf("no usable inference runtime found (looked in %s, LM Studio engine packages, and %q on PATH; install one with `mfsh runtime get`)",
			runtimesRoot(cfg), cfg.LlamaServer)
	}
	return reg, nil
}

// fabricHome is ModelFabric's data directory: models, runtimes, tools, llm-d.
func fabricHome() string {
	if d := os.Getenv("MFSH_HOME"); d != "" {
		return d
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".modelfabric")
	}
	return ".modelfabric"
}

func llmdListen(cfg config.Config) string {
	if cfg.LLMDListen != "" {
		return cfg.LLMDListen
	}
	return "127.0.0.1:8090"
}

// runtimesRoot is where ModelFabric installs its own engine builds.
func runtimesRoot(cfg config.Config) string {
	if cfg.RuntimesRoot != "" {
		return cfg.RuntimesRoot
	}
	if d := os.Getenv("MFSH_RUNTIMES"); d != "" {
		return d
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".modelfabric", "runtimes")
	}
	return "runtimes"
}

// discoverRuntimes lists every runtime definition: declared in config, ModelFabric's
// own installed packages, LM Studio's packages, and the legacy PATH binary.
func discoverRuntimes(cfg config.Config, log *slog.Logger) []*runtime.Definition {
	var defs []*runtime.Definition
	defs = append(defs, cfg.Runtimes...)

	own, err := runtime.DiscoverPackages(runtimesRoot(cfg), runtime.OriginUpstream)
	if err != nil {
		log.Warn("could not scan ModelFabric runtimes", "dir", runtimesRoot(cfg), "err", err)
	}
	defs = append(defs, own...)

	if !cfg.DisableLMStudioRuntimes {
		found, err := runtime.DiscoverLMStudio(runtime.LMStudioRoot())
		if err != nil {
			log.Warn("could not scan LM Studio engine packages", "err", err)
		}
		defs = append(defs, found...)
	}

	// A legacy llama_server setting still works, as the lowest-priority entry.
	if cfg.LlamaServer != "" {
		defs = append(defs, &runtime.Definition{
			Name:       "llama.cpp-path",
			Engine:     "llama.cpp",
			Origin:     runtime.OriginUpstream,
			Entrypoint: cfg.LlamaServer,
		})
	}
	for _, d := range defs {
		if d.ContextLength == 0 {
			d.ContextLength = cfg.ContextLength
		}
		if d.GPULayers == 0 {
			d.GPULayers = cfg.GPULayers
		}
		if d.Parallel == 0 {
			d.Parallel = cfg.Parallel
		}
		if !d.FlashAttention {
			d.FlashAttention = cfg.FlashAttention
		}
	}
	return defs
}

// readinessProbe binds readiness to the expected endpoint AND model, so an
// unrelated server already on the port cannot satisfy our launch.
func readinessProbe(ctx context.Context, spec process.LaunchSpec) error {
	served := spec.ServedModel
	if served == "" {
		served = spec.Model
	}
	return runtime.Ready(ctx, spec.Engine, spec.Endpoint, served)
}

// exportLLMDEndpoints keeps llm-d's file-discovery input in step with the mesh.
//
// The file is only rewritten when its contents actually change, because the
// EPP watches it with fsnotify and every write wakes its reconciler.
func exportLLMDEndpoints(ctx context.Context, srv *server.Server, path string, log *slog.Logger) {
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	warned := false
	for {
		endpoints := srv.Endpoints()

		// Loopback endpoints are useless to an EPP running anywhere else. Say
		// so once rather than silently publishing addresses nothing can dial.
		if !warned {
			for _, e := range endpoints {
				if e.Unreachable() {
					log.Warn("llm-d endpoints are bound to loopback and unreachable from other hosts; set engine_bind to \"tailnet\" (Server settings → Engine Bind)",
						"endpoint", e.Name)
					warned = true
					break
				}
			}
		}
		if changed, err := discovery.WriteFile(path, endpoints); err != nil {
			log.Error("write llm-d endpoints file", "path", path, "err", err)
		} else if changed {
			log.Info("llm-d endpoints updated", "path", path, "endpoints", len(endpoints))
		}

		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// defaultModelsRoot is where models live when the config does not say.
// Having a default means a fresh install can list and load without editing
// anything first.
func defaultModelsRoot() string {
	if d := os.Getenv("MFSH_MODELS"); d != "" {
		return d
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".modelfabric", "models")
	}
	return "models"
}

func defaultConfigPath() string {
	if p := os.Getenv("MFSH_CONFIG"); p != "" {
		return p
	}
	if dir, err := os.UserConfigDir(); err == nil {
		return dir + "/modelfabric/config.json"
	}
	return "mfsh.json"
}

// requireExplicitConfig refuses a config file that was asked for and is not
// there. Load treats a missing file as "run on defaults", which is right for
// the default path on a fresh install and wrong for one named on purpose: an
// entrypoint was started with -config ~/.config/ModelFabric/config.json, a
// capitalisation its file never had, and ran for hours as a GPU node with no
// public listener, without a word.
func requireExplicitConfig(fs *flag.FlagSet, path string) error {
	explicit := os.Getenv("MFSH_CONFIG") != ""
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "config" {
			explicit = true
		}
	})
	if !explicit {
		return nil
	}
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return fmt.Errorf("config file %s does not exist; the node was told to use it, so it will not run on defaults instead. Fix the path, or start without -config and MFSH_CONFIG to use the default location", path)
	}
	return nil
}
