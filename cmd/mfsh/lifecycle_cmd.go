package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// parsePositional resumes flag parsing after each positional argument; Go's flag package stops at the first non-flag.
func parsePositional(fs *flag.FlagSet, args []string) ([]string, error) {
	var positional []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		if fs.NArg() == 0 {
			return positional, nil
		}
		positional = append(positional, fs.Arg(0))
		args = fs.Args()[1:]
	}
}

type apiModel struct {
	Arch            string        `json:"architecture"`
	Key             string        `json:"key"`
	Type            string        `json:"type"`
	DisplayName     string        `json:"display_name"`
	Format          string        `json:"format"`
	Quantization    string        `json:"quantization"`
	ParamsString    string        `json:"params_string"`
	SizeBytes       int64         `json:"size_bytes"`
	Capabilities    []string      `json:"capabilities"`
	Variants        int           `json:"variants"`
	LoadedInstances []apiInstance `json:"loaded_instances"`
}

type apiInstance struct {
	ID         string         `json:"id"`
	Config     map[string]any `json:"config"`
	Origin     string         `json:"origin"`
	TTLSeconds int            `json:"ttl"`
	LastUsed   time.Time      `json:"last_used"`
}

func call(ctx context.Context, addr, method, path string, body any, out any) error {
	// Auto-start here so direct API commands such as load also work when the node is stopped.
	if err := ensureNode(addr); err != nil {
		return err
	}
	var rdr *bytes.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(b)
	} else {
		rdr = bytes.NewReader(nil)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(addr, "/")+path, rdr)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("is the node running? (%w)", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		var e struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.NewDecoder(resp.Body).Decode(&e) == nil && e.Error.Message != "" {
			return fmt.Errorf("%s", e.Error.Message)
		}
		return fmt.Errorf("unexpected status %s", resp.Status)
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func fetchModels(ctx context.Context, addr string) ([]apiModel, error) {
	if err := ensureNode(addr); err != nil {
		return nil, err
	}
	var body struct {
		Models []apiModel `json:"models"`
	}
	if err := call(ctx, addr, http.MethodGet, "/api/v1/models", nil, &body); err != nil {
		return nil, err
	}
	return body.Models, nil
}

func lsCmd(args []string) error {
	fs := flag.NewFlagSet("ls", flag.ExitOnError)
	addr := fs.String("addr", defaultAddr, "address of the local ModelFabric node")
	if err := fs.Parse(args); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	models, err := fetchModels(ctx, *addr)
	if err != nil {
		return err
	}
	if len(models) == 0 {
		fmt.Println("No models found.")
		fmt.Printf("\n  Put GGUF files in %s,\n", bold(defaultModelsRoot()))
		fmt.Printf("  or point %s at another directory in %s\n",
			bold("models_root"), dim(defaultConfigPath()))
		return nil
	}

	var total int64
	llms, embeddings := []apiModel{}, []apiModel{}
	for _, m := range models {
		total += m.SizeBytes
		if m.Type == "embedding" {
			embeddings = append(embeddings, m)
		} else {
			llms = append(llms, m)
		}
	}
	fmt.Printf("You have %s, taking up %s of disk space.\n",
		bold(plural(len(models), "model")), bold(humanBytes(total)))

	section := func(title string, rows []apiModel) {
		if len(rows) == 0 {
			return
		}
		fmt.Println()
		t := newTable(title, "PARAMS", "ARCH", "QUANT", "SIZE", "VISION", "STATE")
		for _, m := range rows {
			state := dim("—")
			if n := len(m.LoadedInstances); n > 0 {
				label := "loaded"
				if n > 1 {
					label = fmt.Sprintf("loaded ×%d", n)
				}
				state = green(label)
			}
			vision := dim("—")
			for _, c := range m.Capabilities {
				if c == "vision" {
					vision = cyan("yes")
				}
			}
			name := m.Key
			if m.Variants > 0 {
				name += dim(fmt.Sprintf(" (%s)", plural(m.Variants+1, "variant")))
			}
			t.add(name, dimDash(m.ParamsString), dimDash(m.Arch), dimDash(m.Quantization),
				dim(humanBytes(m.SizeBytes)), vision, state)
		}
		t.print()
	}
	section("LLM", llms)
	section("EMBEDDING", embeddings)
	return nil
}

func dimDash(s string) string {
	if s == "" {
		return dim("—")
	}
	return dim(s)
}

func plural(n int, word string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, word)
	}
	return fmt.Sprintf("%d %ss", n, word)
}

// pickModel resolves the model argument, prompting when none was given.
//
// A positional argument is returned as given, which is what `load` wants: a
// model key. Unload needs an instance id, so it resolves its own argument with
// unloadTargets and only calls this to prompt.
func pickModel(ctx context.Context, addr string, positional []string, wantLoaded bool) (string, string, error) {
	if len(positional) == 1 {
		return positional[0], "", nil
	}
	models, err := fetchModels(ctx, addr)
	if err != nil {
		return "", "", err
	}

	var choices []Choice
	for _, m := range models {
		loaded := len(m.LoadedInstances) > 0
		if loaded != wantLoaded {
			continue
		}
		note := humanBytes(m.SizeBytes)
		if m.ParamsString != "" {
			note = m.ParamsString + "  " + note
		}
		value := m.Key
		if wantLoaded {
			value = m.LoadedInstances[0].ID
		}
		choices = append(choices, Choice{Value: value, Label: m.Key, Note: note})
	}
	if len(choices) == 0 {
		if wantLoaded {
			return "", "", fmt.Errorf("no models are loaded")
		}
		return "", "", fmt.Errorf("no models available to load")
	}

	prompt := "Select a model to load"
	if wantLoaded {
		prompt = "Select a model to unload"
	}
	// A single candidate needs no prompt, but the caller still wants its label.
	value, err := selectOne(prompt, choices)
	if err != nil {
		return "", "", err
	}
	for _, c := range choices {
		if c.Value == value {
			return value, c.Label, nil
		}
	}
	return value, "", nil
}

// unloadTargets resolves an instance ID or model key.
// A model key selects every loaded replica of that model.
func unloadTargets(models []apiModel, arg string) ([]string, error) {
	var byKey []string
	var loaded []string
	for _, m := range models {
		for _, inst := range m.LoadedInstances {
			if inst.ID == arg {
				return []string{inst.ID}, nil
			}
			if m.Key == arg {
				byKey = append(byKey, inst.ID)
			}
			loaded = append(loaded, m.Key)
		}
	}
	if len(byKey) > 0 {
		return byKey, nil
	}
	if len(loaded) == 0 {
		return nil, fmt.Errorf("no models are loaded")
	}
	return nil, fmt.Errorf("%q is not a loaded model or instance; loaded: %s",
		arg, strings.Join(dedupe(loaded), ", "))
}

func dedupe(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := in[:0:0]
	for _, v := range in {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}

func psCmd(args []string) error {
	fs := flag.NewFlagSet("ps", flag.ExitOnError)
	addr := fs.String("addr", defaultAddr, "address of the local ModelFabric node")
	if err := fs.Parse(args); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	models, err := fetchModels(ctx, *addr)
	if err != nil {
		return err
	}
	t := newTable("INSTANCE", "MODEL", "CONTEXT", "PARALLEL", "VISION", "TTL")
	rows := 0
	for _, m := range models {
		for _, inst := range m.LoadedInstances {
			vision := dim("—")
			if cfgVal(inst.Config, "vision") == "yes" {
				vision = cyan("yes")
			}
			ttl := dim("—")
			if inst.TTLSeconds > 0 {
				left := time.Duration(inst.TTLSeconds)*time.Second - time.Since(inst.LastUsed)
				ttl = fmt.Sprintf("%s left", max(left, 0).Round(time.Second))
			}
			if inst.Origin == "jit" {
				ttl += dim(" (jit)")
			}
			t.add(cyan(inst.ID), m.Key,
				dim(cfgVal(inst.Config, "context_length")),
				dim(cfgVal(inst.Config, "parallel")), vision, ttl)
			rows++
		}
	}
	if rows == 0 {
		fmt.Println("No models loaded.")
		fmt.Printf("\n  Load one with %s\n", bold("mfsh load"))
		return nil
	}
	t.print()
	return nil
}

func loadCmd(args []string) error {
	fs := flag.NewFlagSet("load", flag.ExitOnError)
	addr := fs.String("addr", defaultAddr, "address of the local ModelFabric node")
	preset := fs.String("preset", "", "apply a saved preset's inference settings to this load")
	format := fs.String("format", "", "which weights to load when the model has variants: gguf or mlx")
	collect := settingsFromFlags(fs)
	replicas := fs.Int("replicas", 1, "ensure this many instances of the model on this node")
	addInstance := fs.Bool("add", false, "start another engine for a model already loaded here, with its own settings (a second GPU, a different slot count)")
	ttlFlag := fs.String("ttl", "", "unload after this long idle: a duration (30m) or seconds, as lms takes")
	positional, err := parsePositional(fs, args)
	if err != nil {
		return err
	}
	ttl, err := parseTTL(*ttlFlag)
	if err != nil {
		return err
	}
	if len(positional) > 1 {
		return fmt.Errorf("usage: mfsh load [model] [setting flags] [-preset NAME]   (mfsh load -h lists them)")
	}
	// Reject nonpositive replica counts; otherwise the initial load still creates one instance.
	if *replicas < 1 {
		return fmt.Errorf("-replicas must be at least 1 (got %d)", *replicas)
	}

	// Start the node before announcing the load, so the output reads in order.
	if err := ensureNode(*addr); err != nil {
		return err
	}
	pickCtx, pickCancel := context.WithTimeout(context.Background(), 60*time.Second)
	target, _, err := pickModel(pickCtx, *addr, positional, false)
	pickCancel()
	if err != nil {
		return err
	}

	req := map[string]any{"model": target, "echo_load_config": true}
	settings, err := collect()
	if err != nil {
		return err
	}
	for k, v := range settings {
		req[k] = v
	}
	if *preset != "" {
		req["preset"] = *preset
	}
	if *format != "" {
		req["format"] = *format
	}
	if ttl > 0 {
		req["ttl"] = ttl
	}
	if *addInstance {
		req["add_instance"] = true
	}
	// -gpu each is one engine per GPU: the first load takes GPU 0 and a
	// further instance is added for every other card. The node only knows
	// numbers; "each" is this command's shorthand.
	eachGPU := 0
	if g, _ := req["gpu"].(string); g == "each" {
		if *replicas > 1 || *addInstance {
			return fmt.Errorf("-gpu each starts one engine per GPU by itself; leave out -replicas and -add")
		}
		gctx, gcancel := context.WithTimeout(context.Background(), 20*time.Second)
		var topo struct {
			GPUs []struct {
				Name string `json:"name"`
			} `json:"gpus"`
		}
		err := call(gctx, *addr, http.MethodGet, "/api/v1/topology", nil, &topo)
		gcancel()
		if err != nil {
			return fmt.Errorf("could not ask the node how many GPUs it has: %w", err)
		}
		if len(topo.GPUs) < 2 {
			return fmt.Errorf("-gpu each needs more than one GPU, and this node reports %d; leave -gpu out", len(topo.GPUs))
		}
		eachGPU = len(topo.GPUs)
		req["gpu"] = "0"
	}

	// Allow for cold loads that take minutes; the server waits until the engine is ready.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	fmt.Fprintf(os.Stderr, "%s %s...\n", dim("loading"), bold(target))
	var resp struct {
		Instance struct {
			ID string `json:"id"`
		} `json:"instance"`
		Model           string         `json:"model"`
		ElapsedMS       int64          `json:"elapsed_ms"`
		EffectiveConfig map[string]any `json:"effective_config"`
		Operation       struct {
			ID      string `json:"id"`
			State   string `json:"state"`
			Message string `json:"message"`
		} `json:"operation"`
	}
	if err := call(ctx, *addr, http.MethodPost, "/api/v1/models/load", req, &resp); err != nil {
		return err
	}
	if resp.Instance.ID == "" {
		fmt.Printf("load still running (operation %s): %s\n", resp.Operation.ID, resp.Operation.Message)
		fmt.Println("poll with: mfsh ps")
		return nil
	}

	fmt.Printf("%s %s as %s in %s\n", green("✓ loaded"), bold(resp.Model), cyan(resp.Instance.ID),
		(time.Duration(resp.ElapsedMS) * time.Millisecond).Round(time.Millisecond))

	for g := 1; g < eachGPU; g++ {
		add := map[string]any{}
		for k, v := range req {
			add[k] = v
		}
		add["model"], add["add_instance"], add["gpu"] = resp.Model, true, strconv.Itoa(g)
		var r struct {
			Instance struct {
				ID string `json:"id"`
			} `json:"instance"`
			ElapsedMS int64 `json:"elapsed_ms"`
			Operation struct {
				ID string `json:"id"`
			} `json:"operation"`
		}
		if err := call(ctx, *addr, http.MethodPost, "/api/v1/models/load", add, &r); err != nil {
			return fmt.Errorf("the engine for GPU %d: %w (GPU 0 through %d are loaded; `mfsh ps` lists them)", g, err, g-1)
		}
		fmt.Println(gpuLoadLine(g, r.Instance.ID, r.Operation.ID, time.Duration(r.ElapsedMS)*time.Millisecond))
	}

	// Replicas are added one at a time. Each call waits for readiness, so the
	// count below is always of instances that are actually serving.
	if *replicas > 1 {
		models, err := fetchModels(ctx, *addr)
		if err != nil {
			return err
		}
		have := 0
		for _, m := range models {
			if m.Key == resp.Model {
				have = len(m.LoadedInstances)
			}
		}
		for have < *replicas {
			add := map[string]any{}
			for k, v := range req {
				add[k] = v
			}
			add["model"] = resp.Model
			add["add_instance"] = true
			var r struct {
				Instance struct {
					ID string `json:"id"`
				} `json:"instance"`
				ElapsedMS int64 `json:"elapsed_ms"`
			}
			if err := call(ctx, *addr, http.MethodPost, "/api/v1/models/load", add, &r); err != nil {
				return fmt.Errorf("replica %d of %d: %w", have+1, *replicas, err)
			}
			have++
			fmt.Printf("%s replica %d/%d as %s in %s\n", green("✓"), have, *replicas, cyan(r.Instance.ID),
				(time.Duration(r.ElapsedMS) * time.Millisecond).Round(time.Millisecond))
		}
	}
	if len(resp.EffectiveConfig) > 0 {
		fmt.Println()
		t := newTable("EFFECTIVE CONFIG", "")
		t.indent = "  "
		for _, k := range sortedKeys(resp.EffectiveConfig) {
			t.add(dim(k), configValue(resp.EffectiveConfig[k]))
		}
		t.print()
	}
	return nil
}

// parseTTL reads an idle TTL as seconds ("3600", like lms) or a duration
// ("60m"). Empty means none.
func parseTTL(s string) (int, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	if n, err := strconv.Atoi(s); err == nil && n >= 0 {
		return n, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d < time.Second {
		return 0, fmt.Errorf("-ttl %q: want seconds or a duration such as 30m", s)
	}
	return int(d / time.Second), nil
}

// configValue renders one effective-config value. Nested settings (sampling,
// template variables) print as key=value pairs rather than Go's map syntax.
func configValue(v any) string {
	m, ok := v.(map[string]any)
	if !ok {
		return fmt.Sprintf("%v", v)
	}
	parts := make([]string, 0, len(m))
	for _, k := range sortedKeys(m) {
		parts = append(parts, fmt.Sprintf("%s=%v", k, m[k]))
	}
	return strings.Join(parts, " ")
}

func unloadCmd(args []string) error {
	fs := flag.NewFlagSet("unload", flag.ExitOnError)
	addr := fs.String("addr", defaultAddr, "address of the local ModelFabric node")
	all := fs.Bool("all", false, "unload every loaded instance")
	positional, err := parsePositional(fs, args)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	if len(positional) > 1 {
		// pickModel only ever looked at a single positional, so extra
		// arguments were dropped and the prompt appeared instead.
		return fmt.Errorf("usage: mfsh unload [model|instance-id]   (one at a time, or -all)")
	}
	var targets []string
	if *all {
		models, err := fetchModels(ctx, *addr)
		if err != nil {
			return err
		}
		for _, m := range models {
			for _, inst := range m.LoadedInstances {
				targets = append(targets, inst.ID)
			}
		}
		if len(targets) == 0 {
			fmt.Println("Nothing loaded.")
			return nil
		}
	} else if len(positional) == 1 {
		models, err := fetchModels(ctx, *addr)
		if err != nil {
			return err
		}
		if targets, err = unloadTargets(models, positional[0]); err != nil {
			return err
		}
	} else {
		id, _, err := pickModel(ctx, *addr, nil, true)
		if err != nil {
			return err
		}
		targets = []string{id}
	}

	for _, id := range targets {
		if err := call(ctx, *addr, http.MethodPost, "/api/v1/models/unload",
			map[string]any{"instance_id": id}, nil); err != nil {
			return fmt.Errorf("unload %s: %w", id, err)
		}
		fmt.Printf("%s %s\n", green("✓ unloaded"), cyan(id))
	}
	return nil
}

func opsCmd(args []string) error {
	fs := flag.NewFlagSet("ops", flag.ExitOnError)
	addr := fs.String("addr", defaultAddr, "address of the local ModelFabric node")
	if err := fs.Parse(args); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if err := ensureNode(*addr); err != nil {
		return err
	}
	var body struct {
		Operations []struct {
			ID        string `json:"id"`
			Kind      string `json:"kind"`
			Model     string `json:"model"`
			State     string `json:"state"`
			Message   string `json:"message"`
			Error     string `json:"error"`
			ElapsedMS int64  `json:"elapsed_ms"`
		} `json:"operations"`
	}
	if err := call(ctx, *addr, http.MethodGet, "/api/v1/operations", nil, &body); err != nil {
		return err
	}
	if len(body.Operations) == 0 {
		fmt.Println("No operations recorded.")
		return nil
	}
	t := newTable("OPERATION", "KIND", "MODEL", "STATE", "ELAPSED", "DETAIL")
	for _, o := range body.Operations {
		detail, state := o.Message, o.State
		switch o.State {
		case "succeeded":
			state = green("succeeded")
		case "failed":
			state, detail = red("failed"), o.Error
		case "running":
			state = yellow("running")
		}
		t.add(dim(o.ID), o.Kind, o.Model, state,
			dim((time.Duration(o.ElapsedMS) * time.Millisecond).Round(time.Millisecond).String()),
			dim(truncate(detail, 60)))
	}
	t.print()
	return nil
}

func cfgVal(m map[string]any, key string) string {
	v, ok := m[key]
	if !ok {
		return "—"
	}
	switch t := v.(type) {
	case float64:
		return fmt.Sprintf("%d", int64(t))
	case bool:
		if t {
			return "yes"
		}
		return "no"
	default:
		return fmt.Sprintf("%v", v)
	}
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	for i := range keys {
		for j := i + 1; j < len(keys); j++ {
			if keys[j] < keys[i] {
				keys[i], keys[j] = keys[j], keys[i]
			}
		}
	}
	return keys
}

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

// truncate shortens s to at most n runes. Cutting by byte index split
// multi-byte characters in operation messages and printed the halves.
func truncate(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	out := []rune(s)
	return string(out[:n-1]) + "…"
}

func repinCmd(args []string) error {
	fs := flag.NewFlagSet("repin", flag.ExitOnError)
	addr := fs.String("addr", defaultAddr, "address of the local ModelFabric node")
	positional, err := parsePositional(fs, args)
	if err != nil {
		return err
	}
	if len(positional) != 1 {
		return fmt.Errorf("usage: mfsh repin <model>")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	var resp struct {
		Model string `json:"model"`
	}
	if err := call(ctx, *addr, http.MethodPost, "/api/v1/models/repin",
		map[string]any{"model": positional[0]}, &resp); err != nil {
		return err
	}
	fmt.Printf("%s %s to its current contents\n", green("✓ repinned"), bold(resp.Model))
	return nil
}

func recoverCmd(args []string) error {
	fs := flag.NewFlagSet("recover", flag.ExitOnError)
	addr := fs.String("addr", defaultAddr, "address of the local ModelFabric node")
	clear := fs.String("clear", "", "settle the unresolved launch window for this model")
	positional, err := parsePositional(fs, args)
	if err != nil {
		return err
	}
	if len(positional) > 0 && *clear == "" {
		*clear = positional[0]
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if *clear == "" {
		return fmt.Errorf("usage: mfsh recover -clear <model>\n\n" +
			"An unresolved launch window means a previous node died between starting an\n" +
			"engine and recording its identity. ModelFabric will not launch that model again\n" +
			"until you confirm no orphaned process is running and clear it.")
	}
	var resp struct {
		Model string `json:"model"`
	}
	if err := call(ctx, *addr, http.MethodPost, "/api/v1/models/recover",
		map[string]any{"model": *clear}, &resp); err != nil {
		return err
	}
	fmt.Printf("%s the unresolved launch window for %s\n", green("✓ cleared"), bold(resp.Model))
	return nil
}

func endpointsCmd(args []string) error {
	fs := flag.NewFlagSet("endpoints", flag.ExitOnError)
	addr := fs.String("addr", defaultAddr, "address of the local ModelFabric node")
	out := fs.String("o", "", "write to this file instead of stdout")
	if err := fs.Parse(args); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := ensureNode(*addr); err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		strings.TrimRight(*addr, "/")+"/z/endpoints.yaml", nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected status %s", resp.Status)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if *out != "" {
		if err := os.WriteFile(*out, body, 0o644); err != nil {
			return err
		}
		fmt.Printf("%s %s\n", green("✓ wrote"), bold(*out))
		return nil
	}
	fmt.Print(string(body))
	return nil
}

func runtimeCmd(args []string) error {
	sub := "ls"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		sub, args = args[0], args[1:]
	}
	switch sub {
	case "get", "update", "remove":
		return runtimePkgCmd(sub, args)
	}
	fs := flag.NewFlagSet("runtime", flag.ExitOnError)
	addr := fs.String("addr", defaultAddr, "address of the local ModelFabric node")
	auto := fs.Bool("auto", false, "with select: return to automatic selection")
	positional, err := parsePositional(fs, args)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	type hwView struct {
		CPU         string `json:"cpu"`
		MemoryMB    int    `json:"memory_mb"`
		CUDAVersion string `json:"cuda_version"`
		Vulkan      bool   `json:"vulkan"`
		GPUs        []struct {
			Name       string `json:"name"`
			MemoryMB   int    `json:"memory_mb"`
			ComputeCap string `json:"compute_cap"`
			Driver     string `json:"driver"`
		} `json:"gpus"`
	}
	var body struct {
		Runtimes []struct {
			Name    string   `json:"name"`
			Origin  string   `json:"origin"`
			Backend string   `json:"backend"`
			Pin     string   `json:"pin"`
			Domains []string `json:"domains"`
			Default bool     `json:"default"`
			Fit     string   `json:"fit"`
			Reasons []string `json:"reasons"`
		} `json:"runtimes"`
		Hardware *hwView `json:"hardware"`
	}
	load := func() error {
		return call(ctx, *addr, http.MethodGet, "/api/v1/runtimes", nil, &body)
	}

	fitText := func(fit string) string {
		switch fit {
		case "yes":
			return green("yes")
		case "no":
			return red("no")
		case "unknown":
			return yellow("unknown")
		}
		return dim("—")
	}

	switch sub {
	case "ls", "list":
		if err := load(); err != nil {
			return err
		}
		if len(body.Runtimes) == 0 {
			fmt.Println("No inference runtimes available.")
			fmt.Printf("\n  ModelFabric looks for LM Studio engine packages in %s\n", bold("~/.lmstudio/extensions/backends"))
			fmt.Printf("  or declare one under %s in %s\n", bold("runtimes"), dim(defaultConfigPath()))
			return nil
		}
		t := newTable("ENGINE", "SELECTED", "FITS", "ORIGIN", "BACKEND", "PIN")
		for _, r := range body.Runtimes {
			sel := ""
			if r.Default {
				sel = green("✓")
			}
			pin := dim(r.Pin)
			if r.Pin == "partial" {
				pin = yellow("partial")
			}
			t.add(r.Name, sel, fitText(r.Fit), dim(r.Origin), dim(r.Backend), pin)
		}
		t.print()
		return nil

	case "survey":
		if err := load(); err != nil {
			return err
		}
		if hw := body.Hardware; hw != nil {
			fmt.Printf("%s %s, %s RAM\n", bold("CPU"), hw.CPU, humanBytes(int64(hw.MemoryMB)<<20))
			for _, g := range hw.GPUs {
				fmt.Printf("%s %s, %s, compute %s, driver %s\n", bold("GPU"), g.Name,
					humanBytes(int64(g.MemoryMB)<<20), g.ComputeCap, g.Driver)
			}
			if hw.CUDAVersion != "" {
				fmt.Printf("%s supports up to %s\n", bold("CUDA"), hw.CUDAVersion)
			}
			if len(hw.GPUs) == 0 {
				fmt.Println(dim("No NVIDIA GPU detected."))
			}
			fmt.Println()
		}
		t := newTable("ENGINE", "FITS", "WHY")
		for _, r := range body.Runtimes {
			why := dim("—")
			if len(r.Reasons) > 0 {
				why = strings.Join(r.Reasons, "; ")
			}
			t.add(r.Name, fitText(r.Fit), why)
		}
		t.print()
		return nil

	case "select":
		name := ""
		if *auto {
			// Clearing the choice returns to automatic selection, which follows
			// new engine packages as they are installed.
		} else if len(positional) == 1 {
			name = positional[0]
		} else {
			if err := load(); err != nil {
				return err
			}
			var choices []Choice
			for _, r := range body.Runtimes {
				if r.Fit == "no" {
					continue
				}
				note := r.Backend
				if r.Default {
					note += "  (current)"
				}
				choices = append(choices, Choice{Value: r.Name, Label: r.Name, Note: note})
			}
			if name, err = selectOne("Select the default runtime", choices); err != nil {
				return err
			}
		}
		var resp struct {
			Default string `json:"default"`
		}
		if err := call(ctx, *addr, http.MethodPost, "/api/v1/runtimes/select",
			map[string]any{"name": name}, &resp); err != nil {
			return err
		}
		if name == "" {
			fmt.Printf("%s automatic selection (currently %s)\n", green("✓ using"), bold(resp.Default))
			fmt.Println(dim("  The best compatible runtime is chosen, including newly installed ones."))
			return nil
		}
		fmt.Printf("%s %s as the default runtime\n", green("✓ selected"), bold(resp.Default))
		fmt.Println(dim("  Applies to new loads; running instances keep the runtime they started with."))
		return nil
	}
	return fmt.Errorf("usage: mfsh runtime [ls | survey | select [name] | select -auto | get [backend] | update | remove [name]]")
}

func preferCmd(args []string) error {
	fs := flag.NewFlagSet("prefer", flag.ExitOnError)
	addr := fs.String("addr", defaultAddr, "address of the local ModelFabric node")
	clear := fs.Bool("none", false, "clear the preference")
	positional, err := parsePositional(fs, args)
	if err != nil {
		return err
	}
	if len(positional) > 1 {
		return fmt.Errorf("usage: mfsh prefer [node]   (or -none to clear)")
	}
	if *clear && len(positional) == 1 {
		return fmt.Errorf("-none clears the preference, so it takes no node (got %q)", positional[0])
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := ensureNode(*addr); err != nil {
		return err
	}

	if len(positional) == 0 && !*clear {
		var cur struct {
			Node string `json:"preferred_node"`
		}
		if err := call(ctx, *addr, http.MethodGet, "/z/preferred", nil, &cur); err != nil {
			return err
		}
		if cur.Node == "" {
			fmt.Println("No preferred node; models resolve to the least busy holder.")
			fmt.Printf("\n  Set one with %s\n", bold("mfsh prefer <node>"))
			return nil
		}
		fmt.Printf("Preferred node: %s\n", bold(cur.Node))
		fmt.Println(dim(preferScope))
		return nil
	}

	node := ""
	if !*clear {
		node = positional[0]
	}
	var resp struct {
		Node string `json:"preferred_node"`
	}
	if err := call(ctx, *addr, http.MethodPost, "/z/preferred",
		map[string]any{"node": node}, &resp); err != nil {
		return err
	}
	if resp.Node == "" {
		fmt.Printf("%s preference; models resolve to the least busy holder\n", green("✓ cleared"))
		return nil
	}
	fmt.Printf("%s %s as the preferred node\n", green("✓ set"), bold(resp.Node))
	fmt.Println(dim("  Models held there are used first; other nodes still serve if it cannot."))
	fmt.Println(dim(preferScope))
	return nil
}

const preferScope = `  Applies to requests at this node's front door; with
  llm-d on, llm-d schedules only what the preferred node does not take.`

// gpuLoadLine reports one engine of `load -gpu each`. A load that outlasts
// the server's wait is answered with the operation and no instance. That was
// printed as "✓ GPU 1 as  in 0s", a tick for an engine that was not up and
// might yet fail.
func gpuLoadLine(gpu int, instance, operation string, took time.Duration) string {
	if instance == "" {
		return fmt.Sprintf("%s GPU %d is still loading (operation %s); `mfsh ps` shows when it is up, `mfsh ops` if it fails",
			yellow("…"), gpu, operation)
	}
	return fmt.Sprintf("%s GPU %d as %s in %s", green("✓"), gpu, cyan(instance), took.Round(time.Millisecond))
}
