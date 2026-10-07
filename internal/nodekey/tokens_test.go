package nodekey

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestTokenLifecycle(t *testing.T) {
	tests := []struct {
		name string
		run  func(t *testing.T, s *Store)
	}{
		{name: "a created token matches, and its secret is not stored", run: func(t *testing.T, s *Store) {
			secret, tok, err := s.Create("laptop")
			if err != nil {
				t.Fatal(err)
			}
			if !strings.HasPrefix(secret, "sk-mfsh-") || tok.Hint != secret[len(secret)-4:] {
				t.Fatalf("secret %q, hint %q", secret, tok.Hint)
			}
			if got, ok := s.Match(secret); !ok || got.Name != "laptop" {
				t.Fatalf("Match = %v, %v", got, ok)
			}
			b, _ := os.ReadFile(filepath.Join(s.dir, tokensFile))
			if strings.Contains(string(b), secret) {
				t.Fatal("the secret was written to disk")
			}
		}},
		{name: "a wrong or empty secret matches nothing", run: func(t *testing.T, s *Store) {
			secret, _, _ := s.Create("laptop")
			for _, given := range []string{"", secret + "x", "sk-mfsh-" + strings.Repeat("0", 48)} {
				if _, ok := s.Match(given); ok {
					t.Fatalf("%q matched", given)
				}
			}
		}},
		{name: "revoking one leaves the others working", run: func(t *testing.T, s *Store) {
			a, _, _ := s.Create("a")
			b, _, _ := s.Create("b")
			if _, err := s.Revoke("a"); err != nil {
				t.Fatal(err)
			}
			if _, ok := s.Match(a); ok {
				t.Fatal("revoked token still matches")
			}
			if _, ok := s.Match(b); !ok {
				t.Fatal("the other token stopped matching")
			}
		}},
		{name: "revoke takes an id as well as a name", run: func(t *testing.T, s *Store) {
			_, tok, _ := s.Create("ci")
			if gone, err := s.Revoke(tok.ID); err != nil || gone.Name != "ci" {
				t.Fatalf("Revoke(id) = %v, %v", gone, err)
			}
		}},
		{name: "revoking an unknown token says so", run: func(t *testing.T, s *Store) {
			if _, err := s.Revoke("nope"); err == nil || !strings.Contains(err.Error(), `"nope"`) {
				t.Fatalf("err = %v", err)
			}
		}},
		{name: "names are unique ignoring case", run: func(t *testing.T, s *Store) {
			s.Create("Laptop")
			_, _, err := s.Create("laptop")
			if !errors.Is(err, ErrBadName) {
				t.Fatalf("err = %v, want ErrBadName", err)
			}
			if !strings.HasPrefix(err.Error(), "a token named") {
				t.Fatalf("message = %q", err)
			}
		}},
		{name: "a blank name is refused", run: func(t *testing.T, s *Store) {
			if _, _, err := s.Create("   "); err == nil {
				t.Fatal("a blank name was accepted")
			}
		}},
		{name: "the file is readable only by its owner", run: func(t *testing.T, s *Store) {
			s.Create("laptop")
			fi, err := os.Stat(filepath.Join(s.dir, tokensFile))
			if err != nil {
				t.Fatal(err)
			}
			if perm := fi.Mode().Perm(); perm != 0o600 {
				t.Fatalf("mode = %o, want 600", perm)
			}
		}},
		{name: "use is recorded as last used", run: func(t *testing.T, s *Store) {
			secret, _, _ := s.Create("laptop")
			s.Match(secret)
			ts, _ := s.List()
			if ts[0].LastUsed.IsZero() {
				t.Fatal("last used not recorded")
			}
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tc.run(t, Tokens(t.TempDir()))
		})
	}
}

// `mfsh key create` and `mfsh key rm` edit the file while the node runs; the
// node has to see both without a restart, or a revoke would not revoke.
func TestTokensChangedByAnotherProcess(t *testing.T) {
	dir := t.TempDir()
	node, cli := Tokens(dir), Tokens(dir)

	secret, _, err := cli.Create("laptop")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := node.Match(secret); !ok {
		t.Fatal("the node did not see a token created by another process")
	}
	if _, err := cli.Revoke("laptop"); err != nil {
		t.Fatal(err)
	}
	if _, ok := node.Match(secret); ok {
		t.Fatal("the node still honours a token another process revoked")
	}
}

// The node writes last-used times back; doing so from a stale copy erased a
// token created at the command line in the meantime.
func TestRecordingUseKeepsTokensCreatedElsewhere(t *testing.T) {
	dir := t.TempDir()
	node, cli := Tokens(dir), Tokens(dir)
	a, _, _ := cli.Create("a")
	node.Match(a) // node now holds a copy with only "a"
	b, _, _ := cli.Create("b")

	node.mu.Lock()
	node.used["force"] = time.Now() // anything pending makes the flush write
	err := node.flushLocked()
	node.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := Tokens(dir).Match(b); !ok {
		t.Fatal("writing last-used times erased a token created by another process")
	}
}

// Read as empty, a damaged file would be overwritten by the next create and
// every token already handed out would be gone.
func TestDamagedTokensFileFailsClosed(t *testing.T) {
	dir := t.TempDir()
	s := Tokens(dir)
	secret, _, _ := s.Create("laptop")
	if err := os.WriteFile(filepath.Join(dir, tokensFile), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Match(secret); ok {
		t.Fatal("a damaged file still authorized a request")
	}
	if _, _, err := s.Create("other"); err == nil {
		t.Fatal("create wrote over a damaged file")
	}
	if b, _ := os.ReadFile(filepath.Join(dir, tokensFile)); string(b) != "{not json" {
		t.Fatalf("damaged file was rewritten: %q", b)
	}
}

// The node writes last-used times at most once a minute, and `mfsh key ls`
// reads the file: a token first used inside that minute read "never".
func TestFirstUseReachesTheFileAtOnce(t *testing.T) {
	dir := t.TempDir()
	node, cli := Tokens(dir), Tokens(dir)
	a, _, _ := cli.Create("a")
	b, _, _ := cli.Create("b")
	node.Match(a)
	node.Match(b) // within the same minute as a's flush
	ts, err := Tokens(dir).List()
	if err != nil {
		t.Fatal(err)
	}
	for _, tok := range ts {
		if tok.LastUsed.IsZero() {
			t.Fatalf("%s was used but the file says never", tok.Name)
		}
	}
}
