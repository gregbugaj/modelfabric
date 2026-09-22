// Package slotcache is the cold tier of the prompt cache: KV state that would
// otherwise be thrown away is written to disk, and read back when the
// conversation it belongs to returns.
//
// An engine keeps a conversation's KV cache in a slot, and has only a few. When
// a different conversation takes the slot the state is gone, and the next turn
// of the first one re-reads its whole prompt. Measured on Qwen3-0.6B on the CPU
// with a 6k-token prompt: 24.4 s to re-read it, 0.4 s when the slot was
// restored from disk first (the save took 0.21 s and the restore 0.12 s, for a
// 690 MB file). The saving grows with the prompt, and a coding agent's is the
// largest there is.
//
// The idea is oMLX's (github.com/jundot/omlx): a hot tier in memory and a cold
// tier on SSD, keyed by a chain of hashes over the prompt so that a returning
// conversation is recognised by its prefix. oMLX runs the model in-process and
// can slice the KV tensors into blocks, so it writes only the blocks that are
// new. ModelFabric supervises llama-server from outside and cannot: the unit
// here is a whole slot, through llama-server's own save and restore actions.
// Two things follow from that, and both are deliberate:
//
//   - A slot is saved when it is about to be lost (another conversation is
//     taking it, or the engine is being unloaded), not after every turn. A
//     save rewrites the whole state, and a 90k-token conversation on a 27B
//     model is tens of gigabytes; writing that every turn would wear a disk
//     out to save a read nobody may ever ask for.
//   - To know what a slot holds, this package decides which slot serves each
//     request (the request's id_slot) instead of leaving it to the engine.
//     llama-server does not report which slot it chose, and its /slots does
//     not show the prompt.
//
// The engine still decides what is reusable: it compares tokens, and a
// restored state that does not match the prompt is simply re-read. A wrong
// guess here costs a cache miss, never a wrong answer.
//
// A saved slot contains the conversation's tokens. Turning this on writes
// prompts to disk, which nothing else in ModelFabric does unasked; that is why
// it is off unless configured.
package slotcache

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Block is one link of a prompt's hash chain: it identifies the whole prompt
// up to and including that block (see router.PrefixChain).
type Block = [32]byte

const (
	// minSaveBlocks is the shortest state worth a file. Below it the engine
	// re-reads the prompt faster than a disk round trip is worth the space:
	// 16 blocks is 4 KB of prompt, about a thousand tokens.
	minSaveBlocks = 16
	// minGainBlocks is how much more of the prompt a snapshot must cover than
	// the best slot already does before it is restored over that slot.
	minGainBlocks = 8
	// minHotBlocks is the shortest shared prefix that counts as the same
	// conversation, as in the router's affinity.
	minHotBlocks = 2

	// ioTimeout bounds one save or restore. Generous because the file can be
	// tens of gigabytes and the disk may be a slow one.
	ioTimeout = 10 * time.Minute
)

// snapshot is one saved slot.
type snapshot struct {
	File  string    `json:"file"`
	Sig   string    `json:"sig"`
	Chain string    `json:"chain"` // hex, 64 characters per block
	Bytes int64     `json:"bytes"`
	Token int       `json:"tokens"`
	Saved time.Time `json:"saved"`
	Used  time.Time `json:"used"`

	chain []Block
}

type slot struct {
	sem   chan struct{} // held while a request, save or restore uses the slot
	chain []Block       // what the slot holds; nil when unknown or empty
	// disk is the snapshot this slot was last saved to or restored from, nil
	// when none. What the slot holds beyond it exists nowhere else.
	disk *snapshot
	// task is the engine's id_task for the slot when this package last
	// finished with it. A different value later means something else used the
	// slot (a client dialling the engine directly, llm-d), so what it holds is
	// no longer known.
	task int
	used time.Time
}

type engine struct {
	name, base, sig string
	slots           []*slot
	// off is set when the engine refuses to save, which a build with the
	// projector loaded does. Nothing is asked of it again.
	off bool
}

type blockKey struct {
	sig string
	b   Block
}

// Store is one node's cold tier.
type Store struct {
	dir    string
	max    int64
	client *http.Client
	log    *slog.Logger

	mu      sync.Mutex
	engines map[string]*engine
	snaps   map[string]*snapshot // by file name
	index   map[blockKey]*snapshot
	total   int64

	hits, misses, saves int64
}

// Open reads the snapshots already in dir, so a restart of the node, or of an
// engine, finds what was saved before it. maxBytes caps the directory; the
// least recently used snapshots are removed to stay under it.
func Open(dir string, maxBytes int64, client *http.Client, log *slog.Logger) (*Store, error) {
	// 0700: the files hold prompts.
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("slot cache directory: %w", err)
	}
	s := &Store{dir: dir, max: maxBytes, client: client, log: log,
		engines: map[string]*engine{}, snaps: map[string]*snapshot{}, index: map[blockKey]*snapshot{}}
	metas, _ := filepath.Glob(filepath.Join(dir, "*.json"))
	for _, path := range metas {
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var sn snapshot
		if json.Unmarshal(raw, &sn) != nil || sn.File == "" || sn.File != filepath.Base(sn.File) {
			_ = os.Remove(path)
			continue
		}
		sn.chain = decodeChain(sn.Chain)
		st, err := os.Stat(filepath.Join(dir, sn.File))
		if err != nil || len(sn.chain) == 0 {
			// A sidecar without its state, or one this build cannot read.
			_ = os.Remove(path)
			_ = os.Remove(filepath.Join(dir, sn.File))
			continue
		}
		sn.Bytes = st.Size()
		s.snaps[sn.File] = &sn
		s.total += sn.Bytes
	}
	// State files with no sidecar are a save that was interrupted; nothing
	// says what they hold, so they are only taking space.
	bins, _ := filepath.Glob(filepath.Join(dir, "*.bin"))
	for _, path := range bins {
		if _, ok := s.snaps[filepath.Base(path)]; !ok {
			_ = os.Remove(path)
		}
	}
	s.reindex()
	s.trim()
	return s, nil
}

// Dir is where engines must be told to save (--slot-save-path).
func (s *Store) Dir() string { return s.dir }

// Attach starts tracking an engine's slots. sig identifies what a saved state
// is valid for: the weights, the engine build and the KV types. A state is
// only ever restored into an engine with the same sig.
func (s *Store) Attach(name, baseURL, sig string, slots int) {
	if slots < 1 {
		slots = 1
	}
	e := &engine{name: name, base: strings.TrimRight(baseURL, "/"), sig: sig}
	for i := 0; i < slots; i++ {
		// task -1 matches nothing the engine reports, so a slot is "unknown"
		// until this package has used it. That matters after a node restart:
		// the engine outlived it and its slots hold conversations nobody here
		// remembers.
		e.slots = append(e.slots, &slot{sem: make(chan struct{}, 1), task: -1})
	}
	s.mu.Lock()
	s.engines[name] = e
	s.mu.Unlock()
}

// Detach saves what the engine's idle slots hold and forgets the engine. Call
// it before the engine is stopped: its slots die with it.
func (s *Store) Detach(ctx context.Context, name string) {
	s.mu.Lock()
	e := s.engines[name]
	delete(s.engines, name)
	off := e != nil && e.off
	s.mu.Unlock()
	if e == nil || off {
		return
	}
	tasks := s.tasks(ctx, e)
	for id, sl := range e.slots {
		select {
		case sl.sem <- struct{}{}:
		default:
			continue // still serving; the caller chose not to wait for it
		}
		s.mu.Lock()
		s.forgetIfForeign(sl, id, tasks)
		chain, unsaved := sl.chain, s.unsaved(sl)
		s.mu.Unlock()
		if unsaved >= minSaveBlocks {
			if _, err := s.save(ctx, e, id, chain); err != nil {
				s.log.Warn("slot not saved at unload", "engine", name, "slot", id, "err", err)
			}
		}
		<-sl.sem
	}
}

// Place chooses the slot a request should run in, restoring it from disk when
// a snapshot covers more of the prompt than any slot does, and saving what the
// slot held when that is about to be lost. It returns the slot, or -1 when the
// engine is not tracked and the request should be sent as it is.
//
// done must be called when the request finishes, with the engine's HTTP
// status, or 0 when it never answered. Place blocks while every slot is busy,
// which is where the request would have waited anyway.
func (s *Store) Place(ctx context.Context, name string, chain []Block) (int, func(status int)) {
	none := func(int) {}
	s.mu.Lock()
	e := s.engines[name]
	off := e != nil && e.off
	s.mu.Unlock()
	if e == nil || off {
		return -1, none
	}
	tasks := s.tasks(ctx, e)

	s.mu.Lock()
	for id, sl := range e.slots {
		if len(sl.sem) == 0 {
			s.forgetIfForeign(sl, id, tasks)
		}
	}
	snap, cold := s.lookup(e.sig, chain)
	id, held := s.choose(e, chain, snap, cold)
	s.mu.Unlock()

	sl := e.slots[id]
	if !held {
		select {
		case sl.sem <- struct{}{}:
		case <-ctx.Done():
			return -1, none
		}
	}

	// The slot is ours. Decide again with what it holds now: a request that
	// waited for it may find a different conversation there.
	s.mu.Lock()
	had, unsaved := sl.chain, s.unsaved(sl)
	snap, cold = s.lookup(e.sig, chain)
	s.mu.Unlock()
	hot := common(had, chain)
	restore := snap != nil && cold >= minHotBlocks && cold >= hot+minGainBlocks
	after := hot
	if restore {
		after = common(had, snap.chain)
	}
	// What the slot holds beyond the part that survives is about to be
	// overwritten. Small losses are not worth a file, whether to the request
	// (a regenerated last turn diverges by a block or two) or since the last
	// save (one more question after a restore): a save rewrites the whole
	// state, gigabytes to keep a few hundred tokens. The live run that
	// motivated the second half rewrote an identical 659 MB file on every
	// switch between two conversations.
	if len(had)-after >= minSaveBlocks && unsaved >= minSaveBlocks {
		if sn, err := s.save(ctx, e, id, had); err != nil {
			s.log.Warn("slot not saved before reuse", "engine", name, "slot", id, "err", err)
		} else {
			s.mu.Lock()
			sl.disk = sn
			s.mu.Unlock()
		}
	}
	if restore {
		if err := s.restore(ctx, e, id, snap); err != nil {
			// The file is missing, from another build, or larger than the
			// slot. Whichever: it will fail the same way next time.
			s.log.Warn("snapshot not restored; removing it", "engine", name, "file", snap.File, "err", err)
			s.mu.Lock()
			s.remove(snap)
			s.misses++
			s.mu.Unlock()
		} else {
			s.mu.Lock()
			sl.chain, sl.disk = snap.chain, snap
			snap.Used = time.Now()
			s.hits++
			s.mu.Unlock()
			s.writeMeta(snap)
			s.log.Info("slot restored from disk", "engine", name, "slot", id,
				"tokens", snap.Token, "mib", snap.Bytes>>20)
		}
	} else if hot < minHotBlocks && len(chain) >= minSaveBlocks {
		s.mu.Lock()
		s.misses++
		s.mu.Unlock()
	}

	return id, func(status int) {
		// Read back before releasing, so the task recorded is this request's
		// and not the next one's.
		tasks := s.tasks(context.WithoutCancel(ctx), e)
		s.mu.Lock()
		switch {
		case status >= 200 && status < 300:
			sl.chain = chain
		case status >= 400 && status < 500:
			// Refused before it ran (too long for the context, a malformed
			// body): the slot still holds what it held.
		default:
			// A failed or abandoned request leaves the slot part-way through
			// a prompt. What is there is not known, so it is not recorded and
			// will not be saved under a name it does not deserve.
			sl.chain, sl.disk = nil, nil
		}
		sl.task = -1
		if id < len(tasks) {
			sl.task = tasks[id]
		}
		sl.used = time.Now()
		s.mu.Unlock()
		<-sl.sem
	}
}

// choose picks a slot and tries to take it. held reports whether it did; when
// not, every slot was busy and the caller waits for the one returned. Called
// with s.mu held.
func (s *Store) choose(e *engine, chain []Block, snap *snapshot, cold int) (id int, held bool) {
	take := func(i int) bool {
		select {
		case e.slots[i].sem <- struct{}{}:
			return true
		default:
			return false
		}
	}
	best, hot := -1, 0
	for i, sl := range e.slots {
		if n := common(sl.chain, chain); n > hot {
			best, hot = i, n
		}
	}
	// The slot already holding this conversation. When disk holds materially
	// more of it, that slot is still the one to use if what it has is an
	// earlier state of the same conversation, since restoring over it loses
	// nothing. A slot that merely shares a system prompt with the snapshot
	// holds someone else's conversation, and is treated like any other.
	restoring := snap != nil && cold >= hot+minGainBlocks
	if best >= 0 && hot >= minHotBlocks {
		held := e.slots[best].chain
		if (!restoring || common(held, snap.chain) == len(held)) && take(best) {
			return best, true
		}
	}
	// Otherwise the slot whose loss costs least: one holding nothing known,
	// then the one idle longest. A busy warm slot is not waited for while
	// another is free, since parallel requests sharing a system prompt would
	// then run one at a time.
	order := make([]int, len(e.slots))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool {
		x, y := e.slots[order[a]], e.slots[order[b]]
		if (x.chain == nil) != (y.chain == nil) {
			return x.chain == nil
		}
		return x.used.Before(y.used)
	})
	for _, i := range order {
		if take(i) {
			return i, true
		}
	}
	if best >= 0 && hot >= minHotBlocks {
		return best, false
	}
	return order[0], false
}

// forgetIfForeign drops what is recorded for a slot the engine says was used
// by something else. Called with s.mu held, for a slot not in use here.
func (s *Store) forgetIfForeign(sl *slot, id int, tasks []int) {
	if id >= len(tasks) {
		return // /slots unavailable: keep what is recorded rather than guess
	}
	if sl.task != tasks[id] {
		sl.chain, sl.disk = nil, nil
		sl.task = tasks[id]
	}
}

// unsaved is how many blocks the slot holds that no file has. Called with
// s.mu held.
func (s *Store) unsaved(sl *slot) int {
	if sl.disk == nil || s.snaps[sl.disk.File] != sl.disk {
		return len(sl.chain) // never saved, or the file has since been evicted
	}
	return len(sl.chain) - common(sl.chain, sl.disk.chain)
}

// lookup finds the snapshot covering the longest prefix of chain, and how many
// blocks of it. Called with s.mu held.
func (s *Store) lookup(sig string, chain []Block) (*snapshot, int) {
	for i := len(chain) - 1; i >= 0; i-- {
		if sn, ok := s.index[blockKey{sig, chain[i]}]; ok {
			return sn, i + 1
		}
	}
	return nil, 0
}

// common is how many leading blocks two chains share. A block identifies the
// whole prefix before it, so the chains agree up to the last block that
// matches and the comparison can run from the end.
func common(a, b []Block) int {
	n := min(len(a), len(b))
	for i := n - 1; i >= 0; i-- {
		if a[i] == b[i] {
			return i + 1
		}
	}
	return 0
}

// tasks reads each slot's id_task. A nil result means the engine did not say.
func (s *Store) tasks(ctx context.Context, e *engine) []int {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, e.base+"/slots", nil)
	if err != nil {
		return nil
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil
	}
	var got []struct {
		ID   int `json:"id"`
		Task int `json:"id_task"`
	}
	if json.NewDecoder(resp.Body).Decode(&got) != nil {
		return nil
	}
	out := make([]int, len(got))
	for _, g := range got {
		if g.ID >= 0 && g.ID < len(out) {
			out[g.ID] = g.Task
		}
	}
	return out
}

// action runs one of llama-server's slot actions.
func (s *Store) action(ctx context.Context, e *engine, id int, action, file string) (map[string]any, int, error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), ioTimeout)
	defer cancel()
	body, _ := json.Marshal(map[string]string{"filename": file})
	url := fmt.Sprintf("%s/slots/%d?action=%s", e.base, id, action)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode != http.StatusOK {
		return nil, resp.StatusCode, fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(raw)))
	}
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return out, resp.StatusCode, nil
}

// save writes a slot to disk under its chain. The caller holds the slot.
func (s *Store) save(ctx context.Context, e *engine, id int, chain []Block) (*snapshot, error) {
	tip := chain[len(chain)-1]
	file := e.sig + "-" + hex.EncodeToString(tip[:12]) + ".bin"
	start := time.Now()
	out, code, err := s.action(ctx, e, id, "save", file)
	if err != nil {
		if code == http.StatusNotImplemented {
			// llama-server refuses slot actions with a projector loaded.
			// Engines loaded with vision are not attached, so this is a build
			// that refuses for a reason of its own. Ask once.
			s.mu.Lock()
			e.off = true
			s.mu.Unlock()
			return nil, errors.New("this engine does not support saving slots; the disk cache is off for it")
		}
		return nil, err
	}
	sn := &snapshot{File: file, Sig: e.sig, Chain: encodeChain(chain), chain: chain,
		Saved: time.Now(), Used: time.Now()}
	if n, ok := out["n_saved"].(float64); ok {
		sn.Token = int(n)
	}
	st, err := os.Stat(filepath.Join(s.dir, file))
	if err != nil {
		return nil, fmt.Errorf("engine reported a save but the file is not there: %w", err)
	}
	sn.Bytes = st.Size()
	_ = os.Chmod(filepath.Join(s.dir, file), 0o600)
	if err := s.writeMeta(sn); err != nil {
		_ = os.Remove(filepath.Join(s.dir, file))
		return nil, err
	}

	s.mu.Lock()
	if old := s.snaps[file]; old != nil {
		s.total -= old.Bytes
	}
	s.snaps[file] = sn
	s.total += sn.Bytes
	// An earlier save of the same conversation is a prefix of this one and
	// can never be the better match again.
	for _, old := range s.snaps {
		if old != sn && old.Sig == sn.Sig && len(old.chain) <= len(chain) && common(old.chain, chain) == len(old.chain) {
			s.drop(old)
		}
	}
	s.saves++
	s.reindex()
	s.trim()
	s.mu.Unlock()
	s.log.Info("slot saved to disk", "engine", e.name, "slot", id, "tokens", sn.Token,
		"mib", sn.Bytes>>20, "took", time.Since(start).Round(time.Millisecond))
	return sn, nil
}

func (s *Store) restore(ctx context.Context, e *engine, id int, sn *snapshot) error {
	_, _, err := s.action(ctx, e, id, "restore", sn.File)
	return err
}

func (s *Store) writeMeta(sn *snapshot) error {
	raw, err := json.Marshal(sn)
	if err != nil {
		return err
	}
	path := filepath.Join(s.dir, strings.TrimSuffix(sn.File, ".bin")+".json")
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// drop deletes a snapshot's files and its entry, leaving the index to the
// caller. Called with s.mu held.
func (s *Store) drop(sn *snapshot) {
	if s.snaps[sn.File] != sn {
		return
	}
	delete(s.snaps, sn.File)
	s.total -= sn.Bytes
	_ = os.Remove(filepath.Join(s.dir, sn.File))
	_ = os.Remove(filepath.Join(s.dir, strings.TrimSuffix(sn.File, ".bin")+".json"))
}

// remove is drop plus the index. Called with s.mu held.
func (s *Store) remove(sn *snapshot) {
	s.drop(sn)
	s.reindex()
}

// reindex rebuilds the block index. Where two snapshots share a block the
// longer one wins: it covers everything the shorter does up to that point.
// Called with s.mu held, or before the store is shared.
func (s *Store) reindex() {
	s.index = map[blockKey]*snapshot{}
	for _, sn := range s.snaps {
		for _, b := range sn.chain {
			k := blockKey{sn.Sig, b}
			if cur, ok := s.index[k]; !ok || len(sn.chain) > len(cur.chain) {
				s.index[k] = sn
			}
		}
	}
}

// trim removes the least recently used snapshots until the directory is under
// its cap. Called with s.mu held, or before the store is shared.
func (s *Store) trim() {
	if s.max <= 0 || s.total <= s.max {
		return
	}
	all := make([]*snapshot, 0, len(s.snaps))
	for _, sn := range s.snaps {
		all = append(all, sn)
	}
	sort.Slice(all, func(i, j int) bool { return all[i].Used.Before(all[j].Used) })
	for _, sn := range all {
		if s.total <= s.max {
			break
		}
		s.log.Info("snapshot evicted to stay under cache_disk_mib", "file", sn.File, "mib", sn.Bytes>>20)
		s.drop(sn)
	}
	s.reindex()
}

// Stats is what the cold tier holds and has done since the node started.
type Stats struct {
	Dir       string `json:"dir"`
	Snapshots int    `json:"snapshots"`
	Bytes     int64  `json:"bytes"`
	MaxBytes  int64  `json:"max_bytes"`
	Restores  int64  `json:"restores"`
	Misses    int64  `json:"misses"`
	Saves     int64  `json:"saves"`
}

func (s *Store) Stats() Stats {
	s.mu.Lock()
	defer s.mu.Unlock()
	return Stats{Dir: s.dir, Snapshots: len(s.snaps), Bytes: s.total, MaxBytes: s.max,
		Restores: s.hits, Misses: s.misses, Saves: s.saves}
}

func encodeChain(chain []Block) string {
	var b strings.Builder
	b.Grow(len(chain) * 64)
	for _, c := range chain {
		b.WriteString(hex.EncodeToString(c[:]))
	}
	return b.String()
}

func decodeChain(s string) []Block {
	raw, err := hex.DecodeString(s)
	if err != nil || len(raw)%32 != 0 {
		return nil
	}
	out := make([]Block, len(raw)/32)
	for i := range out {
		copy(out[i][:], raw[i*32:])
	}
	return out
}
