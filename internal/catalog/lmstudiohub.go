package catalog

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// LM Studio's hub catalog.
//
// An install keeps an entry per known model under
// ~/.lmstudio/hub/models/<owner>/<name>/, and its manifest.json is the
// authoritative statement of that model's identity:
//
//	{"type":"model","owner":"qwen","name":"qwen3.8-27b",
//	 "dependencies":[{"purpose":"baseModel",
//	   "sources":[{"type":"huggingface","user":"lmstudio-community",
//	               "repo":"Qwen3.8-27B-GGUF"}]}]}
//
// The source user/repo pair is exactly the directory layout under
// ~/.lmstudio/models, which is what lets a file on disk be matched back to the
// name tools are configured with. Reading it beats deriving the name from GGUF
// metadata: a model whose basename or size_label is missing or oddly formatted
// still gets its real identity.
//
// The sibling model.yaml (an open standard, modelyaml.org) carries the rest —
// capabilities, recommended sampling, a memory floor, template variables. See
// modelyaml.go. Identity always comes from manifest.json; a model.yaml that
// fails to parse costs the recommendations, never the model.

// HubEntry is one catalog entry.
type HubEntry struct {
	ID    string   // "qwen/qwen3.8-27b"
	Owner string   // "qwen"
	Name  string   // "qwen3.8-27b"
	Repos []string // "lmstudio-community/Qwen3.8-27B-GGUF", lowercased
	// SourceRepos are the same repositories with their original case, which
	// is what Hugging Face URLs need.
	SourceRepos []string
	Spec        *ModelSpec // from model.yaml; nil when absent or unreadable
	SpecErr     error      // why model.yaml was not used, when it exists
}

type hubManifest struct {
	Type         string `json:"type"`
	Owner        string `json:"owner"`
	Name         string `json:"name"`
	Dependencies []struct {
		Purpose string `json:"purpose"`
		Sources []struct {
			Type string `json:"type"`
			User string `json:"user"`
			Repo string `json:"repo"`
		} `json:"sources"`
	} `json:"dependencies"`
}

// Hub indexes a hub catalog for lookup by source repository.
type Hub struct {
	byRepo map[string]HubEntry // lowercased "user/repo" -> entry
	byID   map[string]HubEntry // lowercased "owner/name" -> entry
}

// LoadHub reads the catalog from an LM Studio root. A missing catalog yields an
// empty Hub rather than an error: it is an enrichment, never a requirement.
func LoadHub(lmStudioRoot string) *Hub {
	h := &Hub{byRepo: map[string]HubEntry{}, byID: map[string]HubEntry{}}
	if lmStudioRoot == "" {
		return h
	}
	root := filepath.Join(lmStudioRoot, "hub", "models")
	owners, err := os.ReadDir(root)
	if err != nil {
		return h
	}
	for _, owner := range owners {
		if !owner.IsDir() {
			continue
		}
		names, err := os.ReadDir(filepath.Join(root, owner.Name()))
		if err != nil {
			continue
		}
		for _, name := range names {
			if !name.IsDir() {
				continue
			}
			e, ok := readHubEntry(filepath.Join(root, owner.Name(), name.Name()))
			if !ok {
				continue
			}
			h.byID[e.ID] = e
			for _, repo := range e.Repos {
				// First entry wins: two hub entries naming one repo would be a
				// catalog inconsistency, and picking arbitrarily is worse than
				// being stable.
				if _, taken := h.byRepo[repo]; !taken {
					h.byRepo[repo] = e
				}
			}
		}
	}
	return h
}

func readHubEntry(dir string) (HubEntry, bool) {
	raw, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return HubEntry{}, false
	}
	var m hubManifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return HubEntry{}, false
	}
	if m.Type != "model" || m.Owner == "" || m.Name == "" {
		return HubEntry{}, false
	}
	e := HubEntry{
		ID:    strings.ToLower(m.Owner + "/" + m.Name),
		Owner: strings.ToLower(m.Owner),
		Name:  strings.ToLower(m.Name),
	}
	for _, dep := range m.Dependencies {
		for _, src := range dep.Sources {
			if src.Type != "huggingface" || src.User == "" || src.Repo == "" {
				continue
			}
			e.Repos = append(e.Repos, strings.ToLower(src.User+"/"+src.Repo))
			e.SourceRepos = append(e.SourceRepos, src.User+"/"+src.Repo)
		}
	}
	if len(e.Repos) == 0 {
		return HubEntry{}, false
	}
	e.Spec, e.SpecErr = readHubSpec(dir)
	if e.SpecErr != nil {
		e.Spec = nil
	}
	return e, true
}

// Len reports how many source repositories are indexed.
func (h *Hub) Len() int {
	if h == nil {
		return 0
	}
	return len(h.byRepo)
}

// LookupPath finds the catalog entry for a model file, given its path relative
// to a models root. The layout is <user>/<repo>/<file>.gguf, so the first two
// segments identify the source repository.
func (h *Hub) LookupPath(relPath string) (HubEntry, bool) {
	if h == nil || len(h.byRepo) == 0 {
		return HubEntry{}, false
	}
	parts := strings.Split(filepath.ToSlash(relPath), "/")
	if len(parts) < 2 {
		return HubEntry{}, false
	}
	e, ok := h.byRepo[strings.ToLower(parts[0]+"/"+parts[1])]
	return e, ok
}

// Lookup finds a catalog entry by hub id ("qwen/qwen3.8-27b").
func (h *Hub) Lookup(id string) (HubEntry, bool) {
	if h == nil {
		return HubEntry{}, false
	}
	e, ok := h.byID[strings.ToLower(id)]
	return e, ok
}

// Entries lists every catalog entry.
func (h *Hub) Entries() []HubEntry {
	if h == nil {
		return nil
	}
	out := make([]HubEntry, 0, len(h.byID))
	for _, e := range h.byID {
		out = append(out, e)
	}
	return out
}
