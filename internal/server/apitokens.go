package server

import (
	"errors"
	"net/http"
	"os"
	"os/user"
	"time"

	"github.com/gregbugaj/modelfabric/internal/nodekey"
)

// Token management requires loopback or a Tailscale device with the same owner.

// tokenView excludes both the secret and its stored hash.
type tokenView struct {
	ID       string     `json:"id"`
	Name     string     `json:"name"`
	Hint     string     `json:"hint"`
	Prefix   string     `json:"prefix,omitempty"`
	Created  time.Time  `json:"created"`
	LastUsed *time.Time `json:"last_used,omitempty"`
}

func viewOf(t nodekey.Token) tokenView {
	v := tokenView{ID: t.ID, Name: t.Name, Hint: t.Hint, Prefix: t.Prefix, Created: t.Created}
	if !t.LastUsed.IsZero() {
		lu := t.LastUsed
		v.LastUsed = &lu
	}
	return v
}

type tokensList struct {
	// NodeKey is the node key's last four characters. Rotating the key revokes
	// all clients using it; the dashboard requests confirmation.
	NodeKey string `json:"node_key,omitempty"`
	// NodeKeyPrefix is how the node key begins (see nodekey.PrefixOf), so the
	// masked form shown for it is the key's own and not a guess.
	NodeKeyPrefix string      `json:"node_key_prefix,omitempty"`
	Rotatable     bool        `json:"rotatable,omitempty"`
	Tokens        []tokenView `json:"tokens"`
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
			out.NodeKeyPrefix = nodekey.PrefixOf(k)
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

// keyInfo identifies the running node's key directory and user. The CLI must
// not create an unrelated key in its own user's home.
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

// handleKeyReveal answers with the node key, for the dashboard's Reveal
// button. It is what `mfsh key` prints, to the same person: whoever can reach
// this node's own management address.
//
// It is refused over the mesh (see PeerHandler). A node's key is read from its
// own disk and never served to another machine: one node's key in another
// node's browser is how a key meant for one machine ends up used from two.
// Named tokens have no such route because their secrets are not kept, only a
// hash to check them against.
func (s *Server) handleKeyReveal(w http.ResponseWriter, _ *http.Request) {
	if s.keyHome == "" {
		writeError(w, http.StatusNotFound, "this node cannot show its key from here; run `mfsh key` on it")
		return
	}
	key, err := nodekey.Key(s.keyHome)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.log.Warn("node key shown in the dashboard")
	// The answer is a secret: nothing on the way may keep a copy.
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]string{"key": key})
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
