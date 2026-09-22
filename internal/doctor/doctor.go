package doctor

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/gregbugaj/modelfabric/internal/catalog"
	"github.com/gregbugaj/modelfabric/internal/config"
	"github.com/gregbugaj/modelfabric/internal/llmd"
	"github.com/gregbugaj/modelfabric/internal/mesh"
	"github.com/gregbugaj/modelfabric/internal/nodekey"
	"github.com/gregbugaj/modelfabric/internal/osproc"
	"github.com/gregbugaj/modelfabric/internal/runtime"
	"github.com/gregbugaj/modelfabric/internal/tscli"
)

// Package doctor checks everything ModelFabric depends on and says what to do about
// anything wrong. It never starts the node or changes anything: a diagnosis
// must work on exactly the broken state it is asked about, and must not "fix"
// it by accident.
//
// The checks live here rather than in the CLI so the node can serve the same
// report — including for a peer, which is the case the CLI cannot reach at
// all. Rendering stays with the caller: this package decides what is true,
// not how to colour it.

type Status string

const (
	StatusOK   Status = "ok"
	StatusInfo Status = "info"
	StatusWarn Status = "warn"
	StatusFail Status = "fail"
)

type Check struct {
	Section string `json:"section"`
	Name    string `json:"name"`
	Status  Status `json:"status"`
	Detail  string `json:"detail"`
	Fix     string `json:"fix,omitempty"`
}

// Opts is what the caller knows and this package does not: where the node is,
// and the paths and version the binary was built with.
type Opts struct {
	Addr         string
	Version      string
	ConfigPath   string
	ModelsRoot   string
	StateDir     string
	Home         string
	LogDir       string
	NodeLogPath  string
	RuntimesRoot func(config.Config) string
	// Runtimes discovers installed engine builds. It is a func because the
	// CLI and the node build their registries differently, and doctor must
	// report whichever the caller actually uses.
	Runtimes func(config.Config) []*runtime.Definition
}

// Run performs every check and returns them in report order.
func Run(o Opts) []Check {
	d := &doctor{opts: o, addr: o.Addr, http: &http.Client{Timeout: 5 * time.Second}}
	d.checkInstall()
	d.checkNode()
	d.checkTailnet()
	if d.cfg.Entrypoint() {
		// An entrypoint has no GPU, runtime or models by design; what matters
		// is that it can see the nodes that do.
		d.checkEntrypoint()
	} else {
		d.checkHardwareAndRuntimes()
		d.checkModels()
		d.checkEngines()
	}
	d.checkLLMD()
	// Last, so it can report the pids the checks above discovered.
	d.checkProcesses()
	d.checkSecurity()
	return d.checks
}

type doctor struct {
	opts   Opts
	addr   string
	cfg    config.Config
	checks []Check
	http   *http.Client

	nodeUp    bool
	instances []doctorInstance

	// Carried between checks for the Processes section: each is already
	// fetched by the check above it and was previously discarded.
	nodePID     int
	nodeStarted time.Time
}

type doctorInstance struct {
	ID, Model, Runtime string
	PID, Port          int
}

func (d *doctor) add(section, name string, st Status, detail, fix string) {
	d.checks = append(d.checks, Check{section, name, st, detail, fix})
}

// get reads a node endpoint without auto-starting the node.
func (d *doctor) get(path string, out any) error {
	resp, err := d.http.Get(strings.TrimRight(d.addr, "/") + path)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

/* ---------- install ---------- */

func (d *doctor) checkInstall() {
	const s = "modelfabric"
	exe, _ := os.Executable()
	d.add(s, "version", StatusInfo, fmt.Sprintf("%s (%s, %s/%s) %s", d.opts.Version, goruntime.Version(),
		goruntime.GOOS, goruntime.GOARCH, exe), "")

	path := d.opts.ConfigPath
	cfg, err := config.Load(path)
	switch {
	case err != nil:
		d.add(s, "config", StatusFail, fmt.Sprintf("%s: %v", path, err), "fix the file, or move it aside to run on defaults")
	case fileExists(path):
		d.add(s, "config", StatusOK, path, "")
	default:
		d.add(s, "config", StatusOK, "no config file; running on defaults", "")
	}
	d.cfg = cfg

	models := cfg.ModelsRoot
	if models == "" {
		models = d.opts.ModelsRoot
	}
	for _, dir := range []struct{ name, path string }{
		{"data directory", d.opts.Home},
		{"models directory", models},
		{"runtimes directory", d.opts.RuntimesRoot(cfg)},
		{"state directory", d.opts.StateDir},
		{"log directory", d.opts.LogDir},
	} {
		if err := writable(dir.path); err != nil {
			d.add(s, dir.name, StatusFail, fmt.Sprintf("%s: %v", dir.path, err), "check ownership and permissions of "+dir.path)
			continue
		}
		d.add(s, dir.name, StatusOK, dir.path, "")
	}

	if free, err := freeBytes(d.opts.Home); err == nil {
		switch {
		case free < 2<<30:
			d.add(s, "disk space", StatusFail, humanBytes(int64(free))+" free under "+d.opts.Home, "free space before downloading models or runtimes")
		case free < 20<<30:
			d.add(s, "disk space", StatusWarn, humanBytes(int64(free))+" free — a single 27B model is ~17GB", "")
		default:
			d.add(s, "disk space", StatusOK, humanBytes(int64(free))+" free", "")
		}
	}
}

/* ---------- node ---------- */

func (d *doctor) checkNode() {
	const s = "Node"
	listen := strings.TrimPrefix(strings.TrimPrefix(d.addr, "http://"), "https://")
	if waitHealthy(d.addr, 800*time.Millisecond) != nil {
		// Is something else sitting on the port? LM Studio's own server
		// also defaults to 1234, and would answer requests meant for us.
		if l, err := net.Listen("tcp", listen); err != nil {
			d.add(s, "node", StatusFail, "not running, and "+listen+" is held by another program",
				"stop that program (LM Studio's server also uses 1234), or set \"listen\" in "+d.opts.ConfigPath)
		} else {
			l.Close()
			d.add(s, "node", StatusWarn, "not running", "mfsh up")
		}
		return
	}
	d.nodeUp = true
	var b struct {
		Version    string    `json:"version"`
		Executable string    `json:"executable"`
		ExeModTime time.Time `json:"exe_mod_time"`
		Started    time.Time `json:"started"`
		PID        int       `json:"pid"`
	}
	if err := d.get("/z/version", &b); err != nil {
		d.add(s, "node", StatusWarn, "running, but too old to report its version",
			"restart it on this build: mfsh down && mfsh up")
		return
	}
	// Answering is not the same as being ModelFabric. LM Studio's server also
	// defaults to 1234, and answers enough to look healthy, which would make
	// every check below report on the wrong program.
	if b.Version == "" {
		d.nodeUp = false
		d.add(s, "node", StatusFail, "something other than ModelFabric is answering on "+listen,
			"stop that program (LM Studio's server also uses 1234), or set \"listen\" in "+d.opts.ConfigPath)
		return
	}
	d.nodePID, d.nodeStarted = b.PID, b.Started
	d.add(s, "node", StatusOK, fmt.Sprintf("running on %s, pid %d, up %s", listen, b.PID,
		time.Since(b.Started).Round(time.Second)), "")
	// A binary rebuilt after the node started is the classic "I fixed it
	// but nothing changed".
	if fi, err := os.Stat(b.Executable); err == nil && fi.ModTime().After(b.ExeModTime.Add(time.Second)) {
		d.add(s, "binary", StatusWarn, b.Executable+" was rebuilt after the node started; it is running old code",
			"mfsh down && mfsh up")
	} else if exe, _ := os.Executable(); b.Executable != "" && exe != b.Executable {
		d.add(s, "binary", StatusInfo, "node runs "+b.Executable+"; this CLI is "+exe, "")
	} else {
		d.add(s, "binary", StatusOK, "node runs the current build", "")
	}
}

/* ---------- tailnet ---------- */

func (d *doctor) checkTailnet() {
	const s = "Tailnet"
	if _, err := exec.LookPath(tscli.Path()); err != nil {
		d.add(s, "tailscale", StatusWarn, "not installed: this node works alone, with no mesh",
			"install Tailscale and join the tailnet to share GPUs between machines")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	name, addr, err := mesh.SelfIdentity(ctx)
	cancel()
	if err != nil {
		d.add(s, "tailscale", StatusWarn, "not connected: "+err.Error(), "tailscale up")
		return
	}
	d.add(s, "tailscale", StatusOK, fmt.Sprintf("%s at %s", name, addr), "")
	if !d.nodeUp {
		return
	}

	meshPort := d.cfg.MeshPort
	if meshPort == 0 {
		meshPort = 1234
	}
	peerAddr := net.JoinHostPort(addr, strconv.Itoa(meshPort))
	if resp, err := d.http.Get("http://" + peerAddr + "/healthz"); err != nil {
		d.add(s, "mesh listener", StatusFail, "peers cannot reach this node at "+peerAddr+": "+err.Error(),
			"check that nothing else holds that port and the node log (mfsh log engine is for engines; see "+d.opts.NodeLogPath+")")
	} else {
		resp.Body.Close()
		d.add(s, "mesh listener", StatusOK, "peers reach this node at "+peerAddr, "")
	}

	var m struct {
		Peers []struct {
			Node   string   `json:"node"`
			Addr   string   `json:"addr"`
			Alive  bool     `json:"alive"`
			Models []string `json:"models"`
		} `json:"peers"`
	}
	if err := d.get("/z/mesh", &m); err != nil {
		return
	}
	alive, up := 0, []string{}
	for _, p := range m.Peers {
		if p.Alive {
			alive++
			up = append(up, fmt.Sprintf("%s (%s)", p.Node, plural(len(p.Models), "model")))
		}
	}
	switch {
	case len(m.Peers) == 0:
		d.add(s, "peers", StatusInfo, "no other ModelFabric nodes on the tailnet", "run ModelFabric on another machine to share its GPU")
	case alive == len(m.Peers):
		d.add(s, "peers", StatusOK, strings.Join(up, ", "), "")
	default:
		var dead []string
		for _, p := range m.Peers {
			if !p.Alive {
				dead = append(dead, p.Node)
			}
		}
		d.add(s, "peers", StatusInfo, fmt.Sprintf("%d up (%s); unreachable: %s", alive, strings.Join(up, ", "),
			strings.Join(dead, ", ")), "a peer that is off or not running ModelFabric is expected; start it there with mfsh up")
	}
}

/* ---------- hardware & runtimes ---------- */

func (d *doctor) checkHardwareAndRuntimes() {
	const s = "Runtimes"
	hw := runtime.Survey(context.Background())
	if len(hw.GPUs) == 0 {
		gpu := "no NVIDIA GPU"
		if hw.Vulkan {
			gpu += "; Vulkan available"
		}
		d.add(s, "hardware", StatusInfo, fmt.Sprintf("%s, %s RAM, %s", hw.CPU, humanBytes(int64(hw.MemoryMB)<<20), gpu), "")
	} else {
		for _, g := range hw.GPUs {
			// Unified memory has no compute capability, driver version or CUDA
			// behind it: printing those fields empty reads as missing data.
			if g.Unified {
				d.add(s, "hardware", StatusInfo, fmt.Sprintf("%s, %s RAM, %s for the GPU (unified, Metal)",
					hw.CPU, humanBytes(int64(hw.MemoryMB)<<20), humanBytes(int64(g.MemoryMB)<<20)), "")
				continue
			}
			d.add(s, "hardware", StatusInfo, fmt.Sprintf("%s, %s, compute %s, driver %s (CUDA %s)",
				g.Name, humanBytes(int64(g.MemoryMB)<<20), g.ComputeCap, g.Driver, hw.CUDAVersion), "")
		}
	}

	type rt struct {
		Name    string   `json:"name"`
		Default bool     `json:"default"`
		Fit     string   `json:"fit"`
		Reasons []string `json:"reasons"`
		Managed bool     `json:"managed"`
	}
	var body struct {
		Runtimes  []rt   `json:"runtimes"`
		Selection string `json:"selection"`
	}
	if d.nodeUp {
		if err := d.get("/api/v1/runtimes", &body); err != nil {
			d.add(s, "runtimes", StatusFail, "the node could not list runtimes: "+err.Error(), "mfsh log engine; "+d.opts.NodeLogPath)
			return
		}
	} else {
		// Offline: discover as the node would, without starting it.
		// NewRegistry drops definitions whose Resolve failed and says which,
		// so discarding the error made doctor report a clean runtime list for
		// a machine that has a broken one — the opposite of its job.
		reg, regErrs := runtime.NewRegistry(d.opts.Runtimes(d.cfg), d.cfg.DefaultRuntime)
		for _, e := range regErrs {
			d.add(s, "runtime discovery", StatusWarn, e.Error(), "mfsh runtime ls")
		}
		reg.SetHardware(hw)
		def, _ := reg.Default()
		for _, r := range reg.All() {
			fit, why := r.Check(hw)
			body.Runtimes = append(body.Runtimes, rt{Name: r.Name, Fit: string(fit), Reasons: why,
				Default: def != nil && def.Name == r.Name, Managed: r.Provenance() != nil})
		}
		body.Selection = "auto"
	}
	if len(body.Runtimes) == 0 {
		d.add(s, "runtimes", StatusFail, "no inference engine found", "mfsh runtime get")
		return
	}
	fits := 0
	var def *rt
	for i := range body.Runtimes {
		if body.Runtimes[i].Fit == "yes" {
			fits++
		}
		if body.Runtimes[i].Default {
			def = &body.Runtimes[i]
		}
	}
	d.add(s, "runtimes", StatusOK, fmt.Sprintf("%d installed, %d known to fit this machine", len(body.Runtimes), fits), "")
	switch {
	case def == nil:
		d.add(s, "default runtime", StatusFail, "none can be chosen", "mfsh runtime select")
	case def.Fit == "no":
		d.add(s, "default runtime", StatusFail, def.Name+" cannot run here: "+strings.Join(def.Reasons, "; "),
			"mfsh runtime select -auto, or mfsh runtime get")
	case def.Fit == "unknown":
		d.add(s, "default runtime", StatusWarn, def.Name+": "+strings.Join(def.Reasons, "; "),
			"mfsh runtime get installs a build tested on this machine")
	default:
		how := "chosen automatically"
		if body.Selection != "" && body.Selection != "auto" {
			how = "pinned"
		}
		d.add(s, "default runtime", StatusOK, def.Name+" ("+how+")", "")
	}

	if lim, unlimited := osproc.MemlockLimit(); !unlimited && lim > 0 {
		d.add(s, "memlock", StatusInfo, fmt.Sprintf("limit %s: models larger than that load without mlock (by design)",
			humanBytes(int64(lim))), "")
	}
}

/* ---------- models ---------- */

func (d *doctor) checkModels() {
	const s = "Models"
	roots := []string{d.cfg.ModelsRoot}
	if roots[0] == "" {
		roots[0] = d.opts.ModelsRoot
	}
	roots = append(roots, d.cfg.ExtraModelRoots...)
	lm := ""
	if !d.cfg.DisableLMStudioModels {
		lm = runtime.LMStudioRoot()
		if dir := runtime.LMStudioModelsDir(lm); dir != "" {
			roots = append(roots, dir)
		}
	}
	hub := catalog.LoadHub(lm)
	cat, err := catalog.ScanRootsWithHub(hub, roots...)
	if err != nil {
		d.add(s, "catalog", StatusFail, err.Error(), "check models_root in "+d.opts.ConfigPath)
		return
	}
	models := cat.Models()
	var total int64
	for _, m := range models {
		total += m.SizeBytes
	}
	if len(models) == 0 {
		d.add(s, "catalog", StatusWarn, "no models found in "+strings.Join(roots, ", "), "mfsh get qwen/qwen3-0.6b")
	} else {
		d.add(s, "catalog", StatusOK, fmt.Sprintf("%s, %s, in %s", plural(len(models), "model"), humanBytes(total), strings.Join(roots, ", ")), "")
	}
	if lm != "" {
		d.add(s, "LM Studio", StatusInfo, "found at "+lm+"; its models and engines are used read-only", "")
	}
	for _, e := range hub.Entries() {
		if e.SpecErr != nil {
			d.add(s, "model.yaml", StatusWarn, e.ID+": not used ("+e.SpecErr.Error()+"); loads with engine defaults", "")
		}
	}
	for _, m := range models {
		if m.Spec == nil && m.HubID == "" && m.Architecture == "" {
			d.add(s, "metadata", StatusWarn, m.Key+": no GGUF metadata could be read", "re-download it with mfsh get")
		}
	}
}

/* ---------- engines ---------- */

func (d *doctor) checkEngines() {
	const s = "Engines"
	if d.nodeUp {
		var body struct {
			Models []struct {
				Key       string `json:"key"`
				Instances []struct {
					ID     string         `json:"id"`
					PID    int            `json:"pid"`
					Port   int            `json:"port"`
					Config map[string]any `json:"config"`
					Shim   *struct {
						Port          int  `json:"port"`
						KVGauge       bool `json:"kv_gauge"`
						LiveTokens    bool `json:"live_tokens"`
						OutputCeiling int  `json:"output_ceiling"`
						PromptCache   bool `json:"prompt_cache"`
					} `json:"shim"`
				} `json:"loaded_instances"`
			} `json:"models"`
		}
		if err := d.get("/api/v1/models", &body); err == nil {
			for _, m := range body.Models {
				for _, i := range m.Instances {
					rt, _ := i.Config["runtime"].(string)
					d.instances = append(d.instances, doctorInstance{ID: i.ID, Model: m.Key, Runtime: rt, PID: i.PID, Port: i.Port})
					// The shim is a proxy in front of the engine that nothing
					// else shows: say it is there, and what it does to a
					// request on its way through.
					name := "shim · " + m.Key
					if i.Shim == nil {
						d.add("Shims", name, StatusInfo, fmt.Sprintf("none: %s is dialled directly (it publishes its own metrics)", i.ID), "")
						continue
					}
					d.add("Shims", name, StatusOK, fmt.Sprintf(":%d in front of :%d (%s): %s", i.Shim.Port, i.Port, i.ID,
						shimJobs(i.Shim.KVGauge, i.Shim.LiveTokens, i.Shim.OutputCeiling, i.Shim.PromptCache)), "")
				}
			}
		}
		var mesh struct {
			Self struct {
				Engines []struct {
					Name          string `json:"name"`
					Healthy       bool   `json:"healthy"`
					Error         string `json:"error"`
					ContextLength int    `json:"context_length"`
					ContextNote   string `json:"context_note"`
				} `json:"engines"`
			} `json:"self"`
		}
		if err := d.get("/z/mesh", &mesh); err == nil {
			for _, e := range mesh.Self.Engines {
				if !e.Healthy {
					d.add(s, e.Name, StatusFail, "not answering: "+e.Error, "mfsh log engine "+e.Name)
					continue
				}
				// An engine running a different context than it was loaded with
				// is the quietest failure ModelFabric has had: every long conversation
				// routed there overflows at a limit nothing reported, and the
				// only tasks a benchmark run failed to solve died exactly there.
				if e.ContextNote != "" {
					d.add(s, "context", StatusWarn, e.Name+": "+e.ContextNote,
						"reload it and check the settings were applied: mfsh load <model> -context N")
				}
			}
		}
		if len(d.instances) == 0 {
			d.add(s, "loaded", StatusInfo, "no models loaded", "")
		} else {
			var names []string
			for _, i := range d.instances {
				names = append(names, i.Model)
			}
			d.add(s, "loaded", StatusOK, strings.Join(names, ", "), "")
		}
	}

	// Every llama-server on the machine, and whose it is. Unowned ones hold
	// VRAM the node cannot see, which surfaces later as a mysterious
	// out-of-memory load failure.
	ours := map[int]bool{}
	for _, i := range d.instances {
		ours[i.PID] = true
	}
	gpuMem := gpuMemoryByPID()
	for _, p := range llamaServers() {
		mem := ""
		if mb, ok := gpuMem[p.pid]; ok {
			mem = fmt.Sprintf(", %s VRAM", humanBytes(int64(mb)<<20))
		}
		switch {
		case ours[p.pid]:
			continue
		case p.mfsh && !d.nodeUp:
			d.add(s, "engine", StatusInfo, fmt.Sprintf("pid %d (%s%s) outlived its node; the next mfsh up adopts it", p.pid, p.alias, mem), "")
		case p.mfsh:
			d.add(s, "orphan engine", StatusWarn, fmt.Sprintf("pid %d (%s%s) was started by ModelFabric but the node does not own it", p.pid, p.alias, mem),
				fmt.Sprintf("kill %d", p.pid))
		case p.lmstudio:
			d.add(s, "LM Studio engine", StatusInfo, fmt.Sprintf("pid %d is LM Studio's own model%s — it shares the GPU with ModelFabric", p.pid, mem), "")
		default:
			d.add(s, "other engine", StatusInfo, fmt.Sprintf("pid %d: a llama-server not started by ModelFabric%s", p.pid, mem), "")
		}
	}
	// Other GPU users matter for the same reason.
	var others []string
	for pid, mb := range gpuMem {
		if !ours[pid] && !isLlamaServer(pid) && mb >= 1024 {
			others = append(others, fmt.Sprintf("%s (pid %d, %s)", procName(pid), pid, humanBytes(int64(mb)<<20)))
		}
	}
	sort.Strings(others)
	if len(others) > 0 {
		d.add(s, "other GPU use", StatusInfo, strings.Join(others, ", "), "")
	}
}

type llamaProc struct {
	pid            int
	alias          string
	mfsh, lmstudio bool
}

func llamaServers() []llamaProc {
	var out []llamaProc
	for _, pr := range osproc.All() {
		if pr.Argv0Base() != "llama-server" {
			continue
		}
		p := llamaProc{pid: pr.PID, lmstudio: strings.Contains(pr.Argv[0], ".lmstudio")}
		// ModelFabric names every engine with --alias; LM Studio uses --api-key.
		if alias, ok := pr.Arg("--alias"); ok {
			p.alias, p.mfsh, p.lmstudio = alias, true, false
		}
		out = append(out, p)
	}
	return out
}

func isLlamaServer(pid int) bool {
	argv, err := osproc.Argv(pid)
	return err == nil && len(argv) > 0 && filepath.Base(argv[0]) == "llama-server"
}

func procName(pid int) string { return osproc.Name(pid) }

// gpuMemoryByPID reads per-process VRAM from nvidia-smi, in MiB.
func gpuMemoryByPID() map[int]int {
	out := map[int]int{}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	b, err := exec.CommandContext(ctx, "nvidia-smi", "--query-compute-apps=pid,used_memory",
		"--format=csv,noheader,nounits").Output()
	if err != nil {
		return out
	}
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		f := strings.Split(line, ",")
		if len(f) != 2 {
			continue
		}
		pid, err1 := strconv.Atoi(strings.TrimSpace(f[0]))
		mb, err2 := strconv.Atoi(strings.TrimSpace(f[1]))
		if err1 == nil && err2 == nil {
			out[pid] += mb
		}
	}
	return out
}

/* ---------- llm-d ---------- */

func (d *doctor) checkLLMD() {
	const s = "llm-d"
	cfg := llmd.Config{Tools: filepath.Join(d.opts.Home, "tools")}
	if !cfg.Installed() {
		d.add(s, "binaries", StatusInfo, "not installed (optional: llm-d's scheduler for one model)", "mfsh llmd install")
		return
	}
	d.add(s, "binaries", StatusOK, fmt.Sprintf("EPP %s and Envoy %s installed; they run as processes, no containers",
		llmd.EPPVersion, llmd.EnvoyVersion), "")
	if !d.nodeUp {
		return
	}
	var st llmd.Status
	if err := d.get("/api/v1/llmd", &st); err != nil {
		return
	}
	l := &st
	switch {
	case l.State == "disabled":
		d.add(s, "llm-d", StatusInfo, "off", "mfsh llmd enable <model>")
	case l.State == "running" && len(l.Endpoints) == 0:
		d.add(s, "llm-d", StatusWarn, "running for "+l.Model+", but no engine serves it", "mfsh load "+l.Model)
	case l.State == "running":
		cal := "default prefill rate"
		if l.Calibrated {
			cal = "prefill rate measured"
		}
		d.add(s, "llm-d", StatusOK, fmt.Sprintf("scheduling %s with the %s profile across %s (%s); %s: %.0f tok/s",
			l.Model, l.Profile, plural(len(l.Endpoints), "engine"), strings.Join(l.Endpoints, ", "), cal, l.PeakPrefill), "")
	case l.State == "starting":
		d.add(s, "llm-d", StatusInfo, "starting for "+l.Model, "")
	default:
		d.add(s, "llm-d", StatusFail, l.State+": "+l.Error, "mfsh llmd status; logs in "+filepath.Join(d.opts.LogDir, "llmd-*.log"))
	}
}

/* ---------- security ---------- */

func (d *doctor) checkSecurity() {
	const s = "Security"
	listen := d.cfg.Listen
	if listen == "" {
		listen = config.Default().Listen
	}
	if host, _, err := net.SplitHostPort(listen); err == nil && !isLoopbackHost(host) {
		d.add(s, "management API", StatusWarn, "listen "+listen+" exposes load/unload beyond this machine",
			"set \"listen\" to 127.0.0.1:1234; peers use the separate tailnet listener")
	} else {
		d.add(s, "management API", StatusOK, "loopback only ("+listen+")", "")
	}
	switch bind := d.cfg.EngineBind; {
	case bind == "0.0.0.0" || bind == "::" || bind == "[::]":
		d.add(s, "engines", StatusWarn, "engine_bind "+bind+" puts unauthenticated engines on every interface, the LAN included",
			"set engine_bind to \"tailnet\", this node's Tailscale address")
	case bind == "" || isLoopbackHost(bind):
		d.add(s, "engines", StatusOK, "loopback only", "")
	case d.cfg.BindsTailnet():
		fix := ""
		if bind != config.EngineBindTailnet {
			fix = "engine_bind \"tailnet\" says the same; the address is Tailscale's and is looked up at each start either way"
		}
		d.add(s, "engines", StatusOK, "bound to this node's tailnet address (for llm-d); reachable over the tailnet only", fix)
	default:
		// Calling any non-loopback address "tailnet only" was a claim doctor
		// had not checked: a LAN address here exposes unauthenticated engines
		// to the LAN, which is exactly what the wildcard case warns about.
		d.add(s, "engines", StatusWarn,
			"engine_bind "+bind+" is not a tailnet address (100.64.0.0/10 or fd7a:115c:a1e0::/48); engines take no key",
			"set engine_bind to \"tailnet\", or leave it unset for loopback")
	}
	if d.cfg.RequireAPIKey {
		d.add(s, "API key", StatusOK, "required on "+listen+" (Authorization: Bearer)", "")
	} else {
		d.add(s, "API key", StatusInfo, "not required on "+listen+" (loopback, as LM Studio)", "")
	}
	if pub := d.cfg.PublicListen; pub != "" {
		if host, _, err := net.SplitHostPort(pub); err == nil && !isLoopbackHost(host) {
			d.add(s, "public listener", StatusWarn, pub+" serves plain HTTP beyond this machine; keys and prompts travel unencrypted",
				"set public_listen to 127.0.0.1:<port> and publish it with TLS: `tailscale funnel <port>` or a proxy such as Caddy")
		} else {
			d.add(s, "public listener", StatusOK, pub+": inference only, API key required; publish it with Funnel or a TLS proxy", "")
		}
	}
	// The node's API key: it authorizes every request to this node, so a
	// world-readable key file is the whole door left open.
	if kp := nodekey.Path(d.opts.Home); fileExists(kp) {
		if fi, err := os.Stat(kp); err == nil && fi.Mode().Perm()&0o077 != 0 {
			d.add(s, "API key", StatusFail, fmt.Sprintf("%s is readable by others (mode %o)", kp, fi.Mode().Perm()),
				"chmod 600 "+kp)
		}
	}
}

// checkEntrypoint reports what a model-less entrypoint can route to.
func (d *doctor) checkEntrypoint() {
	const s = "Entrypoint"
	var mesh struct {
		Peers []struct {
			Node   string   `json:"node"`
			Alive  bool     `json:"alive"`
			Models []string `json:"models"`
		} `json:"peers"`
	}
	if err := d.get("/z/mesh", &mesh); err != nil {
		d.add(s, "mesh", StatusFail, "cannot read the mesh: "+err.Error(), "mfsh up")
		return
	}
	models := map[string]bool{}
	var nodes []string
	for _, p := range mesh.Peers {
		if !p.Alive {
			continue
		}
		nodes = append(nodes, p.Node)
		for _, m := range p.Models {
			models[m] = true
		}
	}
	switch {
	case len(nodes) == 0:
		d.add(s, "role", StatusFail, "entrypoint, but no other node is reachable: there is nothing to route to",
			"check `tailscale status` and that ModelFabric runs on the GPU nodes")
	case len(models) == 0:
		d.add(s, "role", StatusWarn, fmt.Sprintf("entrypoint; %s reachable, but none serves a model", plural(len(nodes), "node")),
			"load a model on a GPU node")
	default:
		sort.Strings(nodes)
		d.add(s, "role", StatusOK, fmt.Sprintf("entrypoint: routes %s from %s (%s); runs none itself",
			plural(len(models), "model"), plural(len(nodes), "node"), strings.Join(nodes, ", ")), "")
	}
}

func isLoopbackHost(h string) bool {
	if h == "localhost" {
		return true
	}
	ip := net.ParseIP(strings.Trim(h, "[]"))
	return ip != nil && ip.IsLoopback()
}

/* ---------- output ---------- */

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// writable proves a file can be written where this directory is or would be,
// without creating it. doctor is a diagnostic: it said it changes nothing, but
// this used to MkdirAll every path it checked, so running it created
// $MFSH_HOME and the models and runtime directories as a side effect.
func writable(dir string) error {
	probe := dir
	for {
		fi, err := os.Stat(probe)
		if err == nil {
			if !fi.IsDir() {
				return fmt.Errorf("%s is not a directory", probe)
			}
			break
		}
		if !os.IsNotExist(err) {
			return err
		}
		parent := filepath.Dir(probe)
		if parent == probe {
			return fmt.Errorf("no existing parent of %s", dir)
		}
		probe = parent
	}
	// The temp file is created and removed in a directory that already
	// existed, so nothing is left behind and nothing new is made.
	f, err := os.CreateTemp(probe, ".mfsh-doctor-*")
	if err != nil {
		return err
	}
	name := f.Name()
	f.Close()
	return os.Remove(name)
}

func freeBytes(dir string) (uint64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, err
	}
	return st.Bavail * uint64(st.Bsize), nil
}

// humanBytes formats a size for a person. There are already copies of this in
// cmd/mfsh and internal/server; a fourth home for it would be worth having,
// but moving a helper used across the CLI is not this change.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%dB", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f%cB", float64(n)/float64(div), "KMGTPE"[exp])
}

// plural is the CLI's, kept here so a check can phrase its own count.
func plural(n int, word string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, word)
	}
	// A bare "s" gave "3 processs". Only the endings that actually appear in
	// this report are handled; a general pluralizer here would be a lie about
	// how much English it knows.
	suffix := "s"
	switch {
	case strings.HasSuffix(word, "s"), strings.HasSuffix(word, "x"),
		strings.HasSuffix(word, "ch"), strings.HasSuffix(word, "sh"):
		suffix = "es"
	}
	return fmt.Sprintf("%d %s%s", n, word, suffix)
}

// waitHealthy reports whether a node answers /healthz within the timeout. A
// malformed listen address makes the request constructor fail and return nil,
// which Do would dereference — so the error is checked rather than the check
// panicking on a broken config.
func waitHealthy(base string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var last error
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/healthz", nil)
		if err != nil {
			cancel()
			return err
		}
		resp, err := http.DefaultClient.Do(req)
		cancel()
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
			err = fmt.Errorf("%s", resp.Status)
		}
		last = err
		time.Sleep(200 * time.Millisecond)
	}
	return last
}

// shimJobs says in a line what a shim does to a request on its way through.
func shimJobs(kv, tokens bool, ceiling int, cache bool) string {
	var jobs []string
	if kv {
		jobs = append(jobs, "KV-cache gauge for llm-d")
	}
	if tokens {
		jobs = append(jobs, "live tokens")
	}
	if ceiling > 0 {
		jobs = append(jobs, fmt.Sprintf("output ceiling %d", ceiling))
	}
	if cache {
		jobs = append(jobs, "disk prompt cache, for every routing method")
	} else {
		jobs = append(jobs, "disk prompt cache off")
	}
	return strings.Join(jobs, " · ")
}
