package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gregbugaj/modelfabric/internal/hub"
)

// Model hub search, metadata and downloads run on this node. The dashboard
// uses /api/v1/nodes/{node}/ to download into another node's models root.

type hubCache struct {
	mu   sync.Mutex
	at   map[string]time.Time
	vals map[string]any
}

// cached stores hub responses by request with an expiry and size limit to
// reduce repeated anonymous Hugging Face API calls.
const (
	hubCacheTTL = 5 * time.Minute
	hubCacheMax = 512
)

func (c *hubCache) cached(key string, fetch func() (any, error)) (any, error) {
	c.mu.Lock()
	if c.at == nil {
		c.at, c.vals = map[string]time.Time{}, map[string]any{}
	}
	if t, ok := c.at[key]; ok && time.Since(t) < hubCacheTTL {
		v := c.vals[key]
		c.mu.Unlock()
		return v, nil
	}
	c.mu.Unlock()
	v, err := fetch()
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	// Request-controlled keys require eviction and a size bound to prevent
	// unbounded growth from distinct queries.
	for k, t := range c.at {
		if time.Since(t) >= hubCacheTTL {
			delete(c.at, k)
			delete(c.vals, k)
		}
	}
	if len(c.at) >= hubCacheMax {
		oldest, at := "", time.Now()
		for k, t := range c.at {
			if t.Before(at) {
				oldest, at = k, t
			}
		}
		delete(c.at, oldest)
		delete(c.vals, oldest)
	}
	c.at[key], c.vals[key] = time.Now(), v
	c.mu.Unlock()
	return v, nil
}

func (s *Server) registerHub(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/hub/search", s.handleHubSearch)
	mux.HandleFunc("GET /api/v1/hub/model", s.handleHubModel)
	mux.HandleFunc("POST /api/v1/models/download", s.handleDownload)
	mux.HandleFunc("POST /api/v1/operations/{id}/cancel", s.handleCancel)
	mux.HandleFunc("GET /api/v1/storage", s.handleStorage)
	mux.HandleFunc("GET /api/v1/hub/avatar", s.handleAvatar)
}

type avatar struct {
	data []byte
	ct   string
}

// handleAvatar serves a publisher's icon through the node, cached, so the
// dashboard never fetches from outside itself.
func (s *Server) handleAvatar(w http.ResponseWriter, r *http.Request) {
	org := r.URL.Query().Get("org")
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	v, err := s.hub.cached("avatar\x00"+org, func() (any, error) {
		b, ct, err := hub.NewClient().Avatar(ctx, org)
		if err != nil {
			return nil, err
		}
		return avatar{b, ct}, nil
	})
	if err != nil {
		http.NotFound(w, r)
		return
	}
	a := v.(avatar)
	w.Header().Set("Content-Type", a.ct)
	w.Header().Set("Cache-Control", "private, max-age=86400")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = w.Write(a.data)
}

type cancellable struct {
	mu sync.Mutex
	m  map[string]context.CancelFunc
}

func (c *cancellable) add(id string, f context.CancelFunc) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = map[string]context.CancelFunc{}
	}
	c.m[id] = f
}

func (c *cancellable) remove(id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.m, id)
}

func (c *cancellable) cancel(id string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	f, ok := c.m[id]
	if ok {
		f()
	}
	return ok
}

// handleCancel stops a running download. Its partial file stays, so a later
// download of the same model resumes where it stopped.
func (s *Server) handleCancel(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !s.cancels.cancel(id) {
		writeError(w, http.StatusNotFound, "no running download "+id+" (only downloads can be cancelled)")
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"cancelled": id})
}

func (s *Server) handleStorage(w http.ResponseWriter, _ *http.Request) {
	if !s.requireSupervisor(w) {
		return
	}
	out := map[string]any{"models_root": s.sup.ModelsRoot(), "entrypoint": s.sup.Entrypoint()}
	var st syscall.Statfs_t
	root := s.sup.ModelsRoot()
	for root != "" {
		if err := syscall.Statfs(root, &st); err == nil {
			out["free_bytes"] = int64(st.Bavail) * int64(st.Bsize)
			out["total_bytes"] = int64(st.Blocks) * int64(st.Bsize)
			break
		}
		if root == "/" {
			break
		}
		// Not created yet: measure where it will be. The loop used to stop
		// before trying "/" itself, so a models root whose parents are all
		// missing reported no free space at all.
		root = filepath.Dir(root)
	}
	hw := s.hardware()
	var gpus []map[string]any
	for _, g := range hw.GPUs {
		gpus = append(gpus, map[string]any{"name": g.Name, "vram_mb": g.MemoryMB})
	}
	out["gpus"] = gpus
	out["memory_mb"] = hw.MemoryMB
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleHubSearch(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	author := r.URL.Query().Get("author")
	switch author {
	case "":
		author = hub.LMStudioCommunity // LM Studio's picks, as its browser starts
	case "all":
		author = ""
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	v, err := s.hub.cached("search\x00"+author+"\x00"+q, func() (any, error) {
		return hub.NewClient().Search(ctx, author, q, 40)
	})
	if err != nil {
		writeError(w, http.StatusBadGateway, "Hugging Face search failed: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"results": v})
}

func (s *Server) handleHubModel(w http.ResponseWriter, r *http.Request) {
	repo := r.URL.Query().Get("repo")
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	v, err := s.hub.cached("model\x00"+repo, func() (any, error) {
		return hub.NewClient().Details(ctx, repo)
	})
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) handleDownload(w http.ResponseWriter, r *http.Request) {
	if !s.requireSupervisor(w) {
		return
	}
	if s.sup.Entrypoint() {
		writeError(w, http.StatusConflict, "this node is an entrypoint and runs no models; download to a GPU node")
		return
	}
	var req struct {
		Repo  string `json:"repo"`
		Quant string `json:"quant"`
	}
	if err := decodeStrict(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	ref, err := hub.ParseRef(req.Repo)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	ref.Quant = strings.ToUpper(strings.TrimSpace(req.Quant))
	root := s.sup.ModelsRoot()
	if root == "" {
		writeError(w, http.StatusConflict, "this node has no models_root to download into")
		return
	}
	name := ref.Repo
	if ref.Quant != "" {
		name += "@" + ref.Quant
	}
	j := s.sup.Journal()
	op, created := j.Begin("download", name, "download:"+name)
	if created {
		go s.runDownload(op.ID, ref, root)
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"operation": op})
}

// runDownload fetches a model into the models root as a durable operation:
// it outlives the request (and the browser tab), reports progress, and the
// catalog is rescanned when it lands. Files are hash-checked by the hub
// client and renamed into place only when complete.
func (s *Server) runDownload(opID string, ref hub.Ref, root string) {
	j := s.sup.Journal()
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Hour)
	defer cancel()
	s.cancels.add(opID, cancel)
	defer s.cancels.remove(opID)
	client := hub.NewClient()
	j.Progress(opID, "resolving "+ref.Repo)
	plan, err := client.Resolve(ctx, ref)
	if err != nil {
		j.Fail(opID, err)
		return
	}
	start, last := time.Now(), time.Time{}
	_, err = client.Download(ctx, plan, root, func(file string, done, total int64) {
		if time.Since(last) < 500*time.Millisecond && done != total {
			return
		}
		last = time.Now()
		rate := float64(done) / max(time.Since(start).Seconds(), 0.001)
		j.Report(opID, fmt.Sprintf("%s: %s of %s (%s/s)", fileBase(file), humanSize(done), humanSize(total), humanSize(int64(rate))),
			float64(done)/float64(max(total, 1)))
	})
	if err != nil {
		if errors.Is(ctx.Err(), context.Canceled) {
			s.log.Info("download cancelled", "repo", plan.Repo, "quant", plan.Quant)
			j.Cancelled(opID)
			return
		}
		j.Fail(opID, err)
		return
	}
	if err := s.sup.Rescan(); err != nil {
		s.log.Warn("rescan after download", "err", err)
	}
	s.log.Info("model downloaded", "repo", plan.Repo, "quant", plan.Quant, "bytes", plan.TotalBytes(), "took", time.Since(start).Round(time.Second))
	j.Succeed(opID, "")
}

func fileBase(p string) string {
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		return p[i+1:]
	}
	return p
}

func humanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}
