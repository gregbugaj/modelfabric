package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gregbugaj/modelfabric/internal/nodekey"
)

// `mfsh key` prints this node's API key: what an app sends as
// "Authorization: Bearer <key>", or Anthropic's x-api-key.
//
// A node behind a TLS proxy or Tailscale Funnel needs one. A key kept in the
// older gateway/master.key location is still read, so keys already handed out
// keep working (internal/nodekey).

// `mfsh key create|ls|rm` manage named tokens: credentials besides the node
// key, each revocable alone (internal/nodekey/tokens.go). Like the key they
// are a file on this node, so they are edited on disk; a running node picks
// up the change on its next request.
//
// `mfsh key rotate` replaces the node key, cutting off every client that holds
// it; named tokens keep working.
func keyCmd(args []string) error {
	if len(args) > 0 {
		switch args[0] {
		case "create", "new", "ls", "list", "rm", "revoke", "rotate":
			if err := sameKeyHome(defaultAddr, fabricHome()); err != nil {
				return err
			}
		}
		switch args[0] {
		case "create", "new":
			return keyCreate(args[1:])
		case "ls", "list":
			return keyList()
		case "rm", "revoke":
			return keyRevoke(args[1:])
		case "rotate":
			return keyRotate()
		}
	}
	fs := flag.NewFlagSet("key", flag.ExitOnError)
	addr := fs.String("addr", defaultAddr, "address of the local ModelFabric node")
	if err := fs.Parse(args); err != nil {
		return err
	}
	// A key is a file on the node that owns it, and is deliberately not served
	// over the network. Asked about a peer, say where to run it rather than
	// answer with this machine's key.
	if err := mustBeLocal(*addr, "a node's API key", "key"); err != nil {
		return err
	}
	if err := sameKeyHome(*addr, fabricHome()); err != nil {
		return err
	}
	key, err := nodekey.Key(fabricHome())
	if err != nil {
		return err
	}
	fmt.Println(key)
	return nil
}

// sameKeyHome refuses to read or write keys in a directory the running node
// does not use. `mfsh key`, run as root on a node running as another user,
// read root's home, found no key, created one and printed it: a key the node
// had never seen, which it refused, and which looked exactly like the real
// one. A node that is not running, or too old to say, leaves this directory as
// the only one there is.
func sameKeyHome(addr, mine string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(addr, "/")+"/api/v1/key", nil)
	if err != nil {
		return nil
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	var info struct {
		Home string `json:"home"`
		User string `json:"user"`
	}
	if resp.StatusCode != http.StatusOK || json.NewDecoder(resp.Body).Decode(&info) != nil || info.Home == "" {
		return nil
	}
	if filepath.Clean(info.Home) == filepath.Clean(mine) {
		return nil
	}
	who := or(info.User, "its own user")
	return fmt.Errorf("the node on %s keeps its keys in %s, not %s: run this as %s, for example `sudo -u %s mfsh key`",
		addr, info.Home, mine, who, or(info.User, "<user>"))
}

func keyRotate() error {
	key, err := nodekey.Rotate(fabricHome())
	if err != nil {
		return err
	}
	// The key alone on stdout, as `mfsh key` prints it, so it can be captured.
	fmt.Println(key)
	fmt.Fprintf(os.Stderr, "%s node key rotated. Clients holding the old key are refused from now on; named tokens still work.\n", green("✓"))
	return nil
}

// mustBeLocal refuses a command that only makes sense against the node it is
// run on. `why` completes "<what> <why>": a key is read from that node's disk,
// a live stream is served only on its own loopback listener. Both fail the
// same way over the network — quietly answering about the wrong node — so
// both say which node to run on instead.
//
// `mfsh key -addr http://<peer>:1234` would otherwise take the flag and return
// *this* node's key: every request to the peer then answers 401, long after the
// models have loaded and everything looks ready.
func mustBeLocal(addr, what, cmd string) error {
	return mustBeLocalBecause(addr, what, "is read from that node's own disk, not over the network", cmd)
}

func mustBeLocalBecause(addr, what, why, cmd string) error {
	u, err := url.Parse(addr)
	if err != nil || u.Host == "" {
		return nil // not something this can judge; let the caller proceed
	}
	host := u.Hostname()
	switch host {
	case "", "127.0.0.1", "localhost", "::1":
		return nil
	}
	return fmt.Errorf("%s %s: run it on %s, for example `ssh %s mfsh %s`",
		what, why, host, host, cmd)
}

func keyCreate(args []string) error {
	name := strings.TrimSpace(strings.Join(args, " "))
	if name == "" {
		return fmt.Errorf("name the token after what will use it: mfsh key create <name>")
	}
	secret, t, err := nodekey.Tokens(fabricHome()).Create(name)
	if err != nil {
		return err
	}
	// The secret alone on stdout, so `mfsh key create ci > ci.key` captures
	// exactly the token; the warning goes to stderr.
	fmt.Println(secret)
	fmt.Fprintf(os.Stderr, "%s token %q created. It is not stored and cannot be shown again; revoke it with `mfsh key rm %s`.\n",
		green("✓"), t.Name, t.Name)
	return nil
}

func keyList() error {
	ts, err := nodekey.Tokens(fabricHome()).List()
	if err != nil {
		return err
	}
	tb := newTable("NAME", "TOKEN", "CREATED", "LAST USED")
	if k, err := nodekey.Key(fabricHome()); err == nil && len(k) >= 4 {
		tb.add("node key", "sk-mfsh-…"+k[len(k)-4:], "", dim("`mfsh key` prints it"))
	}
	for _, t := range ts {
		used := dim("never")
		if !t.LastUsed.IsZero() {
			used = since(t.LastUsed)
		}
		tb.add(t.Name, "sk-mfsh-…"+t.Hint, t.Created.Local().Format("2006-01-02 15:04"), used)
	}
	fmt.Print(tb.String())
	if len(ts) == 0 {
		fmt.Println(dim("\nNo named tokens. `mfsh key create <name>` makes one for each app, so you can revoke them one at a time."))
	}
	return nil
}

func keyRevoke(args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("which token? mfsh key rm <name>  (mfsh key ls lists them)")
	}
	t, err := nodekey.Tokens(fabricHome()).Revoke(args[0])
	if err != nil {
		return err
	}
	fmt.Printf("revoked %q; requests with it are refused from now on\n", t.Name)
	return nil
}

// since is a coarse age, for "last used": the minute is the store's precision.
func since(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
	return fmt.Sprintf("%dd ago", int(d.Hours()/24))
}
