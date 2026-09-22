// Package catalog indexes prepared models on disk.
//
// It answers "what could be loaded here", which is the `models` half of the
// LM Studio-compatible list response. Loaded instances are the supervisor's
// business, not the catalog's.
//
// Only declared model roots are scanned, so discovery does not walk unrelated
// credentials or user directories. Nothing is downloaded.
package catalog

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
)

// Model is one loadable model, shaped for the /api/v1/models response.
type Model struct {
	Key          string `json:"key"`
	Type         string `json:"type"` // "llm" | "embedding"
	Publisher    string `json:"publisher,omitempty"`
	DisplayName  string `json:"display_name"`
	Architecture string `json:"architecture,omitempty"`
	// ReasoningEfforts are the thinking levels this model's chat template
	// accepts, read from the template in the GGUF. Empty when the template
	// names none, and the settings form then takes a typed level instead of
	// offering choices it cannot vouch for.
	ReasoningEfforts []string `json:"reasoning_efforts,omitempty"`
	Format           string   `json:"format"` // "gguf"
	Quantization     string   `json:"quantization,omitempty"`
	ParamsString     string   `json:"params_string,omitempty"`
	SizeBytes        int64    `json:"size_bytes"`
	MaxContextLength int      `json:"max_context_length,omitempty"`
	// DraftLayers > 0 means the model carries its own MTP draft head, so
	// speculative decoding needs no separate draft model.
	DraftLayers int `json:"draft_layers,omitempty"`
	// Layers is the model's transformer layer count; Experts is non-zero for
	// mixture-of-experts models. Both turn LM Studio-style ratios (GPU
	// offload, CPU expert layers) into llama.cpp's layer counts.
	Layers       int      `json:"layers,omitempty"`
	Experts      int      `json:"experts,omitempty"`
	Capabilities []string `json:"capabilities,omitempty"`

	// Path is the primary weights file. Not part of the LM Studio shape, but
	// the supervisor needs it to build argv.
	Path string `json:"-"`
	// Projector is a paired multimodal projector (mmproj-*.gguf). Without it a
	// vision model silently degrades to text-only, so it is tracked explicitly.
	Projector string `json:"-"`
	// Source is the models root this came from, so the CLI can show where a
	// model lives when several directories are in play.
	Source string `json:"-"`
	// PathKey is the location-derived identifier, kept as an alias so a model
	// stays addressable even when two files share a hub id.
	PathKey string `json:"-"`
	// Variants counts other files that derive the same hub id (different
	// quantizations of one model).
	Variants int `json:"variants,omitempty"`
	// HubID is set when the identity came from a catalog rather than being
	// derived from the model's own metadata.
	HubID string `json:"hub_id,omitempty"`
	// MinMemoryBytes is the publisher's estimate of what a load needs, from
	// model.yaml.
	MinMemoryBytes int64 `json:"min_memory_bytes,omitempty"`
	// Spec is the publisher's model.yaml, when the hub has one.
	Spec *ModelSpec `json:"-"`
}

// Catalog is an immutable snapshot of one or more models roots.
type Catalog struct {
	Root   string   // the primary root, for messages
	Roots  []string // every root scanned, in precedence order
	models map[string]Model
	// aliases maps a path-derived key to the hub id it resolves to.
	aliases map[string]string
	// variants holds shadowed files, addressable by their path key.
	variants map[string]Model
	// hub is LM Studio's catalog, used to resolve authoritative model ids.
	hub *Hub
}

// quantPattern matches the quantization suffix llama.cpp conventionally puts
// last in a GGUF filename: Q4_K_M, Q8_0, IQ3_XXS, BF16, F16.
var quantPattern = regexp.MustCompile(`(?i)(IQ\d+_[A-Z0-9_]+|Q\d+_[A-Z0-9_]+|Q\d+_\d+|BF16|F16|F32)$`)

// paramsPattern matches a parameter count such as 27B or 4B.
var paramsPattern = regexp.MustCompile(`(?i)(\d+(?:\.\d+)?)\s*([BM])\b`)

// Scan indexes every GGUF under root.
func Scan(root string) (*Catalog, error) { return ScanRoots(root) }

// ScanRoots indexes several models directories in precedence order.
//
// Earlier roots win a key collision, so an operator's own directory can shadow
// a model of the same name found in someone else's (an LM Studio install, say).
// A missing root is not an error: a node with no models is a valid node that
// can still route for its peers.
func ScanRoots(roots ...string) (*Catalog, error) { return ScanRootsWithHub(nil, roots...) }

// ScanRootsWithHub indexes several roots, resolving identity through a hub
// catalog when one is available.
func ScanRootsWithHub(hub *Hub, roots ...string) (*Catalog, error) {
	c := &Catalog{
		models:   map[string]Model{},
		aliases:  map[string]string{},
		variants: map[string]Model{},
		hub:      hub,
	}
	for _, r := range roots {
		if r == "" {
			continue
		}
		if c.Root == "" {
			c.Root = r
		}
		c.Roots = append(c.Roots, r)
		if err := c.scanOne(r); err != nil {
			return nil, err
		}
	}
	return c, nil
}

func (c *Catalog) scanOne(root string) error {
	info, err := os.Stat(root)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("scan models root: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("models root %s is not a directory", root)
	}

	// Projectors are collected first so a model found later can be paired with
	// one regardless of walk order.
	projectors := map[string][]string{} // directory -> projector paths
	var weights []string
	var mlxDirs []string
	// bytes of the shards that are not indexed, keyed by their shared prefix
	shardBytes := map[string]int64{}

	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // an unreadable subtree should not fail the whole scan
		}
		if d.IsDir() {
			// An MLX model is a whole directory, and nothing inside it is a
			// model of its own, so it is taken as one and not descended into.
			if path != root && isMLXDir(path) {
				mlxDirs = append(mlxDirs, path)
				return fs.SkipDir
			}
			return nil
		}
		if !strings.EqualFold(filepath.Ext(path), ".gguf") {
			return nil
		}
		base := filepath.Base(path)
		if isProjector(base) {
			// All of them: a directory holding two multimodal models has two
			// projectors, and keeping only the last one attached it to every
			// model in that directory.
			dir := filepath.Dir(path)
			projectors[dir] = append(projectors[dir], path)
			return nil
		}
		// A sharded GGUF is one logical model; index only the first shard —
		// but remember the other shards' bytes, or the catalog reports a
		// 60 GB model as the 12 GB of its first piece, and that number is
		// what the disk summary and placement decisions use.
		if isNonFirstShard(base) {
			if fi, err := d.Info(); err == nil {
				shardBytes[shardGroup(path)] += fi.Size()
			}
			return nil
		}
		weights = append(weights, path)
		return nil
	})
	if err != nil {
		return fmt.Errorf("scan models root: %w", err)
	}

	absRoot, _ := filepath.Abs(root)
	for _, path := range weights {
		m, err := describe(absRoot, path)
		if err != nil {
			continue // unreadable file; skip rather than fail the scan
		}
		c.identify(&m)
		if p, ok := projectorFor(projectors[filepath.Dir(path)], path); ok {
			m.Projector = p
			m.Capabilities = append(m.Capabilities, "vision")
			// The projector is loaded with the model, so it counts toward the
			// footprint the operator cares about.
			if fi, err := os.Stat(p); err == nil {
				m.SizeBytes += fi.Size()
			}
		}
		if extra, ok := shardBytes[shardGroup(path)]; ok {
			m.SizeBytes += extra
		}
		m.Source = root
		c.add(m)
	}
	// MLX directories are indexed after the GGUF files, so that where the same
	// model exists in both formats the hub id keeps meaning what it means on
	// every other node, and the MLX copy stays addressable by its path key.
	for _, dir := range mlxDirs {
		m, err := describeMLX(absRoot, dir)
		if err != nil {
			continue // unreadable directory; skip rather than fail the scan
		}
		c.identify(&m)
		m.Source = root
		c.add(m)
	}
	return nil
}

// identify replaces path-derived identity with the hub catalog's, which states
// a model's identity where the model's own metadata only implies it.
func (c *Catalog) identify(m *Model) {
	e, ok := c.hub.LookupPath(m.PathKey)
	if !ok {
		return
	}
	m.Key = e.ID
	m.Publisher = e.Owner
	m.HubID = e.ID
	if e.Spec == nil {
		return
	}
	m.Spec = e.Spec
	m.MinMemoryBytes = e.Spec.MinMemoryBytes
	// Tool use and reasoning are training facts the weights do not state;
	// vision stays tied to an actual projector file.
	if e.Spec.ToolUse != nil && *e.Spec.ToolUse {
		m.Capabilities = append(m.Capabilities, "tool_use")
	}
	if e.Spec.Reasoning != nil && *e.Spec.Reasoning {
		m.Capabilities = append(m.Capabilities, "reasoning")
	}
}

// add indexes a model, keeping the first of several that claim one identity.
func (c *Catalog) add(m Model) {
	if existing, taken := c.models[m.Key]; taken {
		// Two models claiming one hub id are quantization variants — or the
		// same weights in two formats, GGUF and MLX. Keep the first and record
		// the rest, which stay addressable by their path key.
		existing.Variants++
		c.models[m.Key] = existing
		c.aliases[m.PathKey] = m.Key
		c.variants[m.PathKey] = m
		return
	}
	c.models[m.Key] = m
	if m.PathKey != m.Key {
		c.aliases[m.PathKey] = m.Key
	}
}

// projectorFor picks the projector that belongs to one model file. With a
// single projector in the directory it is that one; with several, the name
// has to say which — attaching the wrong projector makes a model answer as if
// it could see something it cannot.
func projectorFor(candidates []string, model string) (string, bool) {
	switch len(candidates) {
	case 0:
		return "", false
	case 1:
		return candidates[0], true
	}
	stem := strings.ToLower(modelStem(filepath.Base(model)))
	for _, c := range candidates {
		if stem != "" && strings.Contains(strings.ToLower(filepath.Base(c)), stem) {
			return c, true
		}
	}
	return "", false
}

// modelStem is a weights file's name without its quantization and extension,
// which is the part a matching projector tends to share.
func modelStem(base string) string {
	base = strings.TrimSuffix(base, filepath.Ext(base))
	if i := strings.LastIndex(base, "-"); i > 0 {
		base = base[:i]
	}
	return base
}

func isProjector(base string) bool {
	lower := strings.ToLower(base)
	return strings.HasPrefix(lower, "mmproj") || strings.Contains(lower, "-mmproj")
}

// shardPattern matches llama.cpp's split naming, e.g. "-00002-of-00005.gguf".
var shardPattern = regexp.MustCompile(`-(\d{5})-of-(\d{5})\.gguf$`)

// shardGroup is the name a split GGUF's pieces share, so the other shards'
// bytes can be added to the one the catalog indexes.
func shardGroup(path string) string {
	base := filepath.Base(path)
	loc := shardPattern.FindStringIndex(strings.ToLower(base))
	if loc == nil {
		return ""
	}
	return filepath.Join(filepath.Dir(path), base[:loc[0]])
}

func isNonFirstShard(base string) bool {
	m := shardPattern.FindStringSubmatch(strings.ToLower(base))
	return m != nil && m[1] != "00001"
}

func describe(absRoot, path string) (Model, error) {
	info, err := os.Stat(path)
	if err != nil {
		return Model{}, err
	}
	rel, err := filepath.Rel(absRoot, path)
	if err != nil {
		return Model{}, err
	}
	rel = filepath.ToSlash(rel)

	// The path inside the models root, without its extension, is always unique
	// and always available.
	pathKey := strings.TrimSuffix(rel, filepath.Ext(rel))
	key := pathKey
	name := filepath.Base(pathKey)

	var publisher string
	if parts := strings.SplitN(rel, "/", 2); len(parts) == 2 {
		publisher = parts[0]
	}

	m := Model{
		Key:         key,
		PathKey:     pathKey,
		Type:        "llm",
		Publisher:   publisher,
		DisplayName: name,
		Format:      "gguf",
		SizeBytes:   info.Size(),
		Path:        path,
	}
	if q := quantPattern.FindString(name); q != "" {
		m.Quantization = strings.ToUpper(q)
	}
	if p := paramsPattern.FindStringSubmatch(name); p != nil {
		m.ParamsString = strings.ToUpper(p[1] + p[2])
	}
	if looksLikeEmbedding(name) {
		m.Type = "embedding"
	}

	// The header beats the filename wherever it has an answer. A file can be
	// renamed to anything; the metadata is what the engine will actually load.
	if meta, err := readGGUF(path); err == nil {
		// Prefer the identifier the model declares about itself. Tools are
		// configured with "qwen/qwen3.8-27b", not with wherever the file sits,
		// so using the path as the key would break existing configs.
		if hub := meta.HubID(); hub != "" {
			m.Key = hub
			if i := strings.Index(hub, "/"); i > 0 {
				m.Publisher = hub[:i]
			}
		}
		// A renamed file keeps its header: quantization and size come from
		// there when the name does not carry them.
		if q, ok := fileTypeNames[meta.FileType]; ok {
			m.Quantization = q
		}
		if m.ParamsString == "" && meta.SizeLabel != "" {
			m.ParamsString = strings.ToUpper(meta.SizeLabel)
		}
		if meta.Architecture != "" {
			m.Architecture = meta.Architecture
		}
		if len(meta.ReasoningEfforts) > 0 {
			m.ReasoningEfforts = meta.ReasoningEfforts
		}
		if meta.ContextLength > 0 {
			m.MaxContextLength = meta.ContextLength
		}
		if p := humanParams(meta.ParamCount); p != "" {
			m.ParamsString = p
		}
		if meta.Embedding {
			m.Type = "embedding"
		}
		m.Layers = meta.BlockCount
		m.Experts = meta.ExpertCount
		if meta.NextNLayers > 0 {
			m.DraftLayers = meta.NextNLayers
			m.Capabilities = append(m.Capabilities, "speculative")
		}
	}
	return m, nil
}

func looksLikeEmbedding(name string) bool {
	lower := strings.ToLower(name)
	for _, hint := range []string{"embed", "bge-", "gte-", "e5-"} {
		if strings.Contains(lower, hint) {
			return true
		}
	}
	return false
}

// Models returns every indexed model, ordered by key.
func (c *Catalog) Models() []Model {
	out := make([]Model, 0, len(c.models))
	for _, m := range c.models {
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

// NewFromModels builds a catalog over a fixed set of models, keyed as Lookup
// will find them. It scans nothing, which is what callers outside this package
// need when the models are already known — a test that must answer "does this
// model carry a projector" without a filesystem behind it.
func NewFromModels(models map[string]Model) *Catalog {
	c := &Catalog{models: map[string]Model{}, aliases: map[string]string{}, variants: map[string]Model{}}
	for k, m := range models {
		c.models[k] = m
	}
	return c
}

// Lookup finds a model by key.
func (c *Catalog) Lookup(key string) (Model, bool) {
	m, ok := c.models[key]
	return m, ok
}

// Resolve finds a model by key, or by unambiguous suffix so operators can type
// "Qwen3.8-27B-Q4_K_M" instead of the full path. An ambiguous suffix is an
// error rather than an arbitrary pick.
// VariantsOf returns every model sharing one canonical key: the primary
// first, then the shadowed ones. A model with a single file returns just
// itself, so callers need no special case.
//
// Two variants are the same model in two shapes — the same weights as GGUF and
// as MLX, or two quantizations — and which one a machine should run is a
// choice ModelFabric cannot make for you: only one of them may have an engine
// installed, and only one of them may honour constrained output.
func (c *Catalog) VariantsOf(key string) []Model {
	var out []Model
	if m, ok := c.models[key]; ok {
		out = append(out, m)
	}
	for _, v := range c.variants {
		if v.Key == key {
			out = append(out, v)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].PathKey < out[j].PathKey })
	return out
}

// ResolveFormat resolves ref and then narrows to the variant in the requested
// weights format. An empty format means "whichever is primary", which is what
// every caller did before variants could be chosen.
func (c *Catalog) ResolveFormat(ref, format string) (Model, error) {
	m, err := c.Resolve(ref)
	if err != nil {
		return Model{}, err
	}
	if format == "" || strings.EqualFold(m.Format, format) {
		return m, nil
	}
	have := []string{m.Format}
	for _, v := range c.VariantsOf(m.Key) {
		if strings.EqualFold(v.Format, format) {
			return v, nil
		}
		if v.PathKey != m.PathKey {
			have = append(have, v.Format)
		}
	}
	slices.Sort(have)
	return Model{}, fmt.Errorf("%s is not available as %s weights here (have %s)",
		m.Key, format, strings.Join(slices.Compact(have), ", "))
}

func (c *Catalog) Resolve(ref string) (Model, error) {
	if m, ok := c.models[ref]; ok {
		return m, nil
	}
	// A path key addresses a specific file, including a shadowed variant.
	if m, ok := c.variants[ref]; ok {
		return m, nil
	}
	if key, ok := c.aliases[ref]; ok {
		return c.models[key], nil
	}
	var matches []Model
	for key, m := range c.models {
		if strings.HasSuffix(key, "/"+ref) || filepath.Base(key) == ref {
			matches = append(matches, m)
		}
	}
	// The Hugging Face repository a model came from also names it, so the
	// argument given to `mfsh get user/repo` works for `mfsh load` too.
	if len(matches) == 0 {
		prefix := strings.ToLower(strings.TrimSuffix(ref, "/")) + "/"
		for _, m := range c.models {
			if strings.HasPrefix(strings.ToLower(m.PathKey), prefix) {
				matches = append(matches, m)
			}
		}
	}
	switch len(matches) {
	case 1:
		return matches[0], nil
	case 0:
		return Model{}, fmt.Errorf("no model matching %q in %s", ref, c.Root)
	default:
		keys := make([]string, len(matches))
		for i, m := range matches {
			keys[i] = m.Key
		}
		sort.Strings(keys)
		return Model{}, fmt.Errorf("%q is ambiguous: %s", ref, strings.Join(keys, ", "))
	}
}
