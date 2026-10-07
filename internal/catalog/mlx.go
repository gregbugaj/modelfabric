package catalog

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// MLX models are directories containing config.json and safetensors shards.
// The engine receives the directory path; metadata comes from config.json.

type mlxConfig struct {
	ModelType            string   `json:"model_type"`
	Architectures        []string `json:"architectures"`
	MaxPositionEmbed     int      `json:"max_position_embeddings"`
	NumHiddenLayers      int      `json:"num_hidden_layers"`
	NumLocalExperts      int      `json:"num_local_experts"`
	NumExperts           int      `json:"num_experts"`
	NumNextnPredictLayer int      `json:"num_nextn_predict_layers"`
	Quantization         *struct {
		Bits  int `json:"bits"`
		Group int `json:"group_size"`
	} `json:"quantization"`
	// TextConfig carries the language model's own fields on multimodal models,
	// where the top level describes the wrapper.
	TextConfig *struct {
		MaxPositionEmbed int `json:"max_position_embeddings"`
		NumHiddenLayers  int `json:"num_hidden_layers"`
	} `json:"text_config"`
}

// isMLXDir reports whether dir is an MLX model: a config.json beside at least
// one safetensors file. Both are required; a bare config.json is a tokenizer
// or processor directory, and loose safetensors without a config cannot be
// loaded.
func isMLXDir(dir string) bool {
	if _, err := os.Stat(filepath.Join(dir, "config.json")); err != nil {
		return false
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if !e.IsDir() && strings.EqualFold(filepath.Ext(e.Name()), ".safetensors") {
			return true
		}
	}
	return false
}

func readMLXConfig(dir string) (mlxConfig, error) {
	var c mlxConfig
	b, err := os.ReadFile(filepath.Join(dir, "config.json"))
	if err != nil {
		return c, err
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return c, fmt.Errorf("%s: %w", filepath.Join(dir, "config.json"), err)
	}
	return c, nil
}

// mlxFiles lists a model directory's files, relative to it and sorted by
// WalkDir, so a load can pin every byte it will read.
func mlxFiles(dir string) ([]string, int64, error) {
	var files []string
	var size int64
	// Reject enumeration failures rather than pinning an incomplete file list.
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return fmt.Errorf("walk %s: %w", path, err)
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return fmt.Errorf("%s is not under %s: %w", path, dir, err)
		}
		info, err := d.Info()
		if err != nil {
			return fmt.Errorf("stat %s: %w", path, err)
		}
		size += info.Size()
		files = append(files, filepath.ToSlash(rel))
		return nil
	})
	return files, size, err
}

func describeMLX(absRoot, dir string) (Model, error) {
	rel, err := filepath.Rel(absRoot, dir)
	if err != nil {
		return Model{}, err
	}
	rel = filepath.ToSlash(rel)
	files, size, err := mlxFiles(dir)
	if err != nil {
		return Model{}, fmt.Errorf("unreadable MLX directory %s: %w", dir, err)
	}
	if len(files) == 0 {
		// Do not wrap a nil error after a successful read of invalid JSON.
		return Model{}, fmt.Errorf("MLX directory %s holds no files", dir)
	}

	name := filepath.Base(rel)
	var publisher string
	if parts := strings.SplitN(rel, "/", 2); len(parts) == 2 {
		publisher = parts[0]
	}
	m := Model{
		Key:         rel,
		PathKey:     rel,
		Type:        "llm",
		Publisher:   publisher,
		DisplayName: name,
		Format:      "mlx",
		SizeBytes:   size,
		Path:        dir,
	}
	if p := paramsPattern.FindStringSubmatch(name); p != nil {
		m.ParamsString = strings.ToUpper(p[1] + p[2])
	}
	if looksLikeEmbedding(name) {
		m.Type = "embedding"
	}

	cfg, err := readMLXConfig(dir)
	if err != nil {
		return m, nil // listed, but with only what the path says
	}
	m.Architecture = cfg.ModelType
	if m.Architecture == "" && len(cfg.Architectures) > 0 {
		m.Architecture = cfg.Architectures[0]
	}
	m.MaxContextLength = cfg.MaxPositionEmbed
	m.Layers = cfg.NumHiddenLayers
	if cfg.TextConfig != nil {
		if m.MaxContextLength == 0 {
			m.MaxContextLength = cfg.TextConfig.MaxPositionEmbed
		}
		if m.Layers == 0 {
			m.Layers = cfg.TextConfig.NumHiddenLayers
		}
	}
	m.Experts = max(cfg.NumLocalExperts, cfg.NumExperts)
	m.DraftLayers = cfg.NumNextnPredictLayer
	if cfg.Quantization != nil && cfg.Quantization.Bits > 0 {
		// MLX names quantizations by width, the way the repositories do
		// ("…-4bit"), not by llama.cpp's Q4_K_M scheme.
		m.Quantization = fmt.Sprintf("%dbit", cfg.Quantization.Bits)
	}
	return m, nil
}
