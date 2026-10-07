// Package hub downloads GGUF models from Hugging Face.
//
// References are resolved to an immutable commit before downloading, so a
// resumed file cannot mix bytes from different revisions. Published SHA-256
// digests are checked before files become available to load.
//
// Hashing happens while the bytes stream to disk, so verification costs no
// second pass over a multi-gigabyte file.
package hub

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gregbugaj/modelfabric/internal/download"
)

const DefaultBaseURL = "https://huggingface.co"

type Ref struct {
	Repo  string // "user/repo"
	Quant string // "Q4_K_M", or "" for the default choice
}

var repoPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*/[A-Za-z0-9][A-Za-z0-9._-]*$`)

// ParseRef accepts the forms `lms get` does:
//
//	user/repo
//	user/repo@q4_k_m
//	https://huggingface.co/user/repo[/anything]
func ParseRef(s string) (Ref, error) {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://") {
		u, err := url.Parse(s)
		if err != nil {
			return Ref{}, fmt.Errorf("invalid URL %q: %w", s, err)
		}
		if !strings.HasSuffix(u.Host, "huggingface.co") {
			return Ref{}, fmt.Errorf("only huggingface.co URLs are supported, got %q", u.Host)
		}
		parts := strings.Split(strings.Trim(u.Path, "/"), "/")
		if len(parts) < 2 {
			return Ref{}, fmt.Errorf("URL %q does not name a repository", s)
		}
		s = parts[0] + "/" + parts[1]
	}
	var r Ref
	if i := strings.LastIndex(s, "@"); i > 0 {
		r.Quant = strings.ToUpper(s[i+1:])
		s = s[:i]
	}
	if !repoPattern.MatchString(s) {
		return Ref{}, fmt.Errorf("%q is not a Hugging Face repository (expected user/repo)", s)
	}
	r.Repo = s
	return r, nil
}

type File struct {
	Name   string // path inside the repository
	Size   int64
	SHA256 string // lowercase hex; empty if the hub published none
}

// Plan is a resolved download: a repository pinned to one commit.
type Plan struct {
	Repo     string
	Revision string // commit SHA
	Files    []File
	Quant    string
}

func (p Plan) TotalBytes() int64 {
	var n int64
	for _, f := range p.Files {
		n += f.Size
	}
	return n
}

type Client struct {
	BaseURL string
	HTTP    *http.Client
	Token   string // optional, for gated repositories
}

// NewClient returns a client for the public hub, honouring HF_TOKEN.
func NewClient() *Client {
	return &Client{
		BaseURL: DefaultBaseURL,
		// No overall timeout: a large file legitimately takes minutes. The
		// transport bounds connection setup instead.
		HTTP:  &http.Client{Transport: &http.Transport{ResponseHeaderTimeout: 60 * time.Second}},
		Token: os.Getenv("HF_TOKEN"),
	}
}

type apiModel struct {
	SHA      string `json:"sha"`
	Siblings []struct {
		Name string `json:"rfilename"`
		Size int64  `json:"size"`
		LFS  *struct {
			SHA256 string `json:"sha256"`
			Size   int64  `json:"size"`
		} `json:"lfs"`
	} `json:"siblings"`
}

func (c *Client) get(ctx context.Context, u string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	return c.HTTP.Do(req)
}

// Resolve pins a reference to a commit and chooses the files to fetch.
func (c *Client) Resolve(ctx context.Context, ref Ref) (*Plan, error) {
	resp, err := c.get(ctx, c.BaseURL+"/api/models/"+ref.Repo+"?blobs=true")
	if err != nil {
		return nil, fmt.Errorf("query %s: %w", ref.Repo, err)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusUnauthorized, http.StatusForbidden:
		return nil, fmt.Errorf("%s is gated or private; set HF_TOKEN", ref.Repo)
	case http.StatusNotFound:
		return nil, fmt.Errorf("no repository %s on Hugging Face", ref.Repo)
	default:
		return nil, fmt.Errorf("query %s: %s", ref.Repo, resp.Status)
	}
	var m apiModel
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<20)).Decode(&m); err != nil {
		return nil, fmt.Errorf("parse %s: %w", ref.Repo, err)
	}
	if m.SHA == "" {
		return nil, fmt.Errorf("%s did not report a commit; refusing an unpinned download", ref.Repo)
	}

	var ggufs []File
	for _, s := range m.Siblings {
		if !strings.EqualFold(path.Ext(s.Name), ".gguf") {
			continue
		}
		f := File{Name: s.Name, Size: s.Size}
		if s.LFS != nil {
			f.SHA256 = strings.ToLower(s.LFS.SHA256)
			if f.Size == 0 {
				f.Size = s.LFS.Size
			}
		}
		ggufs = append(ggufs, f)
	}
	if len(ggufs) == 0 {
		return nil, fmt.Errorf("%s has no GGUF files", ref.Repo)
	}

	weights, quant, err := chooseWeights(ggufs, ref.Quant)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", ref.Repo, err)
	}
	files := weights
	// A vision model needs its projector, or it loads and silently answers
	// text-only; so it is fetched with the weights, not left as an extra.
	if proj := chooseProjector(ggufs, weights); proj != nil {
		files = append(files, *proj)
	}
	return &Plan{Repo: ref.Repo, Revision: m.SHA, Files: files, Quant: quant}, nil
}

var shardParts = regexp.MustCompile(`(?i)^(.*)-(\d{5})-of-(\d{5})\.gguf$`)

// oneCompleteShardSet reports whether these files are all the shards of one
// model, and all of them.
func oneCompleteShardSet(files []File) error {
	prefix, total := "", 0
	seen := map[int]bool{}
	for _, f := range files {
		m := shardParts.FindStringSubmatch(f.Name)
		if m == nil {
			return fmt.Errorf("they are not shards")
		}
		idx, _ := strconv.Atoi(m[2])
		of, _ := strconv.Atoi(m[3])
		switch {
		case prefix == "":
			prefix, total = m[1], of
		case m[1] != prefix:
			return fmt.Errorf("they are shards of different models")
		case of != total:
			return fmt.Errorf("they disagree on how many shards there are")
		}
		if seen[idx] {
			return fmt.Errorf("shard %d appears twice", idx)
		}
		seen[idx] = true
	}
	if len(seen) != total {
		return fmt.Errorf("only %d of %d shards are present", len(seen), total)
	}
	return nil
}

var shard = regexp.MustCompile(`(?i)-\d{5}-of-\d{5}\.gguf$`)

func isProjector(name string) bool {
	b := strings.ToLower(path.Base(name))
	return strings.HasPrefix(b, "mmproj") || strings.Contains(b, "-mmproj")
}

var quantOf = regexp.MustCompile(`(?i)(IQ\d+_[A-Z0-9_]+|Q\d+_K_[A-Z]+|Q\d+_K|Q\d+_\d+|BF16|F16|F32)`)

// preference is the default order when no quantization is requested: the
// common quality/size sweet spot first, full precision last.
var preference = []string{"Q4_K_M", "Q4_K_S", "Q4_0", "Q5_K_M", "Q6_K", "Q8_0", "BF16", "F16", "F32"}

func chooseWeights(files []File, want string) ([]File, string, error) {
	byQuant := map[string][]File{}
	for _, f := range files {
		if isProjector(f.Name) {
			continue
		}
		q := strings.ToUpper(quantOf.FindString(path.Base(f.Name)))
		if q == "" {
			q = "UNKNOWN"
		}
		byQuant[q] = append(byQuant[q], f)
	}
	if len(byQuant) == 0 {
		return nil, "", errors.New("only projector files found, no weights")
	}

	pick := ""
	if want != "" {
		if _, ok := byQuant[want]; !ok {
			return nil, "", fmt.Errorf("no %s quantization; available: %s", want, strings.Join(keys(byQuant), ", "))
		}
		pick = want
	} else {
		for _, q := range preference {
			if _, ok := byQuant[q]; ok {
				pick = q
				break
			}
		}
		if pick == "" {
			pick = keys(byQuant)[0]
		}
	}

	chosen := byQuant[pick]
	// Several files at one quantization are either shards of one model (all
	// needed) or unrelated variants (ambiguous). Refuse the latter rather
	// than guess.
	if len(chosen) > 1 {
		names := make([]string, len(chosen))
		for i, c := range chosen {
			names[i] = c.Name
		}
		// Looking shard-shaped is not the same as being one set. Checking only
		// the suffix accepted two different models' shards mixed together, and
		// an incomplete set ("1 of 3" with two files); either of which
		// downloads something that cannot load.
		if err := oneCompleteShardSet(chosen); err != nil {
			return nil, "", fmt.Errorf("several %s files: %w: %s", pick, err, strings.Join(names, ", "))
		}
	}
	sort.Slice(chosen, func(i, j int) bool { return chosen[i].Name < chosen[j].Name })
	return chosen, pick, nil
}

// chooseProjector picks the projector for the weights being downloaded. A repo
// holding several model variants holds several projectors, and the best-ranked
// one globally need not belong to the variant selected; pairing the wrong one
// makes a model claim vision it cannot do.
func chooseProjector(files []File, weights []File) *File {
	family := ""
	if len(weights) > 0 {
		family = strings.ToUpper(weightsFamily(weights[0].Name))
	}
	var best, matched *File
	rank := func(name string) int {
		n := strings.ToUpper(name)
		switch {
		case strings.Contains(n, "F16") && !strings.Contains(n, "BF16"):
			return 3
		case strings.Contains(n, "BF16"):
			return 2
		case strings.Contains(n, "F32"):
			return 1
		}
		return 0
	}
	for i := range files {
		if !isProjector(files[i].Name) {
			continue
		}
		if best == nil || rank(files[i].Name) > rank(best.Name) {
			best = &files[i]
		}
		if family != "" && strings.Contains(strings.ToUpper(files[i].Name), family) {
			if matched == nil || rank(files[i].Name) > rank(matched.Name) {
				matched = &files[i]
			}
		}
	}
	if matched != nil {
		return matched
	}
	return best
}

// weightsFamily is a weights file's name without its shard marker,
// quantization and extension; the part a matching projector tends to share.
func weightsFamily(name string) string {
	name = strings.TrimSuffix(name, path.Ext(name))
	if m := shardParts.FindStringSubmatch(name + ".gguf"); m != nil {
		name = m[1]
	}
	if i := strings.LastIndex(name, "-"); i > 0 {
		name = name[:i]
	}
	return name
}

func keys(m map[string][]File) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Progress receives download progress. done and total are bytes across the
// whole plan.
type Progress func(file string, done, total int64)

// ErrChecksum reports a download whose content does not match the hub's hash.
var ErrChecksum = errors.New("checksum mismatch")

// Download fetches a plan into destRoot/<user>/<repo>/, the same layout the
// catalog scans. A partial file from an interrupted run is resumed, and a file
// already present with the right size and hash is skipped.
//
// Each file lands under a ".part" name and is renamed only after its hash
// checks out, so a model that is visible to the catalog is always complete.
func (c *Client) Download(ctx context.Context, p *Plan, destRoot string, progress Progress) (string, error) {
	dir := filepath.Join(destRoot, filepath.FromSlash(p.Repo))
	return dir, c.DownloadInto(ctx, p, dir, progress)
}

// DownloadInto is Download with the destination directory chosen by the
// caller. A copy from a peer lands where the model sat on that peer, which
// need not be <user>/<repo>, while p.Repo still names it in the URL.
func (c *Client) DownloadInto(ctx context.Context, p *Plan, dir string, progress Progress) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	total := p.TotalBytes()
	var base int64
	for _, f := range p.Files {
		// The repository path is kept, not flattened to a basename: a plan
		// holding two files called the same thing in different directories
		// wrote both to one destination, and the second could "resume" the
		// first because the size matched.
		rel, err := safeRel(f.Name)
		if err != nil {
			return err
		}
		dest := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return err
		}
		if err := c.fetch(ctx, p, f, dest, func(n int64) {
			if progress != nil {
				progress(f.Name, base+n, total)
			}
		}); err != nil {
			return fmt.Errorf("%s: %w", f.Name, err)
		}
		base += f.Size
	}
	return nil
}

// joinEscaped percent-escapes each segment of a slash-separated path, leaving
// the separators alone.
func joinEscaped(p string) string {
	parts := strings.Split(p, "/")
	for i, seg := range parts {
		parts[i] = url.PathEscape(seg)
	}
	return strings.Join(parts, "/")
}

// safeRel turns a repository path into a relative local path, refusing any
// that would land outside the destination directory.
func safeRel(name string) (string, error) {
	clean := path.Clean("/" + name)
	rel := strings.TrimPrefix(clean, "/")
	if rel == "" || rel == "." {
		return "", fmt.Errorf("file %q has no name", name)
	}
	return filepath.FromSlash(rel), nil
}

func (c *Client) fetch(ctx context.Context, p *Plan, f File, dest string, onBytes func(int64)) error {
	if info, err := os.Stat(dest); err == nil && info.Size() == f.Size {
		if f.SHA256 == "" {
			onBytes(f.Size)
			return nil
		}
		if sum, err := fileSHA256(dest); err == nil && sum == f.SHA256 {
			onBytes(f.Size)
			return nil
		}
	}

	part := dest + ".part"
	h := sha256.New()
	var have int64
	if info, err := os.Stat(part); err == nil && info.Size() < f.Size {
		// Resume: hash what is already there so the final digest covers the
		// whole file, then ask the server for the rest.
		existing, err := os.Open(part)
		if err != nil {
			return err
		}
		have, err = io.Copy(h, existing)
		existing.Close()
		if err != nil {
			return err
		}
	} else {
		_ = os.Remove(part)
	}
	onBytes(have)

	// Pinned to the resolved commit, never "main": the bytes cannot change
	// underneath a resumed or repeated download.
	// Each component is escaped: a Hugging Face filename may legally contain
	// "#", "?", "%" or a space, and pasted raw they become a fragment, a query
	// or a bad escape rather than part of the path.
	u := c.BaseURL + "/" + joinEscaped(p.Repo) + "/resolve/" + joinEscaped(p.Revision) + "/" + joinEscaped(f.Name)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	if have > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", have))
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	flags := os.O_CREATE | os.O_WRONLY
	switch {
	case have > 0 && resp.StatusCode == http.StatusPartialContent:
		if err := download.ValidateRange(resp, have, f.Size); err != nil {
			return err
		}
		flags |= os.O_APPEND
	case resp.StatusCode == http.StatusOK:
		// The server ignored the range: start over rather than append a whole
		// file onto a partial one.
		have = 0
		h.Reset()
		flags |= os.O_TRUNC
	default:
		return fmt.Errorf("download: %s", resp.Status)
	}

	out, err := os.OpenFile(part, flags, 0o644)
	if err != nil {
		return err
	}
	written := have
	buf := make([]byte, 1<<20)
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := out.Write(buf[:n]); werr != nil {
				out.Close()
				return werr
			}
			h.Write(buf[:n])
			written += int64(n)
			onBytes(written)
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			out.Close()
			// Keep the partial file: the next run resumes from here.
			return rerr
		}
	}
	if err := out.Close(); err != nil {
		return err
	}

	if written != f.Size {
		return fmt.Errorf("got %d bytes, expected %d", written, f.Size)
	}
	if f.SHA256 != "" {
		if got := hex.EncodeToString(h.Sum(nil)); got != f.SHA256 {
			// A corrupt partial must not be resumed from next time.
			_ = os.Remove(part)
			return fmt.Errorf("%w: got %s, expected %s", ErrChecksum, got, f.SHA256)
		}
	}
	return os.Rename(part, dest)
}

func fileSHA256(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// LMStudioCommunity is the organisation that publishes the GGUF builds behind
// LM Studio's hub ids.
const LMStudioCommunity = "lmstudio-community"

// SearchGGUF lists an author's GGUF repositories whose name contains query.
func (c *Client) SearchGGUF(ctx context.Context, author, query string) ([]string, error) {
	u := c.BaseURL + "/api/models?limit=50&author=" + url.QueryEscape(author) + "&search=" + url.QueryEscape(query)
	resp, err := c.get(ctx, u)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("search %s: %s", author, resp.Status)
	}
	var found []struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&found); err != nil {
		return nil, fmt.Errorf("search %s: %w", author, err)
	}
	var out []string
	for _, f := range found {
		if strings.HasSuffix(strings.ToLower(f.ID), "-gguf") {
			out = append(out, f.ID)
		}
	}
	sort.Strings(out)
	return out, nil
}

// HubGGUFRepo maps an LM Studio hub id to the GGUF repository that serves it,
// using only public Hugging Face data: LM Studio's staff-picked models are
// published as lmstudio-community/<Name>-GGUF. An exact name match is
// returned; otherwise the near misses, for the caller to offer.
//
// LM Studio's own catalog API would answer this directly, but it is not a
// public interface, and ModelFabric does not build on private endpoints.
func (c *Client) HubGGUFRepo(ctx context.Context, id string) (repo string, nearMisses []string, err error) {
	owner, name, ok := strings.Cut(id, "/")
	if !ok || name == "" {
		return "", nil, fmt.Errorf("%q is not a hub id (expected owner/name)", id)
	}
	if strings.EqualFold(owner, LMStudioCommunity) || strings.HasSuffix(strings.ToLower(name), "-gguf") {
		return "", nil, nil
	}
	found, err := c.SearchGGUF(ctx, LMStudioCommunity, name)
	if err != nil {
		return "", nil, err
	}
	want := strings.ToLower(LMStudioCommunity + "/" + name + "-gguf")
	for _, f := range found {
		if strings.ToLower(f) == want {
			return f, nil, nil
		}
	}
	return "", found, nil
}
