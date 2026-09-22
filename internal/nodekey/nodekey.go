// Package nodekey holds this node's API key: the credential apps present to
// ModelFabric's own front door.
//
// A node exposed through a TLS proxy or Tailscale Funnel needs one.
//
// An older build kept the key in gateway/master.key, and that location is
// still read first, on purpose. That key is deployed: it
// sits in clients' configuration, in benchmark environments, behind reverse
// proxies. Generating a fresh one because a file moved would revoke every
// credential already issued and look, from the outside, exactly like ModelFabric
// breaking.
package nodekey

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	// file is where a key created from now on is written.
	file = "api.key"
	// legacyFile is where older builds kept it. Read, never written.
	legacyFile = "gateway/master.key"
)

// Path is where this node's key lives: the legacy file while it exists, else
// the current one. Reported by doctor, which checks its permissions.
func Path(home string) string {
	if legacy := filepath.Join(home, legacyFile); exists(legacy) {
		return legacy
	}
	return filepath.Join(home, file)
}

// Key returns this node's API key, creating it on first use. It is readable
// only by its owner: it authorizes every request to this node.
func Key(home string) (string, error) {
	// The older file, if this node ever had one. Still authoritative: it is
	// what existing clients send.
	if k, err := read(filepath.Join(home, legacyFile)); err != nil {
		return "", err
	} else if k != "" {
		return k, nil
	}

	p := filepath.Join(home, file)
	if k, err := read(p); err != nil {
		return "", err
	} else if k != "" {
		return k, nil
	}
	if err := os.MkdirAll(home, 0o755); err != nil {
		return "", err
	}
	key, err := newKey()
	if err != nil {
		return "", err
	}
	// O_EXCL so two processes starting at once cannot each write a different
	// key and hand out one that is not the one on disk: the loser reads the
	// winner's key instead of overwriting it.
	f, err := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		if !os.IsExist(err) {
			return "", err
		}
		k, rerr := read(p)
		if rerr != nil {
			return "", rerr
		}
		if k == "" {
			return "", fmt.Errorf("key file %s is empty", p)
		}
		return k, nil
	}
	if _, err := f.WriteString(key + "\n"); err != nil {
		f.Close()
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	return key, nil
}

// The "sk-" shape is what OpenAI-compatible clients expect of a key, and
// plenty of them validate it; "mfsh" says whose it is in a log.
func newKey() (string, error) {
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return "sk-mfsh-" + hex.EncodeToString(raw), nil
}

// Rotate replaces this node's key with a new one and returns it. Every client
// holding the old key is cut off at once; named tokens are not touched.
//
// The new key is written beside the file and renamed over it, so a request
// arriving meanwhile reads the old key or the new one, never an empty file
// (which Key would answer by generating a third). The legacy file goes after:
// Key prefers it, so left in place it would keep the old key working.
func Rotate(home string) (string, error) {
	if err := os.MkdirAll(home, 0o755); err != nil {
		return "", err
	}
	key, err := newKey()
	if err != nil {
		return "", err
	}
	p := filepath.Join(home, file)
	tmp, err := os.CreateTemp(home, ".api.key-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return "", err
	}
	if _, err := tmp.WriteString(key + "\n"); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if err := os.Rename(tmp.Name(), p); err != nil {
		return "", err
	}
	if err := os.Remove(filepath.Join(home, legacyFile)); err != nil && !os.IsNotExist(err) {
		return "", fmt.Errorf("the new key is in %s, but the old one in %s still works until it is removed: %w", p, legacyFile, err)
	}
	return key, nil
}

// read returns the key in a file, or "" when the file is absent. An unreadable
// file is an error rather than an absent one: treating it as missing generated a
// new key over the top of the old one, silently revoking every credential
// already issued from it.
func read(p string) (string, error) {
	b, err := os.ReadFile(p)
	switch {
	case err == nil:
		return strings.TrimSpace(string(b)), nil
	case os.IsNotExist(err):
		return "", nil
	default:
		return "", fmt.Errorf("read node key %s: %w", p, err)
	}
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
