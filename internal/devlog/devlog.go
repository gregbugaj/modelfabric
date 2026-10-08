// Package devlog is the node's developer log: one chronological stream of
// what happened to each request and what the engines said while it happened.
//
// The traffic log (internal/server/traffic.go) has one row per request,
// written when it ends. That answers "what was served". It does not answer
// "what is this request doing right now" or "why did it go there", which is
// what someone debugging an agent against the mesh asks, and what LM Studio's
// developer log is for. Here a request is several entries: received, placed,
// the engine's progress on it, done.
//
// Entries are held in a bounded ring in memory and nothing is written to
// disk. An entry carries a request or response body only when the caller put
// one there, and callers do that only while body capture is switched on (see
// DropBodies).
package devlog

import (
	"sync"
	"time"
)

// Levels, least to most detailed. Debug is the engine's own output and the
// router's per-attempt detail; a reader asks for it.
const (
	Error = "error"
	Warn  = "warn"
	Info  = "info"
	Debug = "debug"
)

// Sources say who wrote an entry.
const (
	Router = "router" // this node's handling of a request
	Engine = "engine" // a line from an engine's own log, or read out of one
	Node   = "node"   // a model loading, an engine stopping
)

// Entry is one line of the developer log.
type Entry struct {
	// Seq orders entries on one node and lets a reader resume after a gap.
	Seq   uint64    `json:"seq"`
	Time  time.Time `json:"time"`
	Level string    `json:"level"`
	// Source is who wrote it: Router, Engine or Node.
	Source string `json:"source"`
	// Node is the machine the entry was written on. Filled in by Log.
	Node string `json:"node,omitempty"`
	// Trace ties a request's entries together, across nodes. Empty for
	// entries that belong to no request, an engine's log lines among them:
	// an engine names a slot and a task, not the request behind them.
	Trace  string `json:"trace,omitempty"`
	Model  string `json:"model,omitempty"`
	Engine string `json:"engine,omitempty"`
	// Msg is the line as a person reads it.
	Msg string `json:"msg"`
	// Fields are the figures behind Msg, for a reader that wants to sort or
	// chart them and not parse a sentence.
	Fields map[string]any `json:"fields,omitempty"`
	// Body is a request or response as sent, cut at the capture cap. Empty
	// unless body capture was on when the entry was written.
	Body      string `json:"body,omitempty"`
	Truncated bool   `json:"truncated,omitempty"`
}

const (
	// DefaultKeep holds a few hundred requests' worth with debug on: an
	// engine writes a dozen lines per request.
	DefaultKeep = 4000
	subBuffer   = 256
)

// Log is a bounded ring of entries with live subscribers. The zero value is
// not usable; a nil *Log accepts and drops everything, so callers need no
// check of their own.
type Log struct {
	node string

	mu   sync.Mutex
	seq  uint64
	ring []Entry
	keep int
	subs map[chan Entry]struct{}
}

// New returns a Log for the named node holding at most keep entries;
// keep <= 0 means DefaultKeep.
func New(node string, keep int) *Log {
	if keep <= 0 {
		keep = DefaultKeep
	}
	return &Log{node: node, keep: keep, subs: map[chan Entry]struct{}{}}
}

// Add records an entry and hands it to every subscriber. Time defaults to
// now and Level to Info.
func (l *Log) Add(e Entry) {
	if l == nil {
		return
	}
	if e.Time.IsZero() {
		e.Time = time.Now()
	}
	if e.Level == "" {
		e.Level = Info
	}
	e.Node = l.node
	l.mu.Lock()
	defer l.mu.Unlock()
	l.seq++
	e.Seq = l.seq
	l.ring = append(l.ring, e)
	if len(l.ring) > l.keep {
		// Copy down, so the backing array does not grow without bound.
		l.ring = append(l.ring[:0], l.ring[len(l.ring)-l.keep:]...)
	}
	for ch := range l.subs {
		// A reader that has fallen behind loses entries. Routing never waits
		// for someone watching a log.
		select {
		case ch <- e:
		default:
		}
	}
}

// Shows reports whether an entry at level is shown to a reader asking for
// atLeast. An unknown atLeast reads as Info.
func Shows(level, atLeast string) bool {
	rank := map[string]int{Error: 0, Warn: 1, Info: 2, Debug: 3}
	want, ok := rank[atLeast]
	if !ok {
		want = rank[Info]
	}
	got, ok := rank[level]
	if !ok {
		got = rank[Info]
	}
	return got <= want
}

// Recent returns up to limit of the latest entries at or above level, oldest
// first. limit <= 0 means all that are held.
func (l *Log) Recent(limit int, level string) []Entry {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]Entry, 0, min(len(l.ring), max(limit, 0)+64))
	for _, e := range l.ring {
		if Shows(e.Level, level) {
			out = append(out, e)
		}
	}
	if limit > 0 && len(out) > limit {
		out = out[len(out)-limit:]
	}
	return out
}

// Subscribe returns a channel of entries from now on, the entries already
// held, and a function that ends the subscription.
func (l *Log) Subscribe() (<-chan Entry, []Entry, func()) {
	if l == nil {
		return nil, nil, func() {}
	}
	ch := make(chan Entry, subBuffer)
	l.mu.Lock()
	l.subs[ch] = struct{}{}
	backlog := append([]Entry(nil), l.ring...)
	l.mu.Unlock()
	return ch, backlog, func() {
		l.mu.Lock()
		delete(l.subs, ch)
		l.mu.Unlock()
	}
}

// DropBodies removes every body already held. Called when body capture is
// switched off, so prompts do not sit in memory until the ring rolls over.
func (l *Log) DropBodies() {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	for i := range l.ring {
		l.ring[i].Body, l.ring[i].Truncated = "", false
	}
}
