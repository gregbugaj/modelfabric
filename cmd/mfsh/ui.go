package main

import (
	"embed"
	"io/fs"
	"log/slog"
	"net/http"
	"strings"
)

// The built UI is compiled into the binary so a node stays a single file to
// deploy. `make ui` populates web/dist; the checked-in placeholder keeps the
// build working before anyone has run it.
//
//go:embed all:web/dist
var uiFS embed.FS

func isAPIPath(p string) bool {
	for _, mount := range []string{"/api", "/z"} {
		if p == mount || strings.HasPrefix(p, mount+"/") {
			return true
		}
	}
	return false
}

func uiHandler(log *slog.Logger) http.Handler {
	sub, err := fs.Sub(uiFS, "web/dist")
	if err != nil {
		log.Warn("no embedded UI", "err", err)
		return nil
	}
	files := http.FileServer(http.FS(sub))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Exclude API paths, including bare /api and /z, from the SPA fallback to prevent HTML responses to JSON clients.
		if isAPIPath(r.URL.Path) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":{"message":"no such endpoint","type":"modelfabric_error","code":404}}` + "\n"))
			return
		}
		p := strings.TrimPrefix(r.URL.Path, "/")
		if p == "" {
			p = "index.html"
		}
		// Anything that is not a real asset falls through to index.html so
		// client-side routes survive a page reload.
		served := p
		if _, err := fs.Stat(sub, p); err != nil {
			r = r.Clone(r.Context())
			r.URL.Path = "/"
			// Use index.html's cache policy for fallbacks; a missing asset URL must not cache the SPA shell for a year.
			served = "index.html"
		}
		if strings.HasPrefix(served, "assets/") {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			w.Header().Set("Cache-Control", "no-cache")
		}
		files.ServeHTTP(w, r)
	})
}
