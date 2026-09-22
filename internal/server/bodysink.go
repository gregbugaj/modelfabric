package server

import (
	"encoding/json"
	"os"
	"sync"

	"github.com/gregbugaj/modelfabric/internal/router"
)

// bodySink appends captured requests to a file, one JSON object per line.
//
// Separate from the in-memory ring on purpose: the ring forgets, a file does
// not. Turning this on is a decision about what ends up on disk, so it is a
// config setting rather than a button in the dashboard.
type bodySink struct {
	mu sync.Mutex
	f  *os.File
}

func newBodySink(path string) (*bodySink, error) {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	return &bodySink{f: f}, nil
}

// write appends one event. Failures are dropped rather than surfaced: a full
// disk must not turn into failed inference.
func (s *bodySink) write(e router.Event) {
	if s == nil || s.f == nil {
		return
	}
	b, err := json.Marshal(e)
	if err != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, _ = s.f.Write(append(b, '\n'))
}

func (s *bodySink) Close() error {
	if s == nil || s.f == nil {
		return nil
	}
	return s.f.Close()
}
