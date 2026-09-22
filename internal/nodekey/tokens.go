package nodekey

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Named tokens, besides the node key.
//
// One key per node meant every app, script and borrowed laptop held the same
// secret, and revoking any of them revoked all of them. A token is a credential
// of its own: named, shown once, stored only as a hash, and revoked alone. It
// is accepted wherever the node key is accepted from an app — inference — and
// nowhere else: the node key also proves a request came from this node itself
// (the forwarding marker; see server.FrontHandler), and no token can.
//
// The file is shared between the node and `mfsh key`, which edits it on disk
// rather than over the API because a key is the node's own file (key_cmd.go).
// Writers take a lock and re-read before changing it; the node notices a
// change by its size and modification time, so a token created at the
// command line works on the next request without a restart.

const (
	tokensFile = "tokens.json"
	tokensLock = "tokens.lock"
	// lastUsedEvery bounds how often a request writes "last used" back: once
	// a minute is precise enough to answer "is anything still using this?",
	// and a disk write per request is not a price authentication should pay.
	lastUsedEvery = time.Minute
	maxNameLen    = 64
)

// ErrBadName and ErrNoToken mark mistakes in the request rather than failures
// of the store, so the API can answer 400 or 404 instead of 500. They are
// matched with errors.Is and never printed: a message reads as the sentence
// it was given, not "bad token name: a token named …".
var (
	ErrBadName = errors.New("bad token name")
	ErrNoToken = errors.New("no such token")
)

type requestErr struct {
	kind error
	msg  string
}

func (e *requestErr) Error() string { return e.msg }
func (e *requestErr) Unwrap() error { return e.kind }

func badName(format string, a ...any) error {
	return &requestErr{ErrBadName, fmt.Sprintf(format, a...)}
}

// Token is one named credential. Hash is the SHA-256 of the secret: the
// secret is random, so a plain hash is enough, and it is all that is kept.
type Token struct {
	ID       string    `json:"id"`
	Name     string    `json:"name"`
	Hash     string    `json:"hash"`
	Hint     string    `json:"hint"` // the secret's last four characters, to tell tokens apart
	Created  time.Time `json:"created"`
	LastUsed time.Time `json:"last_used,omitzero"`
}

// Store is the tokens of one ModelFabric home.
type Store struct {
	dir string

	mu      sync.Mutex
	tokens  []Token
	stamp   fileStamp // of the file tokens was read from
	used    map[string]time.Time
	flushed time.Time
}

// fileStamp identifies one version of the file. The inode is in it because
// every write is a rename onto a new file: size and time alone could match
// across a revoke and a create within one tick of a coarse filesystem clock,
// and the node would keep honouring the revoked token.
type fileStamp struct {
	mod   time.Time
	size  int64
	inode uint64
	ok    bool
}

// Tokens is the store for home.
func Tokens(home string) *Store {
	return &Store{dir: home, used: map[string]time.Time{}}
}

func (s *Store) path() string { return filepath.Join(s.dir, tokensFile) }

// List returns the tokens, oldest first, with last-used times as this process
// knows them.
func (s *Store) List() ([]Token, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.refresh(); err != nil {
		return nil, err
	}
	out := make([]Token, len(s.tokens))
	copy(out, s.tokens)
	for i := range out {
		if t, ok := s.used[out[i].ID]; ok && t.After(out[i].LastUsed) {
			out[i].LastUsed = t
		}
	}
	return out, nil
}

// Create makes a token called name and returns its secret, which is not kept
// and cannot be shown again.
func (s *Store) Create(name string) (string, Token, error) {
	name = strings.TrimSpace(name)
	switch {
	case name == "":
		return "", Token{}, badName("a token needs a name, so you can tell later what uses it")
	case len(name) > maxNameLen:
		return "", Token{}, badName("token name is %d characters; keep it to %d", len(name), maxNameLen)
	}
	secret, err := newSecret()
	if err != nil {
		return "", Token{}, err
	}
	id, err := randomHex(6)
	if err != nil {
		return "", Token{}, err
	}
	t := Token{ID: id, Name: name, Hash: hashOf(secret), Hint: secret[len(secret)-4:], Created: time.Now().UTC()}
	err = s.update(func(ts []Token) ([]Token, error) {
		for _, x := range ts {
			if strings.EqualFold(x.Name, name) {
				return nil, badName("a token named %q already exists; revoke it first or pick another name", x.Name)
			}
		}
		return append(ts, t), nil
	})
	if err != nil {
		return "", Token{}, err
	}
	return secret, t, nil
}

// Revoke removes the token whose ID or name is ref.
func (s *Store) Revoke(ref string) (Token, error) {
	var gone Token
	err := s.update(func(ts []Token) ([]Token, error) {
		for i, x := range ts {
			if x.ID == ref || x.Name == ref {
				gone = x
				return append(ts[:i:i], ts[i+1:]...), nil
			}
		}
		return nil, &requestErr{ErrNoToken, fmt.Sprintf("no token named %q; `mfsh key ls` lists them", ref)}
	})
	return gone, err
}

// Match reports the token whose secret is given. An unreadable store matches
// nothing: it must fail closed, as the node key does.
func (s *Store) Match(given string) (Token, bool) {
	if given == "" {
		return Token{}, false
	}
	h := []byte(hashOf(given))
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.refresh() != nil {
		return Token{}, false
	}
	for _, t := range s.tokens {
		if subtle.ConstantTimeCompare(h, []byte(t.Hash)) == 1 {
			now := time.Now().UTC()
			s.used[t.ID] = now
			// A first use is written at once, whatever the minute says: left
			// for the next flush, `mfsh key ls` read the file and called a
			// token in use "never used", which is the one answer that must
			// not be wrong when deciding what is safe to revoke.
			if now.Sub(s.flushed) >= lastUsedEvery || t.LastUsed.IsZero() {
				s.flushed = now
				// Best effort: a failure to record when a token was used must
				// not turn into a refused request.
				_ = s.flushLocked()
			}
			return t, true
		}
	}
	return Token{}, false
}

// refresh re-reads the file when it has changed since it was last read.
// Called with s.mu held.
func (s *Store) refresh() error {
	st, err := stampOf(s.path())
	if err != nil {
		return err
	}
	if st == s.stamp {
		return nil
	}
	ts, err := readTokens(s.path())
	if err != nil {
		return err
	}
	s.tokens, s.stamp = ts, st
	return nil
}

// update applies change to the file's current contents under the lock. The
// file is re-read first, so a token created by `mfsh key create` while the
// node ran is not lost when the node writes last-used times.
func (s *Store) update(change func([]Token) ([]Token, error)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.updateLocked(change)
}

func (s *Store) updateLocked(change func([]Token) ([]Token, error)) error {
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return err
	}
	unlock, err := lockFile(filepath.Join(s.dir, tokensLock))
	if err != nil {
		return err
	}
	defer unlock()
	ts, err := readTokens(s.path())
	if err != nil {
		return err
	}
	next, err := change(ts)
	if err != nil {
		return err
	}
	if err := writeTokens(s.path(), next); err != nil {
		return err
	}
	st, err := stampOf(s.path())
	if err != nil {
		return err
	}
	s.tokens, s.stamp = next, st
	return nil
}

// flushLocked writes the last-used times this process has seen. Called with
// s.mu held.
func (s *Store) flushLocked() error {
	if len(s.used) == 0 {
		return nil
	}
	return s.updateLocked(func(ts []Token) ([]Token, error) {
		for i := range ts {
			if t, ok := s.used[ts[i].ID]; ok && t.After(ts[i].LastUsed) {
				ts[i].LastUsed = t
			}
		}
		return ts, nil
	})
}

// readTokens reads the file; absent is no tokens. Unreadable or malformed is
// an error, not "no tokens": read as empty, the next write would erase every
// token already handed out.
func readTokens(p string) ([]Token, error) {
	b, err := os.ReadFile(p)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read tokens %s: %w", p, err)
	}
	var f struct {
		Tokens []Token `json:"tokens"`
	}
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("tokens file %s is not valid JSON (%v); fix or remove it", p, err)
	}
	return f.Tokens, nil
}

// writeTokens replaces the file whole, owner-only: the hashes are not
// secrets, but which apps hold a credential and when they last used it is
// nobody else's business.
func writeTokens(p string, ts []Token) error {
	if ts == nil {
		ts = []Token{}
	}
	b, err := json.MarshalIndent(struct {
		Tokens []Token `json:"tokens"`
	}{ts}, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(p), ".tokens-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), p)
}

func stampOf(p string) (fileStamp, error) {
	fi, err := os.Stat(p)
	if os.IsNotExist(err) {
		return fileStamp{}, nil
	}
	if err != nil {
		return fileStamp{}, fmt.Errorf("read tokens %s: %w", p, err)
	}
	st := fileStamp{mod: fi.ModTime(), size: fi.Size(), ok: true}
	if sys, ok := fi.Sys().(*syscall.Stat_t); ok {
		st.inode = uint64(sys.Ino)
	}
	return st, nil
}

func lockFile(p string) (func(), error) {
	f, err := os.OpenFile(p, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, fmt.Errorf("lock %s: %w", p, err)
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, nil
}

// newSecret has the node key's shape, so clients that validate "sk-" keys
// accept a token too.
func newSecret() (string, error) {
	h, err := randomHex(24)
	if err != nil {
		return "", err
	}
	return "sk-mfsh-" + h, nil
}

func randomHex(n int) (string, error) {
	raw := make([]byte, n)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw), nil
}

func hashOf(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}
