package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Load and inference settings as command-line flags, shared by `mfsh load`
// (this load only) and `mfsh defaults` (every load of a model). The JSON
// names match the API and the saved files.

type settingKind int

const (
	kindInt settingKind = iota
	kindFloat
	kindBool
	kindString
)

type settingFlag struct {
	flag, key string
	kind      settingKind
	help      string
}

// settingFlags is every setting. -context, -parallel and -gpu-layers keep
// the names `mfsh load` always had.
var settingFlags = []settingFlag{
	{"context", "context_length", kindInt, "context length per request"},
	{"parallel", "parallel", kindInt, "concurrent slots"},
	{"gpu-layers", "gpu_layers", kindInt, "layers to offload to the GPU"},
	{"gpu", "gpu", kindString, "GPUs this engine may use, as nvidia-smi numbers them: 0, 1, 0,1 or all (with load, also each: one engine per GPU)"},
	{"offload-ratio", "offload_ratio", kindFloat, "fraction of layers on the GPU, 0..1 (LM Studio's GPU offload)"},
	{"flash-attention", "flash_attention", kindBool, "flash attention on|off"},
	{"vision", "vision", kindBool, "serve images on|off; off loads a multimodal model text-only, freeing the projector and letting it speculate"},
	{"spec", "spec_mode", kindString, "speculative decoding: auto|mtp|draft|off"},
	{"draft-model", "draft_model", kindString, "draft model for speculative decoding (a model key)"},
	{"draft-max", "draft_max", kindInt, "max tokens drafted per step"},
	{"draft-min", "draft_min", kindInt, "min tokens drafted per step"},
	{"draft-p-min", "draft_p_min", kindFloat, "min draft probability to keep drafting, 0..1"},
	{"draft-gpu-layers", "draft_gpu_layers", kindInt, "draft model layers on the GPU"},
	{"batch", "batch_size", kindInt, "logical batch size (LM Studio: evaluation batch size)"},
	{"ubatch", "ubatch_size", kindInt, "physical batch size"},
	{"cache-k", "cache_type_k", kindString, "K cache type: f16|q8_0|q4_0|…"},
	{"cache-v", "cache_type_v", kindString, "V cache type: f16|q8_0|q4_0|…"},
	{"kv-unified", "kv_unified", kindBool, "one KV pool shared by all slots on|off"},
	{"kv-offload", "kv_offload", kindBool, "KV cache on the GPU on|off"},
	{"cache-reuse", "cache_reuse", kindInt, "min chunk to reuse from the cache by shifting"},
	{"ctx-checkpoints", "ctx_checkpoints", kindInt, "context checkpoints per slot"},
	{"cache-ram", "cache_ram", kindInt, "host-RAM prompt cache in MiB, 0 off (default: 8192 or 1/8 of RAM, whichever is less)"},
	{"keep-in-memory", "keep_in_memory", kindBool, "lock the model in RAM (mlock) on|off"},
	{"mmap", "try_mmap", kindBool, "memory-map the model on|off"},
	{"n-cpu-moe", "n_cpu_moe", kindInt, "MoE: keep the experts of the first N layers on the CPU"},
	{"cpu-moe-ratio", "cpu_moe_ratio", kindFloat, "MoE: fraction of layers whose experts stay on the CPU, 0..1"},
	{"rope-base", "rope_freq_base", kindFloat, "RoPE frequency base"},
	{"rope-scale", "rope_freq_scale", kindFloat, "RoPE frequency scale"},
	{"threads", "threads", kindInt, "CPU threads for generation"},
	{"threads-batch", "threads_batch", kindInt, "CPU threads for prompt processing"},
	{"seed", "seed", kindInt, "random seed"},
	{"temp", "temperature", kindFloat, "default temperature"},
	{"top-k", "top_k", kindInt, "default top-k"},
	{"top-p", "top_p", kindFloat, "default top-p"},
	{"min-p", "min_p", kindFloat, "default min-p"},
	{"repeat-penalty", "repeat_penalty", kindFloat, "default repeat penalty"},
	{"presence-penalty", "presence_penalty", kindFloat, "default presence penalty"},
	{"frequency-penalty", "frequency_penalty", kindFloat, "default frequency penalty"},
	{"thinking", "enable_thinking", kindBool, "thinking on|off by default"},
}

type argList []string

func (a *argList) String() string     { return strings.Join(*a, " ") }
func (a *argList) Set(v string) error { *a = append(*a, v); return nil }

func settingsFromFlags(fs *flag.FlagSet) func() (map[string]any, error) {
	vals := map[string]*string{}
	for _, f := range settingFlags {
		vals[f.key] = fs.String(f.flag, "", f.help)
	}
	var extra argList
	fs.Var(&extra, "arg", "extra engine argument, repeatable (flags ModelFabric manages are refused)")
	return func() (map[string]any, error) {
		out := map[string]any{}
		for _, f := range settingFlags {
			raw := strings.TrimSpace(*vals[f.key])
			if raw == "" {
				continue
			}
			v, err := parseSetting(f.kind, raw)
			if err != nil {
				return nil, fmt.Errorf("-%s: %w", f.flag, err)
			}
			out[f.key] = v
		}
		if len(extra) > 0 {
			out["extra_args"] = []string(extra)
		}
		return out, nil
	}
}

func parseSetting(kind settingKind, raw string) (any, error) {
	switch kind {
	case kindInt:
		n, err := strconv.Atoi(raw)
		if err != nil {
			return nil, fmt.Errorf("want a whole number, got %q", raw)
		}
		return n, nil
	case kindFloat:
		f, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			return nil, fmt.Errorf("want a number, got %q", raw)
		}
		return f, nil
	case kindBool:
		switch strings.ToLower(raw) {
		case "on", "true", "yes", "1":
			return true, nil
		case "off", "false", "no", "0":
			return false, nil
		}
		return nil, fmt.Errorf("want on or off, got %q", raw)
	}
	return raw, nil
}

func flagForKey(key string) string {
	for _, f := range settingFlags {
		if f.key == key {
			return "-" + f.flag
		}
	}
	if key == "extra_args" {
		return "-arg"
	}
	return key
}

func printSettings(settings map[string]any, indent string) {
	keys := make([]string, 0, len(settings))
	for k := range settings {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	t := newTable("", "")
	t.indent = indent
	for _, k := range keys {
		t.add(dim(flagForKey(k)), configValue(settings[k]))
	}
	t.print()
}

func defaultsCmd(args []string) error {
	fs := flag.NewFlagSet("defaults", flag.ExitOnError)
	addr := fs.String("addr", defaultAddr, "address of the local ModelFabric node")
	preset := fs.String("preset", "", "default preset for this model (\"none\" to clear)")
	var unset argList
	fs.Var(&unset, "unset", "remove one saved setting by flag name, repeatable (e.g. -unset temp)")
	clear := fs.Bool("clear", false, "remove every saved default for this model")
	collect := settingsFromFlags(fs)
	positional, err := parsePositional(fs, args)
	if err != nil {
		return err
	}
	if len(positional) != 1 {
		return fmt.Errorf("usage: mfsh defaults <model> [setting flags] [-preset NAME] [-unset FLAG] [-clear]\n  run `mfsh defaults -h` for every setting")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	path := "/api/v1/model-defaults?model=" + url.QueryEscape(positional[0])

	var cur struct {
		Model    string `json:"model"`
		Defaults struct {
			Preset   string         `json:"preset"`
			Settings map[string]any `json:"settings"`
		} `json:"defaults"`
	}
	if err := call(ctx, *addr, http.MethodGet, path, nil, &cur); err != nil {
		return err
	}
	set, err := collect()
	if err != nil {
		return err
	}
	changed := *clear || len(set) > 0 || len(unset) > 0 || *preset != ""
	if changed {
		next := cur.Defaults
		if next.Settings == nil || *clear {
			next.Settings = map[string]any{}
		}
		if *clear {
			next.Preset = ""
		}
		for k, v := range set {
			next.Settings[k] = v
		}
		for _, u := range unset {
			key := strings.TrimPrefix(u, "-")
			for _, f := range settingFlags {
				if f.flag == key {
					key = f.key
				}
			}
			if key == "arg" {
				key = "extra_args"
			}
			delete(next.Settings, key)
		}
		switch *preset {
		case "":
		case "none":
			next.Preset = ""
		default:
			next.Preset = *preset
		}
		body := map[string]any{"preset": next.Preset, "settings": next.Settings}
		if err := call(ctx, *addr, http.MethodPut, path, body, &cur); err != nil {
			return err
		}
	}

	fmt.Printf("%s %s\n", bold("Defaults for"), bold(cur.Model))
	if cur.Defaults.Preset == "" && len(cur.Defaults.Settings) == 0 {
		fmt.Println(dim("  none saved: loads use the model's model.yaml and the runtime's defaults"))
	} else {
		if cur.Defaults.Preset != "" {
			fmt.Printf("  %s %s\n", dim("preset"), cur.Defaults.Preset)
		}
		printSettings(cur.Defaults.Settings, "  ")
	}
	if changed {
		fmt.Println(dim("\n  Applied on the next load; a running instance keeps its settings until reloaded."))
	}
	return nil
}

func presetCmd(args []string) error {
	sub := "ls"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		sub, args = args[0], args[1:]
	}
	fs := flag.NewFlagSet("preset "+sub, flag.ExitOnError)
	addr := fs.String("addr", defaultAddr, "address of the local ModelFabric node")
	desc := fs.String("description", "", "with save: what the preset is for")
	name := fs.String("name", "", "with import: name to save it under (default: the file's)")
	collect := settingsFromFlags(fs)
	positional, err := parsePositional(fs, args)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	type preset struct {
		Name        string         `json:"name"`
		Description string         `json:"description"`
		Settings    map[string]any `json:"settings"`
	}
	switch sub {
	case "ls", "list":
		var body struct {
			Presets []preset `json:"presets"`
		}
		if err := call(ctx, *addr, http.MethodGet, "/api/v1/presets", nil, &body); err != nil {
			return err
		}
		if len(body.Presets) == 0 {
			fmt.Println("No presets.")
			fmt.Printf("\n  Save one with %s\n  or import LM Studio's with %s\n",
				bold("mfsh preset save <name> -temp 0.7 -top-p 0.9"), bold("mfsh preset import <file.json>"))
			return nil
		}
		for _, p := range body.Presets {
			fmt.Printf("%s  %s\n", bold(p.Name), dim(p.Description))
			printSettings(p.Settings, "  ")
		}
		return nil
	case "save":
		if len(positional) != 1 {
			return fmt.Errorf("usage: mfsh preset save <name> [inference flags] [-description TEXT]")
		}
		set, err := collect()
		if err != nil {
			return err
		}
		if len(set) == 0 {
			return fmt.Errorf("give at least one setting, e.g. -temp 0.7")
		}
		var p preset
		if err := call(ctx, *addr, http.MethodPut, "/api/v1/presets/"+url.PathEscape(positional[0]),
			map[string]any{"description": *desc, "settings": set}, &p); err != nil {
			return err
		}
		fmt.Printf("%s preset %s\n", green("✓ saved"), bold(p.Name))
		printSettings(p.Settings, "  ")
		fmt.Printf("\n  Use it with %s or make it a model's default with %s\n",
			bold("mfsh load <model> -preset "+p.Name), bold("mfsh defaults <model> -preset "+p.Name))
		return nil
	case "rm", "delete":
		if len(positional) != 1 {
			return fmt.Errorf("usage: mfsh preset rm <name>")
		}
		if err := call(ctx, *addr, http.MethodDelete, "/api/v1/presets/"+url.PathEscape(positional[0]), nil, nil); err != nil {
			return err
		}
		fmt.Printf("%s preset %s\n", green("✓ removed"), bold(positional[0]))
		return nil
	case "import":
		if len(positional) != 1 {
			return fmt.Errorf("usage: mfsh preset import <lm-studio-preset.json> [-name NAME]")
		}
		// Check the 1 MiB server limit before allocating memory for the file.
		const maxPreset = 1 << 20
		if fi, err := os.Stat(positional[0]); err == nil && fi.Size() > maxPreset {
			return fmt.Errorf("%s is %s; a preset must be under 1MB", positional[0], humanBytes(fi.Size()))
		}
		data, err := os.ReadFile(positional[0])
		if err != nil {
			return err
		}
		if len(data) > maxPreset {
			return fmt.Errorf("%s is larger than the 1MB a preset may be", positional[0])
		}
		var raw json.RawMessage = data
		var resp struct {
			Preset  preset   `json:"preset"`
			Skipped []string `json:"skipped"`
		}
		if err := call(ctx, *addr, http.MethodPost, "/api/v1/presets/import?name="+url.QueryEscape(*name), raw, &resp); err != nil {
			return err
		}
		fmt.Printf("%s LM Studio preset as %s\n", green("✓ imported"), bold(resp.Preset.Name))
		printSettings(resp.Preset.Settings, "  ")
		if len(resp.Skipped) > 0 {
			fmt.Printf("  %s %s\n", dim("not applicable to a server, skipped:"), strings.Join(resp.Skipped, ", "))
		}
		return nil
	}
	return fmt.Errorf("usage: mfsh preset [ls | save <name> ... | rm <name> | import <file>]")
}
