package server

import (
	"errors"
	"net/http"
	"os"
	"os/user"
	"time"

	"github.com/gregbugaj/modelfabric/internal/nodekey"
)

// The dashboard's token manager: list, create, revoke. Management routes, so
// loopback-only, or a device Tailscale reports as the same owner (peer.go) —
// the same people who can already read the node key from its disk.

// tokenView is a token as the API shows it: never the secret, which is not
// kept, and not the hash, which is nobody's business but the store's.
type tokenView struct {
	ID       string     `json:"id"`
	Name     string     `json:"name"`
	Hint     string     `json:"hint"`
	Created  time.Time  `json:"created"`
	LastUsed *time.Time `json:"last_used,omitempty"`
}

func viewOf(t nodekey.Token) tokenView {
	v := tokenView{ID: t.ID, Name: t.Name, Hint: t.Hint, Created: t.Created}
	if !t.LastUsed.IsZero() {
		lu := t.LastUsed
		v.LastUsed = &lu
	}
	return v
}

type tokensList struct {
	// NodeKey is the node key's last four characters. It is listed so the
	// page shows every credential the node accepts. It is rotated rather than
	// revoked (there is always exactly one), and rotating cuts off every
	// client holding it at once, so the page asks first.
	NodeKey string `json:"node_key,omitempty"`
	// Rotatable is whether this node can rotate its key from here.
	Rotatable bool        `json:"rotatable,omitempty"`
	Tokens    []tokenView `json:"tokens"`
}

func (s *Server) tokenStore(w http.ResponseWriter) bool {
	if s.keys == nil {
		writeError(w, http.StatusNotFound, "this node keeps no named tokens")
		return false
	}
	return true
}

func (s *Server) handleTokens(w http.ResponseWriter, _ *http.Request) {
	if !s.tokenStore(w) {
		return
	}
	ts, err := s.keys.List()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := tokensList{Tokens: make([]tokenView, 0, len(ts))}
	for _, t := range ts {
		out.Tokens = append(out.Tokens, viewOf(t))
	}
	if s.apiKey != nil {
		if k, err := s.apiKey(); err == nil && len(k) >= 4 {
			out.NodeKey = k[len(k)-4:]
		}
	}
	out.Rotatable = s.keyHome != ""
	writeJSON(w, http.StatusOK, out)
}

// handleTokenCreate answers with the secret, once. Nothing can show it again.
func (s *Server) handleTokenCreate(w http.ResponseWriter, r *http.Request) {
	if !s.tokenStore(w) {
		return
	}
	var body struct {
		Name string `json:"name"`
	}
	if err := decodeBody(w, r, &body, 4096); err != nil {
		writeError(w, http.StatusBadRequest, "send {\"name\": \"...\"}")
		return
	}
	secret, t, err := s.keys.Create(body.Name)
	if err != nil {
		writeError(w, tokenErrStatus(err), err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, struct {
		tokenView
		Token string `json:"token"`
	}{viewOf(t), secret})
}

func (s *Server) handleTokenRevoke(w http.ResponseWriter, r *http.Request) {
	if !s.tokenStore(w) {
		return
	}
	var body struct {
		ID string `json:"id"`
	}
	if err := decodeBody(w, r, &body, 4096); err != nil || body.ID == "" {
		writeError(w, http.StatusBadRequest, "send {\"id\": \"...\"}")
		return
	}
	t, err := s.keys.Revoke(body.ID)
	if err != nil {
		writeError(w, tokenErrStatus(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"revoked": viewOf(t)})
}

func tokenErrStatus(err error) int {
	switch {
	case errors.Is(err, nodekey.ErrBadName):
		return http.StatusBadRequest
	case errors.Is(err, nodekey.ErrNoToken):
		return http.StatusNotFound
	}
	return http.StatusInternalServerError
}

// keyInfo is where this node keeps its key and who it runs as: what `mfsh
// key` checks before reading a key file. Run as another user, it read that
// user's home, found no key, and created one the node never used: printed as
// if it were the node's key, it was refused by the node it was meant for.
type keyInfo struct {
	Home string `json:"home"`
	File string `json:"file"`
	User string `json:"user,omitempty"`
}

func (s *Server) handleKeyInfo(w http.ResponseWriter, _ *http.Request) {
	if s.keyHome == "" {
		writeError(w, http.StatusNotFound, "this node does not say where its key is kept")
		return
	}
	info := keyInfo{Home: s.keyHome, File: nodekey.Path(s.keyHome)}
	if u, err := user.Current(); err == nil {
		info.User = u.Username
	} else {
		info.User = os.Getenv("USER")
	}
	writeJSON(w, http.StatusOK, info)
}

// handleKeyRotate replaces the node key and answers with the new one, once,
// as a created token is answered. Named tokens are not touched.
func (s *Server) handleKeyRotate(w http.ResponseWriter, _ *http.Request) {
	if s.keyHome == "" {
		writeError(w, http.StatusNotFound, "this node cannot rotate its key from here; run `mfsh key rotate` on it")
		return
	}
	key, err := nodekey.Rotate(s.keyHome)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.log.Warn("node key rotated; clients holding the old key are refused", "file", nodekey.Path(s.keyHome))
	writeJSON(w, http.StatusOK, map[string]string{"key": key, "hint": key[len(key)-4:]})
}
