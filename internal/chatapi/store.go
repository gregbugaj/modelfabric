package chatapi

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"sync"
	"time"
)

// Store keeps conversations so a request can continue one by its
// response_id. It is memory only, on the node that answered: a restart
// forgets everything, and nothing is written to disk. A conversation is
// prompt text, so it is bounded by count, by size and by age rather than kept.
type Store struct {
	mu    sync.Mutex
	max   int
	ttl   time.Duration
	now   func() time.Time
	items map[string]*stored
	// bytes is what the conversations held add up to, against maxBytes. An
	// agent's conversation runs to megabytes, so a count alone would let a
	// thousand of them take the node's memory.
	bytes, maxBytes int64
}

type stored struct {
	model    string
	messages []map[string]any
	used     time.Time
	size     int64
}

// NewStore keeps at most max conversations, each for ttl after its last use.
func NewStore(max int, ttl time.Duration) *Store {
	return &Store{max: max, ttl: ttl, now: time.Now, items: map[string]*stored{}, maxBytes: 256 << 20}
}

func (s *Store) Put(model string, messages []map[string]any) string {
	raw := make([]byte, 24)
	rand.Read(raw)
	id := "resp_" + hex.EncodeToString(raw)
	s.PutID(id, model, messages)
	return id
}

// PutID stores a conversation under an id the caller already has: the one an
// engine gave its response. A conversation too large to hold beside the
// others is not stored, and continuing it is answered as unknown.
func (s *Store) PutID(id, model string, messages []map[string]any) {
	b, _ := json.Marshal(messages)
	size := int64(len(b))
	s.mu.Lock()
	defer s.mu.Unlock()
	if size > s.maxBytes/4 {
		return
	}
	now := s.now()
	for k, v := range s.items {
		if now.Sub(v.used) > s.ttl {
			s.drop(k)
		}
	}
	for len(s.items) >= s.max || (s.bytes+size > s.maxBytes && len(s.items) > 0) {
		oldest, at := "", now
		for k, v := range s.items {
			if !v.used.After(at) {
				oldest, at = k, v.used
			}
		}
		s.drop(oldest)
	}
	s.drop(id)
	s.items[id] = &stored{model: model, messages: messages, used: now, size: size}
	s.bytes += size
}

func (s *Store) drop(id string) {
	if v, ok := s.items[id]; ok {
		s.bytes -= v.size
		delete(s.items, id)
	}
}

// Get returns a stored conversation's messages. The slice is the caller's to
// append to: a response can be continued more than once, each a branch.
func (s *Store) Get(id string) ([]map[string]any, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.items[id]
	if !ok || s.now().Sub(v.used) > s.ttl {
		s.drop(id)
		return nil, false
	}
	v.used = s.now()
	return append([]map[string]any(nil), v.messages...), true
}

func (s *Store) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.items)
}
