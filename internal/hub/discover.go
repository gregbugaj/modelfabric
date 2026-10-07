package hub

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Discover: what the dashboard's model browser shows; LM Studio's search
// and model page; from public Hugging Face data only.

type Summary struct {
	Repo         string    `json:"repo"`
	Author       string    `json:"author"`
	Name         string    `json:"name"`
	Downloads    int64     `json:"downloads"`
	Likes        int64     `json:"likes"`
	LastModified time.Time `json:"last_modified"`
	Pipeline     string    `json:"pipeline,omitempty"`
	BaseModel    string    `json:"base_model,omitempty"`
	Vision       bool      `json:"vision,omitempty"`
	Model
}

// Model is what a repository's GGUF header says, as Hugging Face publishes
// it: enough to show a model's size, architecture and capabilities before
// downloading it.
type Model struct {
	Params        string `json:"params,omitempty"` // "27.3B"
	Architecture  string `json:"architecture,omitempty"`
	ContextLength int64  `json:"context_length,omitempty"`
	// Creator is who made the model (the base model's owner), not who
	// quantized it: the name and icon LM Studio shows.
	Creator string `json:"creator,omitempty"`
	// Pick marks LM Studio's own community uploads.
	Pick      bool `json:"pick,omitempty"`
	Tools     bool `json:"tools,omitempty"`
	Reasoning bool `json:"reasoning,omitempty"`
}

type ggufInfo struct {
	Total         int64  `json:"total"`
	Architecture  string `json:"architecture"`
	ContextLength int64  `json:"context_length"`
	ChatTemplate  string `json:"chat_template"`
}

// modelInfo derives what the dashboard shows from the GGUF header. Tool use
// and reasoning come from the chat template itself: a template that renders
// tools can call them; one that emits a thinking block reasons.
func modelInfo(author, baseModel string, g ggufInfo) Model {
	m := Model{Architecture: g.Architecture, ContextLength: g.ContextLength, Params: paramsLabel(g.Total),
		Pick: strings.EqualFold(author, LMStudioCommunity)}
	if org, _, ok := strings.Cut(baseModel, "/"); ok {
		m.Creator = org
	} else if !m.Pick {
		m.Creator = author
	}
	t := g.ChatTemplate
	m.Tools = strings.Contains(t, "tools")
	m.Reasoning = strings.Contains(t, "<think>") || strings.Contains(t, "enable_thinking") ||
		strings.Contains(t, "reasoning") || strings.Contains(t, "thinking")
	return m
}

func paramsLabel(n int64) string {
	switch {
	case n >= 1e9:
		return strings.TrimSuffix(fmt.Sprintf("%.1f", float64(n)/1e9), ".0") + "B"
	case n >= 1e6:
		return fmt.Sprintf("%dM", int64(float64(n)/1e6+0.5))
	case n > 0:
		return fmt.Sprint(n)
	}
	return ""
}

// Search lists GGUF repositories, most downloaded first. author "" searches
// every publisher; LM Studio's own picks are author "lmstudio-community".
// allowedAvatarHost is an exact host match, not a suffix: "huggingface.co" as
// a suffix also accepts "evil-huggingface.co".
func allowedAvatarHost(host string) bool {
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	return host == "huggingface.co" || strings.HasSuffix(host, ".huggingface.co")
}

func (c *Client) Search(ctx context.Context, author, query string, limit int) ([]Summary, error) {
	if limit <= 0 || limit > 100 {
		limit = 40
	}
	q := url.Values{}
	q.Set("filter", "gguf")
	q.Set("sort", "downloads")
	q.Set("direction", "-1")
	q.Set("limit", fmt.Sprint(limit))
	if author != "" {
		q.Set("author", author)
	}
	if query != "" {
		q.Set("search", query)
	}
	for _, e := range []string{"downloads", "likes", "lastModified", "pipeline_tag", "tags", "gguf"} {
		q.Add("expand[]", e)
	}
	resp, err := c.get(ctx, c.BaseURL+"/api/models?"+q.Encode())
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("search: %s", resp.Status)
	}
	var found []struct {
		ID           string    `json:"id"`
		Downloads    int64     `json:"downloads"`
		Likes        int64     `json:"likes"`
		LastModified time.Time `json:"lastModified"`
		Pipeline     string    `json:"pipeline_tag"`
		Tags         []string  `json:"tags"`
		GGUF         ggufInfo  `json:"gguf"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 32<<20)).Decode(&found); err != nil {
		return nil, fmt.Errorf("search: %w", err)
	}
	out := make([]Summary, 0, len(found))
	for _, f := range found {
		author, name, _ := strings.Cut(f.ID, "/")
		s := Summary{Repo: f.ID, Author: author, Name: strings.TrimSuffix(strings.TrimSuffix(name, "-GGUF"), "-gguf"),
			Downloads: f.Downloads, Likes: f.Likes, LastModified: f.LastModified, Pipeline: f.Pipeline,
			Vision: f.Pipeline == "image-text-to-text"}
		for _, t := range f.Tags {
			if b, ok := strings.CutPrefix(t, "base_model:quantized:"); ok {
				s.BaseModel = b
			}
		}
		s.Model = modelInfo(author, s.BaseModel, f.GGUF)
		out = append(out, s)
	}
	return out, nil
}

// QuantOption is one way to download a repository: a quantization and the
// files it takes (shards together), plus the vision projector when there is
// one, since the weights alone would load text-only.
type QuantOption struct {
	Quant     string   `json:"quant"`
	Files     []string `json:"files"`
	Bytes     int64    `json:"bytes"`
	Projector string   `json:"projector,omitempty"`
	// Recommended marks the choice `mfsh get` makes with no quantization.
	Recommended bool `json:"recommended,omitempty"`
}

type Details struct {
	Repo         string        `json:"repo"`
	Revision     string        `json:"revision"`
	Downloads    int64         `json:"downloads"`
	Likes        int64         `json:"likes"`
	LastModified time.Time     `json:"last_modified"`
	License      string        `json:"license,omitempty"`
	BaseModel    string        `json:"base_model,omitempty"`
	Pipeline     string        `json:"pipeline,omitempty"`
	Options      []QuantOption `json:"options"`
	Readme       string        `json:"readme"`
	Vision       bool          `json:"vision,omitempty"`
	Model
}

func (c *Client) Details(ctx context.Context, repo string) (*Details, error) {
	if !repoPattern.MatchString(repo) {
		return nil, fmt.Errorf("%q is not a repository (user/repo)", repo)
	}
	resp, err := c.get(ctx, c.BaseURL+"/api/models/"+repo+"?blobs=true")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		return nil, fmt.Errorf("no repository %s on Hugging Face", repo)
	default:
		return nil, fmt.Errorf("%s: %s", repo, resp.Status)
	}
	var m struct {
		apiModel
		Downloads    int64     `json:"downloads"`
		Likes        int64     `json:"likes"`
		LastModified time.Time `json:"lastModified"`
		Pipeline     string    `json:"pipeline_tag"`
		CardData     struct {
			License   any `json:"license"`
			BaseModel any `json:"base_model"`
		} `json:"cardData"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<20)).Decode(&m); err != nil {
		return nil, fmt.Errorf("parse %s: %w", repo, err)
	}
	d := &Details{Repo: repo, Revision: m.SHA, Downloads: m.Downloads, Likes: m.Likes,
		LastModified: m.LastModified, Pipeline: m.Pipeline,
		License: firstString(m.CardData.License), BaseModel: firstString(m.CardData.BaseModel)}

	var ggufs []File
	for _, s := range m.Siblings {
		if strings.EqualFold(path.Ext(s.Name), ".gguf") {
			f := File{Name: s.Name, Size: s.Size}
			if s.LFS != nil && f.Size == 0 {
				f.Size = s.LFS.Size
			}
			ggufs = append(ggufs, f)
		}
	}
	d.Options = quantOptions(ggufs)
	d.Readme = c.readme(ctx, repo)
	author, _, _ := strings.Cut(repo, "/")
	d.Model = modelInfo(author, d.BaseModel, c.ggufHeader(ctx, repo))
	d.Vision = m.Pipeline == "image-text-to-text" || (len(d.Options) > 0 && d.Options[0].Projector != "")
	return d, nil
}

// quantOptions groups a repository's GGUF files by quantization, the same way
// the downloader chooses them, sorted by size.
func quantOptions(ggufs []File) []QuantOption {
	byQuant := map[string][]File{}
	for _, f := range ggufs {
		if isProjector(f.Name) {
			continue
		}
		q := strings.ToUpper(quantOf.FindString(path.Base(f.Name)))
		if q == "" {
			q = "UNKNOWN"
		}
		byQuant[q] = append(byQuant[q], f)
	}
	_, rec, _ := chooseWeights(ggufs, "")
	var out []QuantOption
	for q, files := range byQuant {
		o := QuantOption{Quant: q, Recommended: q == rec}
		for _, f := range files {
			o.Files = append(o.Files, f.Name)
			o.Bytes += f.Size
		}
		sort.Strings(o.Files)
		// Per option: a repo with several model variants has several
		// projectors, and each option needs the one that belongs to it.
		if proj := chooseProjector(ggufs, files); proj != nil {
			o.Projector = proj.Name
			o.Bytes += proj.Size
		}
		out = append(out, o)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Bytes < out[j].Bytes })
	return out
}

// ggufHeader is the GGUF metadata Hugging Face extracted from a repository;
// zero when it has none.
func (c *Client) ggufHeader(ctx context.Context, repo string) ggufInfo {
	var out struct {
		GGUF ggufInfo `json:"gguf"`
	}
	resp, err := c.get(ctx, c.BaseURL+"/api/models/"+repo+"?expand[]=gguf")
	if err != nil {
		return out.GGUF
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		_ = json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&out)
	}
	return out.GGUF
}

// Avatar fetches an organization's or user's picture from Hugging Face, for
// the dashboard's publisher icons. Only Hugging Face's own avatar host is
// followed.
func (c *Client) Avatar(ctx context.Context, org string) (data []byte, contentType string, err error) {
	if !regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,95}$`).MatchString(org) {
		return nil, "", fmt.Errorf("invalid name %q", org)
	}
	var meta struct {
		AvatarURL string `json:"avatarUrl"`
	}
	for _, kind := range []string{"organizations", "users"} {
		resp, err := c.get(ctx, c.BaseURL+"/api/"+kind+"/"+org+"/avatar")
		if err != nil {
			return nil, "", err
		}
		if resp.StatusCode == http.StatusOK {
			_ = json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&meta)
		}
		resp.Body.Close()
		if meta.AvatarURL != "" {
			break
		}
	}
	u, err := url.Parse(meta.AvatarURL)
	if err != nil || u.Scheme != "https" || !allowedAvatarHost(u.Host) {
		return nil, "", fmt.Errorf("no avatar for %s", org)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, "", err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, "", err
	}
	// A redirect can leave the allowlist after the first URL was checked, so
	// where the bytes actually came from is checked too.
	if final := resp.Request.URL; final != nil && (final.Scheme != "https" || !allowedAvatarHost(final.Host)) {
		resp.Body.Close()
		return nil, "", fmt.Errorf("avatar for %s redirected off huggingface.co", org)
	}
	defer resp.Body.Close()
	ct := resp.Header.Get("Content-Type")
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(ct, "image/") {
		return nil, "", fmt.Errorf("avatar for %s: %s %s", org, resp.Status, ct)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 512<<10))
	return b, ct, err
}

// readme returns the repository's model card without its YAML front matter;
// "" when there is none.
func (c *Client) readme(ctx context.Context, repo string) string {
	resp, err := c.get(ctx, c.BaseURL+"/"+repo+"/resolve/main/README.md")
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ""
	}
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 256<<10))
	s := string(b)
	if rest, ok := strings.CutPrefix(s, "---\n"); ok {
		if _, after, found := strings.Cut(rest, "\n---"); found {
			s = strings.TrimLeft(after, "-\n")
		}
	}
	return strings.TrimSpace(s)
}

func firstString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case []any:
		if len(t) > 0 {
			if s, ok := t[0].(string); ok {
				return s
			}
		}
	}
	return ""
}
