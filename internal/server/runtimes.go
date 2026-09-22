package server

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/gregbugaj/modelfabric/internal/rtpkg"
	"github.com/gregbugaj/modelfabric/internal/runtime"
)

// Runtime management for the dashboard: what LM Studio's runtime page does —
// check for builds, install, update, remove — against ModelFabric's own runtimes
// directory. Installs run as durable operations in the same journal as model
// loads, so the page shows progress by polling what it already polls, and a
// closed tab does not cancel a download.

// SetRuntimesRoot enables installing and removing ModelFabric's own runtimes.
func (s *Server) SetRuntimesRoot(root string) { s.runtimesRoot = root }

// availableCache keeps upstream's release list for a while: the page asks
// on demand, and GitHub allows 60 anonymous API calls an hour.
type availableCache struct {
	mu     sync.Mutex
	at     time.Time
	builds []rtpkg.Build
}

const availableTTL = 10 * time.Minute

func (s *Server) upstreamBuilds(ctx context.Context, refresh bool) ([]rtpkg.Build, error) {
	// A fresh cache is answered without holding the lock across the upstream
	// request: the fetch can take 30s, and every reader that only wanted the
	// cached value used to queue behind it.
	s.available.mu.Lock()
	if !refresh && s.available.builds != nil && time.Since(s.available.at) < availableTTL {
		builds := s.available.builds
		s.available.mu.Unlock()
		return builds, nil
	}
	s.available.mu.Unlock()

	lctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	builds, err := rtpkg.NewClient().Builds(lctx, 30)
	if err != nil {
		return nil, err
	}
	s.available.mu.Lock()
	s.available.builds, s.available.at = builds, time.Now()
	s.available.mu.Unlock()
	return builds, nil
}

type planView struct {
	Name        string `json:"name"`
	DisplayName string `json:"display_name"`
	Build       string `json:"build"`
	Backend     string `json:"backend"`
	// BackendVersion is the CUDA (or ROCm) version the build targets.
	BackendVersion string `json:"backend_version,omitempty"`
	Reason         string `json:"reason"`
	DownloadBytes  int64  `json:"download_bytes"`
	Installed      bool   `json:"installed"`
}

func (s *Server) planView(p *rtpkg.Plan) planView {
	v := planView{Name: p.RuntimeName(), DisplayName: p.DisplayName, Build: p.Build, Backend: p.Backend, BackendVersion: p.BackendVersion,
		Reason: p.Reason, DownloadBytes: p.Engine.Size}
	if p.Vendor != nil {
		if _, err := os.Stat(filepath.Join(s.runtimesRoot, "vendor", p.VendorName)); err != nil {
			v.DownloadBytes += p.Vendor.Size // shared runtime not yet present
		}
	}
	if _, err := os.Stat(filepath.Join(s.runtimesRoot, p.Dir())); err == nil {
		v.Installed = true
	}
	return v
}

// handleRuntimesAvailable answers "what could I install, and is anything
// out of date" — the page's Check for updates.
func (s *Server) handleRuntimesAvailable(w http.ResponseWriter, r *http.Request) {
	if !s.requireRuntimeInstall(w) {
		return
	}
	builds, err := s.upstreamBuilds(r.Context(), r.URL.Query().Get("refresh") == "1")
	if err != nil {
		writeError(w, http.StatusBadGateway, "could not reach upstream llama.cpp releases: "+err.Error())
		return
	}
	hw := s.hardware()
	resp := map[string]any{}
	options := map[string]any{}
	for _, backend := range []string{"", "cuda", "vulkan", "cpu", "rocm"} {
		// Offer only what can work here; the install would fail its device
		// probe otherwise. CPU always works.
		if (backend == "cuda" && len(hw.GPUs) == 0) || (backend == "vulkan" && !hw.Vulkan) ||
			(backend == "rocm" && !hw.ROCm) {
			continue
		}
		p, err := rtpkg.Choose(builds, rtpkg.Want{Backend: backend}, hw)
		key := backend
		if key == "" {
			key = "recommended"
		}
		if err != nil {
			options[key] = map[string]string{"error": err.Error()}
			continue
		}
		options[key] = s.planView(p)
	}
	resp["options"] = options
	updates, err := rtpkg.Updates(s.runtimesRoot, builds, hw)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	type updateView struct {
		rtpkg.Update
		Latest *planView `json:"latest,omitempty"`
	}
	views := make([]updateView, 0, len(updates))
	for _, u := range updates {
		v := updateView{Update: u}
		if u.Latest != nil {
			pv := s.planView(u.Latest)
			v.Latest = &pv
		}
		views = append(views, v)
	}
	resp["updates"] = views
	if len(builds) > 0 {
		resp["newest_build"] = builds[0].Tag
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) hardware() runtime.Hardware {
	if reg := s.sup.Runtimes(); reg != nil {
		if hw := reg.Hardware(); hw != nil {
			return *hw
		}
	}
	return runtime.Survey(context.Background())
}

// handleRuntimeGet starts an install and returns its operation. The request
// names a backend, a build and a CUDA version, each optional.
func (s *Server) handleRuntimeGet(w http.ResponseWriter, r *http.Request) {
	if !s.requireRuntimeInstall(w) {
		return
	}
	var req struct {
		Backend string `json:"backend"`
		Build   string `json:"build"`
		CUDA    string `json:"cuda"`
	}
	if err := decodeBody(w, r, &req, 1<<20); err != nil {
		writeError(w, http.StatusBadRequest, "parse request body: "+err.Error())
		return
	}
	builds, err := s.upstreamBuilds(r.Context(), false)
	if err != nil {
		writeError(w, http.StatusBadGateway, "could not reach upstream llama.cpp releases: "+err.Error())
		return
	}
	hw := s.hardware()
	plan, err := rtpkg.Choose(builds, rtpkg.Want{Backend: req.Backend, Build: req.Build, CUDA: req.CUDA}, hw)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if s.planView(plan).Installed {
		writeError(w, http.StatusConflict, plan.RuntimeName()+" is already installed")
		return
	}
	j := s.sup.Journal()
	op, created := j.Begin("runtime-get", plan.RuntimeName(), "runtime-get:"+plan.RuntimeName())
	if created {
		go s.runRuntimeInstall(op.ID, plan, hw)
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"operation": op})
}

// runtimeInstalls serializes installs. The journal's dedupe key is the runtime
// name, which only stops the same runtime being installed twice; two different
// runtimes still share vendor/<VendorName> and .staging-* under the runtimes
// root, so overlapping installs could stage into each other.
var runtimeInstalls sync.Mutex

func (s *Server) runRuntimeInstall(opID string, plan *rtpkg.Plan, hw runtime.Hardware) {
	runtimeInstalls.Lock()
	defer runtimeInstalls.Unlock()
	j := s.sup.Journal()
	// Downloads outlive the request that started them.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Hour)
	defer cancel()

	total, last := plan.TotalBytes(), time.Time{}
	var vendorDone int64
	_, err := rtpkg.NewClient().Install(ctx, plan, s.runtimesRoot, hw, func(file string, done, size int64) {
		what := "engine"
		if plan.Vendor != nil && file == plan.Vendor.Name {
			what, vendorDone = "CUDA runtime", done
		}
		if time.Since(last) < time.Second && done != size {
			return
		}
		last = time.Now()
		sofar := done
		if what == "engine" {
			sofar += vendorDone
		}
		j.Report(opID, fmt.Sprintf("downloading %s: %s of %s", what, humanBytes(done), humanBytes(size)),
			float64(sofar)/float64(max(total, 1)))
	})
	if err != nil {
		s.log.Error("runtime install failed", "runtime", plan.RuntimeName(), "err", err)
		j.Fail(opID, err)
		return
	}
	j.Report(opID, "verified, probed on this machine, installed", 1)
	if s.reloadRuntimes != nil {
		_ = s.reloadRuntimes()
	}
	s.log.Info("runtime installed", "runtime", plan.RuntimeName())
	j.Succeed(opID, "")
}

// handleRuntimeRemove deletes a ModelFabric-installed runtime. The checks live
// here, in one place, for the CLI and the page alike.
func (s *Server) handleRuntimeRemove(w http.ResponseWriter, r *http.Request) {
	if !s.requireRuntimeInstall(w) {
		return
	}
	var req struct {
		Name string `json:"name"`
	}
	if err := decodeBody(w, r, &req, 1<<20); err != nil {
		writeError(w, http.StatusBadRequest, "parse request body: "+err.Error())
		return
	}
	d, ok := s.sup.Runtimes().Lookup(req.Name)
	switch {
	case !ok:
		writeError(w, http.StatusNotFound, fmt.Sprintf("no runtime named %q", req.Name))
		return
	case d.Provenance() == nil:
		writeError(w, http.StatusForbidden, fmt.Sprintf(
			"%s is not a ModelFabric-installed runtime (origin %s); manage it where it came from", req.Name, d.Origin))
		return
	}
	for _, inst := range s.sup.Instances() {
		if inst.Runtime == req.Name {
			writeError(w, http.StatusConflict, fmt.Sprintf("%s is running %s; unload first", req.Name, inst.ID))
			return
		}
	}
	if err := rtpkg.Remove(s.runtimesRoot, d.PackageDir()); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.log.Info("runtime removed", "runtime", req.Name)
	s.handleRuntimeRescan(w, r)
}

func (s *Server) requireRuntimeInstall(w http.ResponseWriter) bool {
	if !s.requireSupervisor(w) {
		return false
	}
	if s.runtimesRoot == "" || s.sup.Runtimes() == nil {
		writeError(w, http.StatusNotImplemented, "this node does not manage runtimes")
		return false
	}
	return true
}

func humanBytes(n int64) string {
	units := []string{"B", "KB", "MB", "GB", "TB"}
	v, i := float64(n), 0
	for v >= 1024 && i < len(units)-1 {
		v /= 1024
		i++
	}
	if i == 0 {
		return fmt.Sprintf("%dB", n)
	}
	return fmt.Sprintf("%.1f%s", v, units[i])
}
