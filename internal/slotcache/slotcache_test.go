package slotcache

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// fakeEngine stands in for llama-server's slot endpoints: /slots, and the save
// and restore actions writing into the same directory the store reads.
type fakeEngine struct {
	mu      sync.Mutex
	dir     string
	tasks   []int
	calls   []string // "save 0 file", "restore 1 file"
	refuse  int      // status for save, when non-zero
	size    int
	srv     *httptest.Server
	nextTID int
}

func newFakeEngine(t *testing.T, dir string, slots int) *fakeEngine {
	t.Helper()
	f := &fakeEngine{dir: dir, tasks: make([]int, slots), size: 1000, nextTID: 1}
	for i := range f.tasks {
		f.tasks[i] = -1
	}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if r.URL.Path == "/slots" {
			type s struct {
				ID   int `json:"id"`
				Task int `json:"id_task"`
			}
			var out []s
			for i, t := range f.tasks {
				out = append(out, s{i, t})
			}
			_ = json.NewEncoder(w).Encode(out)
			return
		}
		id := strings.TrimPrefix(r.URL.Path, "/slots/")
		var body struct {
			Filename string `json:"filename"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		action := r.URL.Query().Get("action")
		f.calls = append(f.calls, action+" "+id+" "+body.Filename)
		switch action {
		case "save":
			if f.refuse != 0 {
				w.WriteHeader(f.refuse)
				return
			}
			_ = os.WriteFile(filepath.Join(f.dir, body.Filename), make([]byte, f.size), 0o644)
			_, _ = io.WriteString(w, `{"n_saved":123}`)
		case "restore":
			if _, err := os.Stat(filepath.Join(f.dir, body.Filename)); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			_, _ = io.WriteString(w, `{"n_restored":123}`)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// ran is a request finishing on a slot: the engine gives it a new task id.
func (f *fakeEngine) ran(slot int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tasks[slot] = f.nextTID
	f.nextTID++
}

func (f *fakeEngine) took() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := f.calls
	f.calls = nil
	return out
}

// chainOf builds a chain of n blocks for a conversation, the way the router
// does: each block's hash covers everything before it.
func chainOf(name string, n int) []Block {
	out := make([]Block, n)
	prev := sha256.Sum256([]byte(name))
	for i := range out {
		prev = sha256.Sum256(prev[:])
		out[i] = prev
	}
	return out
}

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func serve(t *testing.T, s *Store, f *fakeEngine, chain []Block, status int) int {
	t.Helper()
	slot, done := s.Place(context.Background(), "e", chain)
	if slot < 0 {
		t.Fatalf("request was not placed")
	}
	f.ran(slot)
	done(status)
	return slot
}

func open(t *testing.T, dir string, max int64) *Store {
	t.Helper()
	s, err := Open(dir, max, http.DefaultClient, quiet())
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func kinds(calls []string) string {
	var out []string
	for _, c := range calls {
		parts := strings.Fields(c)
		out = append(out, parts[0]+" "+parts[1])
	}
	return strings.Join(out, ", ")
}

func TestEvictedConversationIsSavedThenRestored(t *testing.T) {
	dir := t.TempDir()
	f := newFakeEngine(t, dir, 1)
	s := open(t, dir, 0)
	s.Attach("e", f.srv.URL, "sig", 1)

	a, b := chainOf("a", 40), chainOf("b", 40)
	serve(t, s, f, a, 200)
	if got := kinds(f.took()); got != "" {
		t.Fatalf("first request on an empty slot touched the disk: %s", got)
	}
	serve(t, s, f, b, 200)
	if got := kinds(f.took()); got != "save 0" {
		t.Fatalf("taking a's slot: got %q, want a save of a", got)
	}
	serve(t, s, f, a, 200)
	if got := kinds(f.took()); got != "save 0, restore 0" {
		t.Fatalf("a returning: got %q, want b saved then a restored", got)
	}
	if st := s.Stats(); st.Restores != 1 || st.Snapshots != 2 {
		t.Fatalf("stats: %+v", st)
	}
}

func TestPlacement(t *testing.T) {
	cases := []struct {
		name string
		run  func(t *testing.T, s *Store, f *fakeEngine)
	}{
		{"the next turn of a conversation stays in its slot and writes nothing", func(t *testing.T, s *Store, f *fakeEngine) {
			first := serve(t, s, f, chainOf("a", 40), 200)
			again := serve(t, s, f, chainOf("a", 60), 200)
			if first != again {
				t.Fatalf("turn two went to slot %d, turn one was in %d", again, first)
			}
			if got := kinds(f.took()); got != "" {
				t.Fatalf("a growing conversation was written to disk: %s", got)
			}
		}},
		{"a new conversation takes the empty slot, not the occupied one", func(t *testing.T, s *Store, f *fakeEngine) {
			a := serve(t, s, f, chainOf("a", 40), 200)
			b := serve(t, s, f, chainOf("b", 40), 200)
			if a == b {
				t.Fatalf("both conversations were put in slot %d with another free", a)
			}
			if got := kinds(f.took()); got != "" {
				t.Fatalf("nothing was lost, yet: %s", got)
			}
		}},
		{"a short conversation is not worth a file", func(t *testing.T, s *Store, f *fakeEngine) {
			serve(t, s, f, chainOf("a", 4), 200)
			serve(t, s, f, chainOf("b", 4), 200)
			serve(t, s, f, chainOf("c", 4), 200)
			if got := kinds(f.took()); got != "" {
				t.Fatalf("saved a prompt shorter than the engine can re-read: %s", got)
			}
		}},
		{"a failed request leaves the slot unknown and unsaved", func(t *testing.T, s *Store, f *fakeEngine) {
			// 0 is a response that never finished. The slot holds part of a
			// prompt; saving it under a's name would restore the wrong thing.
			serve(t, s, f, chainOf("a", 40), 0)
			serve(t, s, f, chainOf("b", 40), 200)
			serve(t, s, f, chainOf("c", 40), 200)
			for _, c := range f.took() {
				if strings.HasPrefix(c, "save") && strings.Contains(c, fileFor("sig", chainOf("a", 40))) {
					t.Fatalf("saved the slot of a request that failed: %s", c)
				}
			}
		}},
		{"a request the engine refused does not change what the slot holds", func(t *testing.T, s *Store, f *fakeEngine) {
			slot := serve(t, s, f, chainOf("a", 40), 200)
			// Too long for the context: llama-server answers 400 without
			// touching the slot, and does not run a task.
			got, done := s.Place(context.Background(), "e", chainOf("a", 60))
			done(400)
			if got != slot {
				t.Fatalf("placed in %d, conversation is in %d", got, slot)
			}
			if next := serve(t, s, f, chainOf("a", 50), 200); next != slot {
				t.Fatalf("after a refusal the conversation was no longer found in slot %d", slot)
			}
		}},
		{"a slot something else used is forgotten rather than saved under the old name", func(t *testing.T, s *Store, f *fakeEngine) {
			slot := serve(t, s, f, chainOf("a", 40), 200)
			f.ran(slot) // a client dialling the engine directly
			serve(t, s, f, chainOf("b", 40), 200)
			serve(t, s, f, chainOf("c", 40), 200)
			serve(t, s, f, chainOf("d", 40), 200)
			for _, c := range f.took() {
				if strings.Contains(c, fileFor("sig", chainOf("a", 40))) {
					t.Fatalf("saved a slot under a conversation it no longer held: %s", c)
				}
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			f := newFakeEngine(t, dir, 2)
			s := open(t, dir, 0)
			s.Attach("e", f.srv.URL, "sig", 2)
			tc.run(t, s, f)
		})
	}
}

func fileFor(sig string, chain []Block) string {
	tip := chain[len(chain)-1]
	return sig + "-" + hexOf(tip[:12])
}

func hexOf(b []byte) string {
	const digits = "0123456789abcdef"
	out := make([]byte, 0, len(b)*2)
	for _, c := range b {
		out = append(out, digits[c>>4], digits[c&15])
	}
	return string(out)
}

// What oMLX's startup scan does: a restart finds what was saved before it.
func TestSnapshotsSurviveARestart(t *testing.T) {
	dir := t.TempDir()
	f := newFakeEngine(t, dir, 1)
	s := open(t, dir, 0)
	s.Attach("e", f.srv.URL, "sig", 1)
	a := chainOf("a", 40)
	serve(t, s, f, a, 200)
	s.Detach(context.Background(), "e")
	if got := kinds(f.took()); got != "save 0" {
		t.Fatalf("unload: got %q, want the slot saved", got)
	}

	// A new node process and a new engine: nothing in memory carries over.
	f2 := newFakeEngine(t, dir, 1)
	s2 := open(t, dir, 0)
	s2.Attach("e", f2.srv.URL, "sig", 1)
	serve(t, s2, f2, chainOf("a", 60), 200)
	if got := kinds(f2.took()); got != "restore 0" {
		t.Fatalf("after restart: got %q, want a restored", got)
	}
}

// Two quantizations of one model have KV of the same shape, so a state from
// one restores into the other without error and answers from the wrong
// numbers. The signature is what stops that.
func TestSnapshotIsNotRestoredIntoDifferentWeights(t *testing.T) {
	dir := t.TempDir()
	f := newFakeEngine(t, dir, 1)
	s := open(t, dir, 0)
	s.Attach("e", f.srv.URL, "q4", 1)
	serve(t, s, f, chainOf("a", 40), 200)
	s.Detach(context.Background(), "e")
	f.took()

	s.Attach("e", f.srv.URL, "q8", 1)
	serve(t, s, f, chainOf("a", 60), 200)
	if got := kinds(f.took()); got != "" {
		t.Fatalf("restored another model's state: %s", got)
	}
}

func TestDiskCapEvictsLeastRecentlyUsed(t *testing.T) {
	dir := t.TempDir()
	f := newFakeEngine(t, dir, 1)
	s := open(t, dir, 2500) // room for two 1000-byte snapshots
	s.Attach("e", f.srv.URL, "sig", 1)
	for _, name := range []string{"a", "b", "c", "d"} {
		serve(t, s, f, chainOf(name, 40), 200)
	}
	// a, b and c have been saved in turn; only the newest two fit.
	if st := s.Stats(); st.Snapshots != 2 || st.Bytes > 2500 {
		t.Fatalf("cap not held: %+v", st)
	}
	if _, err := os.Stat(filepath.Join(dir, fileFor("sig", chainOf("a", 40))+".bin")); !os.IsNotExist(err) {
		t.Fatalf("the oldest snapshot is still on disk")
	}
	left, _ := filepath.Glob(filepath.Join(dir, "*"))
	if len(left) != 4 {
		t.Fatalf("want 2 states and 2 sidecars on disk, found %d files", len(left))
	}
}

// A longer save of the same conversation replaces the shorter one: the short
// one can never be the better match again, and it is gigabytes.
func TestLaterSaveReplacesEarlierOfSameConversation(t *testing.T) {
	dir := t.TempDir()
	f := newFakeEngine(t, dir, 1)
	s := open(t, dir, 0)
	s.Attach("e", f.srv.URL, "sig", 1)
	serve(t, s, f, chainOf("a", 40), 200)
	serve(t, s, f, chainOf("b", 40), 200) // saves a@40
	serve(t, s, f, chainOf("a", 80), 200) // saves b, restores a@40, grows to 80
	serve(t, s, f, chainOf("b", 40), 200) // saves a@80
	if _, err := os.Stat(filepath.Join(dir, fileFor("sig", chainOf("a", 40))+".bin")); !os.IsNotExist(err) {
		t.Fatalf("the 40-block save of a is still on disk beside the 80-block one")
	}
	if _, err := os.Stat(filepath.Join(dir, fileFor("sig", chainOf("a", 80))+".bin")); err != nil {
		t.Fatalf("the 80-block save of a is missing: %v", err)
	}
}

// llama-server answers 501 to slot actions once a projector is loaded. One
// refusal is enough to stop asking, and requests go on unplaced.
func TestEngineThatCannotSaveIsLeftAlone(t *testing.T) {
	dir := t.TempDir()
	f := newFakeEngine(t, dir, 1)
	f.refuse = http.StatusNotImplemented
	s := open(t, dir, 0)
	s.Attach("e", f.srv.URL, "sig", 1)
	serve(t, s, f, chainOf("a", 40), 200)
	serve(t, s, f, chainOf("b", 40), 200) // tries to save a, is refused
	if slot, _ := s.Place(context.Background(), "e", chainOf("c", 40)); slot != -1 {
		t.Fatalf("still placing on an engine that refused to save: slot %d", slot)
	}
}

func TestUntrackedEngineIsNotPlaced(t *testing.T) {
	s := open(t, t.TempDir(), 0)
	if slot, _ := s.Place(context.Background(), "nobody", chainOf("a", 40)); slot != -1 {
		t.Fatalf("placed on an engine never attached: slot %d", slot)
	}
}

// The first live run alternated two conversations on one slot and rewrote an
// identical 659 MB file on every switch: a restored slot counted as unsaved
// the moment a request ran in it. What is already on disk is not saved again.
func TestRestoredSlotIsNotSavedAgainUnchanged(t *testing.T) {
	dir := t.TempDir()
	f := newFakeEngine(t, dir, 1)
	s := open(t, dir, 0)
	s.Attach("e", f.srv.URL, "sig", 1)
	a, b := chainOf("a", 40), chainOf("b", 40)
	serve(t, s, f, a, 200)
	serve(t, s, f, b, 200) // saves a
	serve(t, s, f, a, 200) // saves b, restores a
	f.took()
	for i := 0; i < 3; i++ {
		serve(t, s, f, b, 200)
		serve(t, s, f, a, 200)
	}
	if got := kinds(f.took()); strings.Contains(got, "save") {
		t.Fatalf("slots already on disk were written again: %s", got)
	}
}
