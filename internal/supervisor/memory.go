package supervisor

import (
	"bytes"
	"context"
	"io"
	"os"
	"sync"
	"time"

	"github.com/gregbugaj/modelfabric/internal/osproc"
)

// llama-server exposes RAM-cache drops, but not occupancy, at the configured
// log level. These markers cover eviction and entries too large to retain.
var cacheDropMarks = [][]byte{
	[]byte("making room for prompt cache entry"), // the oldest entry removed
	[]byte("exceeds cache size limit"),           // one too large to keep at all
}

// longestMark is at least the length of the longest mark: one byte less than
// it of a log's tail is kept between reads, so a message split across two
// reads is seen whole in the second.
const longestMark = 40

type engineWatch struct {
	offset  int64
	carry   []byte
	dropped int64
	rssMB   int64
	rssAt   time.Time
}

type memoryWatch struct {
	mu sync.Mutex
	by map[string]*engineWatch
	// gpuByPID is nvidia-smi's last answer for every process, and gpuAt when
	// it was asked. One question covers all of a node's engines.
	gpuByPID map[int][]osproc.GPUUse
	gpuAt    time.Time
	// gpuGoodAt is when nvidia-smi last answered at all. A failed ask keeps
	// the last answer, but only for gpuTrust: after that it is not known.
	gpuGoodAt time.Time
	// askGPU stands in for nvidia-smi in tests; nil means the real one.
	askGPU func(context.Context) map[int][]osproc.GPUUse
}

// gpuTrust is how long the last answer stands while nvidia-smi is failing.
// One failed ask (a driver reset, a timeout) used to blank every engine's GPU
// memory until the next ask five seconds later. Longer than a few asks and
// the figure is no longer a measurement, so it reads unknown again.
const gpuTrust = 30 * time.Second

// gpu returns what the process holds on each GPU, nil when unknown. Peers
// poll state often and nvidia-smi takes tens of milliseconds, so it is asked
// at most every five seconds.
func (m *memoryWatch) gpu(pid int) []osproc.GPUUse {
	if pid <= 0 {
		return nil
	}
	m.mu.Lock()
	stale := time.Since(m.gpuAt) > 5*time.Second
	if stale {
		// Stamped before asking, so callers arriving meanwhile use the last
		// answer and do not each start an nvidia-smi of their own.
		m.gpuAt = time.Now()
	}
	m.mu.Unlock()
	if stale {
		// Not under the lock: a driver that is slow to answer must not hold
		// up the state every peer polls for.
		ask := m.askGPU
		if ask == nil {
			ask = osproc.GPUMemoryByPID
		}
		// nil is nvidia-smi missing or failing; an answer naming no process
		// is an empty map, and is an answer.
		fresh := ask(context.Background())
		m.mu.Lock()
		switch {
		case fresh != nil:
			m.gpuByPID, m.gpuGoodAt = fresh, time.Now()
		case time.Since(m.gpuGoodAt) > gpuTrust:
			m.gpuByPID = nil
		}
		m.mu.Unlock()
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.gpuByPID[pid]
}

// read returns cumulative RAM-cache drops and RSS in MiB (zero if unavailable).
// RSS probes are throttled because peer polls are frequent and macOS needs a
// subprocess for each measurement.
func (m *memoryWatch) read(id, logPath string, pid int) (dropped, rssMB int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.by == nil {
		m.by = map[string]*engineWatch{}
	}
	w := m.by[id]
	if w == nil {
		w = &engineWatch{}
		m.by[id] = w
	}
	if logPath != "" {
		w.count(logPath)
	}
	if pid > 0 && time.Since(w.rssAt) > 5*time.Second {
		w.rssAt = time.Now()
		w.rssMB = 0
		if b, ok := osproc.Resident(pid); ok {
			w.rssMB = int64(b >> 20)
		}
	}
	return w.dropped, w.rssMB
}

func (m *memoryWatch) forget(id string) {
	m.mu.Lock()
	delete(m.by, id)
	m.mu.Unlock()
}

// count reads what the log has gained since the last call and adds the drops
// in it. A log that is shorter than it was has been replaced, and is read
// from the start.
func (w *engineWatch) count(path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return
	}
	if st.Size() < w.offset {
		w.offset, w.carry, w.dropped = 0, nil, 0
	}
	if st.Size() == w.offset {
		return
	}
	if _, err := f.Seek(w.offset, io.SeekStart); err != nil {
		return
	}
	// Bounded, so a log that grew by gigabytes between two looks is caught up
	// over several and never read into memory at once.
	buf := make([]byte, min(st.Size()-w.offset, 4<<20))
	n, _ := io.ReadFull(f, buf)
	if n == 0 {
		return
	}
	w.offset += int64(n)
	// The tail of the last read goes in front, so a message split across the
	// two is whole here. One that lay entirely inside that tail was counted
	// last time, so only those reaching past it count now.
	old := len(w.carry)
	data := append(w.carry, buf[:n]...)
	for _, mark := range cacheDropMarks {
		for at := 0; ; {
			i := bytes.Index(data[at:], mark)
			if i < 0 {
				break
			}
			if at+i+len(mark) > old {
				w.dropped++
			}
			at += i + len(mark)
		}
	}
	w.carry = append([]byte(nil), data[len(data)-min(len(data), longestMark-1):]...)
}
