// Command routesim replays a recorded benchmark run through the router's
// placement logic, in virtual time, and reports what each engine would have
// had to read again.
//
//	go run ./bench/routesim -run ~/.local/share/modelfabric/bench/swe/runs/1005b-litellm
//	go run ./bench/routesim -run RUN -policy least-busy
//	go run ./bench/routesim -run RUN -fleet 'xpredator:2:131072:1761:101:8192,...'
//
// It exists because of how the placement faults of 2026-10-05 were found: run
// a 90-minute benchmark on three GPUs, read the counters, change a rule, run it
// again. Five faults took six runs and a working day. A replay takes seconds,
// so a rule can be tried against every recorded run before any GPU is used, and
// the real benchmark is left to confirm it.
//
// The "router" policy is the router's own code (router.Placement and
// mesh.Rank), not a copy. What is modelled is everything around it: the
// engines. That model is deliberately small, and wrong in ways that matter for
// absolute times (see engine.rates). It is for comparing placements on the same
// workload, and -check prints the recorded run beside the simulated one so that
// how far to trust it is a number rather than a feeling.
//
// That check is not a formality. The first model predicted 34 minutes and 0.85M
// tokens read again for a rule whose real run took 88 minutes and 3.6M. It was
// missing two things, both read off the recorded calls afterwards: the router
// places from a view of its peers up to a poll old (see sim.poll), and an
// engine writing an answer all but stops while another slot reads a prompt
// (see engine.rates). A rule is only worth a GPU run once the replay of the
// run it was recorded from lands near what that run measured.
package main

import (
	"container/heap"
	"encoding/json"
	"flag"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gregbugaj/modelfabric/internal/mesh"
	"github.com/gregbugaj/modelfabric/internal/router"
)

// turn is one model call of a recorded conversation.
type turn struct {
	prompt, out int     // tokens
	pause       float64 // seconds the agent took before its next call
	body        []byte  // the request as sent, for the prefix hash
	// What actually happened, for -check.
	recCached int
	recSecs   float64
	recWait   float64 // from the request being sent to the engine starting on it
	recEngine string  // the engine's build, the only name a response carries
	recEnd    float64 // when the answer arrived, unix seconds
}

type task struct {
	id    string
	turns []turn
}

// sharedPrefix is what every conversation has cached everywhere: the system
// prompt. Read off the engines' slots during the 2026-10-05 runs, where a
// conversation landing cold reported exactly this many tokens from cache.
const sharedPrefix = 321

// kibPerToken is the size of saved KV state per token for Qwen3.8-27B with an
// f16 cache, from llama-server's own message on minion: "prompt state size
// 10486.991 MiB" for a 127,049-token prompt.
const kibPerToken = 84.5

func loadRun(dir, model string) ([]task, error) {
	files, _ := filepath.Glob(filepath.Join(dir, "*", "*.traj.json"))
	if len(files) == 0 {
		return nil, fmt.Errorf("no trajectories under %s", dir)
	}
	sort.Strings(files)
	var tasks []task
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			return nil, err
		}
		var tr struct {
			InstanceID string `json:"instance_id"`
			Messages   []struct {
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
				Extra   struct {
					Timestamp float64 `json:"timestamp"`
					Response  struct {
						Fingerprint string `json:"system_fingerprint"`
						Usage       struct {
							Prompt     int `json:"prompt_tokens"`
							Completion int `json:"completion_tokens"`
						} `json:"usage"`
						Timings struct {
							CacheN      int     `json:"cache_n"`
							PromptMs    float64 `json:"prompt_ms"`
							PredictedMs float64 `json:"predicted_ms"`
						} `json:"timings"`
					} `json:"response"`
				} `json:"extra"`
			} `json:"messages"`
		}
		if err := json.Unmarshal(raw, &tr); err != nil {
			return nil, fmt.Errorf("%s: %w", f, err)
		}
		t := task{id: tr.InstanceID}
		type msg struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		}
		var sent []msg
		asked := 0.0 // when the request now being answered was sent
		for i, m := range tr.Messages {
			if m.Role == "assistant" && m.Extra.Response.Usage.Prompt > 0 {
				body, _ := json.Marshal(map[string]any{"model": model, "messages": sent})
				tn := turn{
					prompt: m.Extra.Response.Usage.Prompt, out: m.Extra.Response.Usage.Completion,
					body: body, recCached: m.Extra.Response.Timings.CacheN,
					recSecs:   (m.Extra.Response.Timings.PromptMs + m.Extra.Response.Timings.PredictedMs) / 1000,
					recEngine: m.Extra.Response.Fingerprint, recEnd: m.Extra.Timestamp,
				}
				if asked > 0 {
					tn.recWait = max(m.Extra.Timestamp-asked-tn.recSecs, 0)
				}
				// The pause is from this answer to the last message before the
				// next call: the agent running the command it was given.
				last := m.Extra.Timestamp
				for _, n := range tr.Messages[i+1:] {
					if n.Role == "assistant" {
						break
					}
					if n.Extra.Timestamp > last {
						last = n.Extra.Timestamp
					}
				}
				tn.pause = last - m.Extra.Timestamp
				asked = last
				t.turns = append(t.turns, tn)
			}
			sent = append(sent, msg{m.Role, m.Content})
		}
		if len(t.turns) > 0 {
			tasks = append(tasks, t)
		}
	}
	return tasks, nil
}

// slot is one of an engine's slots and the conversation cached in it.
type slot struct {
	conv   string
	tokens int
	busy   bool
	used   float64 // when it was last given a request
	freed  float64 // when it last finished one
}

type parked struct {
	conv   string
	tokens int
}

// engine models one llama-server: slots that each keep one conversation's KV,
// a pool those share, and a host-RAM cache that evicted conversations are
// parked in and restored from.
type engine struct {
	name            string
	slots           []slot
	pool            int
	prefill, decode float64 // tokens a second, one request alone
	ramTokens       int
	ram             []parked // oldest first
	queue           []*request
	// How this engine's requests slow each other, measured; see rates.
	pshare, dshare, dstarve float64

	running []*request
	last    float64 // when running was last brought up to date
	gen     int     // bumped on every change, so a superseded wake-up is ignored

	// What the router knows of this engine, as of the last poll; see sim.poll.
	reported, pending int
	published         float64
	// Prompt tokens read and the seconds spent reading them: the lifetime
	// average the mesh computes from llama-server's counters and routes by.
	readTokens  int
	readSeconds float64

	// served is each finished request's read and cached tokens, for -trace.
	served []hitAt
	// Totals.
	requests, prefilled, cached, generated int
}

type hitAt struct {
	at           float64
	read, cached int
}

// recentHit is the engine's cache hit rate over the last 90 seconds, for
// -trace; zero when it has not served enough to say.
func (e *engine) recentHit(now float64) float64 {
	read, cached := 0, 0
	for i := len(e.served) - 1; i >= 0 && now-e.served[i].at <= 90; i-- {
		read += e.served[i].read
		cached += e.served[i].cached
	}
	if read+cached < 20000 {
		return 0
	}
	return max(float64(cached)/float64(read+cached), 0.0001)
}

type request struct {
	conv        string
	prompt, out int
	arrived     float64
	done        func(now float64)
	cachedGot   int
	// What is left of it: a restore from the RAM cache, then the prompt, then
	// the answer.
	restore, toRead, toWrite float64
}

func (e *engine) busy() int {
	n := 0
	for _, s := range e.slots {
		if s.busy {
			n++
		}
	}
	return n
}

func (e *engine) held() int {
	n := 0
	for _, s := range e.slots {
		n += s.tokens
	}
	return n
}

// park puts an evicted conversation in the RAM cache if it fits, dropping the
// oldest to make room. One too large for the whole cache is simply lost, which
// is what llama-server does ("exceeds cache size limit, skipping").
func (e *engine) park(conv string, tokens int) {
	if conv == "" || tokens <= sharedPrefix || tokens > e.ramTokens {
		return
	}
	e.unpark(conv)
	e.ram = append(e.ram, parked{conv, tokens})
	total := 0
	for _, p := range e.ram {
		total += p.tokens
	}
	for total > e.ramTokens && len(e.ram) > 0 {
		total -= e.ram[0].tokens
		e.ram = e.ram[1:]
	}
}

func (e *engine) unpark(conv string) (int, bool) {
	for i, p := range e.ram {
		if p.conv == conv {
			e.ram = append(e.ram[:i], e.ram[i+1:]...)
			return p.tokens, true
		}
	}
	return 0, false
}

// serve starts r in a free slot. The caller has checked that one is free, and
// brought the engine up to date.
//
// Slot choice follows llama-server: the slot holding this conversation if it
// is idle, otherwise the idle slot unused longest, whose own conversation is
// parked. A conversation found in the RAM cache is restored instead of read.
func (e *engine) serve(r *request, now float64) {
	pick := -1
	for i, s := range e.slots {
		if !s.busy && s.conv == r.conv {
			pick = i
			break
		}
	}
	cached, restore := sharedPrefix, 0.0
	if pick < 0 {
		// An empty slot before anyone's: llama-server takes the slot unused
		// longest, and one never used is older than any. Choosing by time
		// alone evicted a conversation while a slot beside it stood empty.
		for i, s := range e.slots {
			if s.busy {
				continue
			}
			switch {
			case pick < 0:
				pick = i
			case (s.conv == "") != (e.slots[pick].conv == ""):
				if s.conv == "" {
					pick = i
				}
			case s.used < e.slots[pick].used:
				pick = i
			}
		}
		old := e.slots[pick]
		e.slots[pick].conv, e.slots[pick].tokens = "", 0
		if n, ok := e.unpark(r.conv); ok {
			cached, restore = n, 1.0
		}
		e.park(old.conv, old.tokens)
	} else {
		cached = e.slots[pick].tokens
	}
	cached = min(cached, r.prompt)
	// The pool: other idle slots give up their conversations, oldest first,
	// until this one fits.
	need := r.prompt + r.out
	for e.held()-e.slots[pick].tokens+need > e.pool {
		victim := -1
		for i, s := range e.slots {
			if i != pick && !s.busy && s.tokens > 0 && (victim < 0 || s.used < e.slots[victim].used) {
				victim = i
			}
		}
		if victim < 0 {
			break
		}
		e.park(e.slots[victim].conv, e.slots[victim].tokens)
		e.slots[victim].conv, e.slots[victim].tokens = "", 0
	}
	e.slots[pick] = slot{conv: r.conv, tokens: need, busy: true, used: now}
	r.cachedGot = cached
	r.restore, r.toRead, r.toWrite = restore, float64(r.prompt-cached), float64(r.out)
	e.running = append(e.running, r)
	e.requests++
	e.prefilled += r.prompt - cached
	e.cached += cached
	e.generated += r.out
	e.served = append(e.served, hitAt{now, r.prompt - cached, cached})
}

// rates is how fast each running request reads and writes right now, in tokens
// a second.
//
// The numbers are what the engines did in the two full runs of 2026-10-05,
// from each call's own timings sorted by what else the engine was doing:
//
//	                     xpredator (5090)   minion (6000 Ada)
//	reading, alone            1500-1800         500-570
//	reading, beside another    800-1080         350-400
//	writing, alone               75-81           60-72
//	writing, beside a writer       81            36-44
//	writing, beside a reader     16-20            5-7
//
// The last row is what the first model lacked, and it is most of the cost of a
// cold read. A slot reading a prompt takes the GPU in 2048-token batches and a
// slot writing gets one step between them, so every other conversation on the
// engine writes at a tenth of its speed until the read is over. On minion,
// 4,100 of the 8,500 seconds spent writing in the router run were spent like
// that. A re-read costs its own time and everyone else's.
func (e *engine) rates() (read, write float64) {
	readers, writers := 0, 0
	for _, r := range e.running {
		switch {
		case r.restore > 0:
		case r.toRead > 0:
			readers++
		default:
			writers++
		}
	}
	read, write = e.prefill, e.decode
	if readers > 1 {
		read = e.prefill * e.pshare / float64(readers)
	}
	switch {
	case readers > 0:
		write = min(e.dstarve, e.decode)
	case writers > 1:
		write = e.decode * e.dshare
	}
	return read, write
}

// advance brings every running request up to now at the rates that have held
// since the last change, and returns those that finished.
func (e *engine) advance(now float64) []*request {
	dt := now - e.last
	e.last = now
	if dt <= 0 {
		dt = 0
	}
	read, write := e.rates()
	var done []*request
	keep := e.running[:0]
	for _, r := range e.running {
		switch {
		case r.restore > 0:
			r.restore -= dt
		case r.toRead > 0:
			n := min(read*dt, r.toRead)
			r.toRead -= n
			e.readTokens += int(n + .5)
			e.readSeconds += dt
		default:
			r.toWrite -= write * dt
		}
		// A wake-up is set for the moment a phase ends, and lands a rounding
		// error to either side of it.
		if r.restore < 1e-6 {
			r.restore = 0
		}
		if r.toRead < 1e-3 {
			r.toRead = 0
		}
		if r.restore == 0 && r.toRead == 0 && r.toWrite < 1e-3 {
			done = append(done, r)
			continue
		}
		keep = append(keep, r)
	}
	e.running = keep
	return done
}

// next is how long until some running request finishes the phase it is in, at
// the current rates. ok is false when nothing is running.
func (e *engine) next() (float64, bool) {
	read, write := e.rates()
	best, ok := 0.0, false
	for _, r := range e.running {
		var t float64
		switch {
		case r.restore > 0:
			t = r.restore
		case r.toRead > 0:
			t = r.toRead / read
		default:
			t = r.toWrite / write
		}
		if !ok || t < best {
			best, ok = t, true
		}
	}
	return best, ok
}

// measuredRate is the engine's prefill rate as the mesh would compute it: a
// lifetime average, and nothing until it has read enough to be trusted
// (mesh.minTrustedPrefillTokens).
func (e *engine) measuredRate() float64 {
	if e.readTokens < 20000 || e.readSeconds <= 0 {
		return 0
	}
	return float64(e.readTokens) / e.readSeconds
}

func (e *engine) release(conv string, now float64) {
	for i := range e.slots {
		if e.slots[i].busy && e.slots[i].conv == conv {
			e.slots[i].busy, e.slots[i].freed = false, now
			return
		}
	}
}

// settled reports whether the engine has a slot that has been free for at
// least grace seconds: one whose conversation, if it has one, has had time to
// come back for it and has not.
func (e *engine) settled(now, grace float64) bool {
	for _, s := range e.slots {
		if !s.busy && (s.conv == "" || now-s.freed >= grace) {
			return true
		}
	}
	return false
}

// events is the simulation's clock: a heap of things due to happen.
type event struct {
	at  float64
	seq int
	fn  func(now float64)
}
type events []event

func (h events) Len() int { return len(h) }
func (h events) Less(i, j int) bool {
	if h[i].at != h[j].at {
		return h[i].at < h[j].at
	}
	return h[i].seq < h[j].seq
}
func (h events) Swap(i, j int) { h[i], h[j] = h[j], h[i] }
func (h *events) Push(x any)   { *h = append(*h, x.(event)) }
func (h *events) Pop() any     { o := *h; x := o[len(o)-1]; *h = o[:len(o)-1]; return x }

type sim struct {
	now     float64
	q       events
	seq     int
	engines []*engine
	policy  string
	place   *router.Placement
	rng     *rand.Rand
	model   string

	// holdGrace and holdMax turn on holding at the router: a request whose
	// home has no free slot is not handed to an engine to queue inside it,
	// where it cannot be recalled. It waits here for a slot that has stayed
	// free for holdGrace seconds, up to holdMax, and then goes wherever the
	// policy would have sent it.
	//
	// Tried 2026-10-05 and not adopted. It halved the slowest calls (p99 358s
	// to about 180s at 8 workers) by keeping requests out of the slow node's
	// queue, but the overflow then took GPU slots instead: 0.85M tokens read
	// again became 1.4-2.2M, wall clock did not improve, and with a grace
	// shorter than an agent's longest pause it was far worse (4.7M at 1s). A
	// request held here is also waiting time the caller sees directly. Kept
	// behind its flag so the next person can measure it rather than rebuild it.
	holdGrace, holdMax float64
	held               int

	// poll is how old the router's view of the engines may be, in seconds; 0
	// for a router that sees them exactly, which since the fix below it does
	// for every request it sent itself. -poll 2 is the router as it was, and
	// what the recorded run 1005b-router-v7-stale-peer-load-88min has to be
	// replayed with to land near what it measured.
	//
	// In the benchmark every request enters at one node and every engine is
	// that node's peer. It learned a peer's load from a poll every two seconds
	// and, in between, added one for each request it sent there. Nothing was
	// taken off when a request finished: that waited for the next poll. An agent
	// asks again 0.2 seconds after its last answer (the median of 775 recorded
	// calls), so its own finished request is still counted against its home,
	// and on a two-slot engine with its neighbour busy the home reads as full.
	// Measured in the 88-minute router run of 2026-10-05: 142 moves, 109 of
	// them away from an engine that had a slot free, and 2.6M of the 3.6M
	// tokens read again were read on the call straight after a move.
	//
	// A single proxy that counts its own requests in and out (LiteLLM) has no
	// such gap, so the least-busy policies always see the engines exactly. The
	// mesh now counts the same way (mesh.Peer.sent).
	poll      float64
	waited    float64 // seconds requests spent queued for a slot
	engineSec float64 // seconds requests spent being served

	home       map[string]string
	lastSeen   map[string]float64
	trace      float64 // seconds between trace lines, 0 for none
	nextTrace  float64
	migrations int
	latencies  []float64
}

// traceLine prints, per engine, how many conversations are on it (seen within
// the last minute), how many requests it is running, and its recent cache hit.
func (s *sim) traceLine() {
	fmt.Printf("  t+%4.0fm", s.now/60)
	for _, e := range s.engines {
		n := 0
		for conv, h := range s.home {
			if h == e.name && s.now-s.lastSeen[conv] <= 60 {
				n++
			}
		}
		fmt.Printf("  %s: %d convs on %d slots, %d busy +%d queued, hit %3.0f%%", e.name, n, len(e.slots), e.busy(), len(e.queue), 100*e.recentHit(s.now))
	}
	fmt.Println()
}

func (s *sim) at(t float64, fn func(float64)) {
	s.seq++
	heap.Push(&s.q, event{t, s.seq, fn})
}

func (s *sim) engine(name string) *engine {
	for _, e := range s.engines {
		if e.name == name {
			return e
		}
	}
	return nil
}

// choose picks the engine for a request under the policy being tried.
func (s *sim) choose(conv string, body []byte) (*engine, func(), func(time.Duration, int)) {
	switch s.policy {
	case "random":
		return s.engines[s.rng.Intn(len(s.engines))], func() {}, func(time.Duration, int) {}
	case "least-busy", "least-busy-first":
		// LiteLLM's least-busy: fewest requests it has in flight, and no idea
		// of slots, speed or caches. Ties go to chance, or with -first to the
		// engine listed first.
		var best []*engine
		for _, e := range s.engines {
			n := e.busy() + len(e.queue)
			switch {
			case len(best) == 0 || n < best[0].busy()+len(best[0].queue):
				best = []*engine{e}
			case n == best[0].busy()+len(best[0].queue):
				best = append(best, e)
			}
		}
		if s.policy == "least-busy-first" {
			return best[0], func() {}, func(time.Duration, int) {}
		}
		return best[s.rng.Intn(len(best))], func() {}, func(time.Duration, int) {}
	}
	// Experimental policies, tried here before anything is written into the
	// router. Each is a few lines, and a replay of two recorded runs is a
	// second; a rule that loses to least-busy here does not get deployed.
	if strings.HasPrefix(s.policy, "x-") {
		return s.experiment(conv), func() {}, func(time.Duration, int) {}
	}
	choice := s.orderFor(body)
	name := choice.Candidates[0].Name
	var attempt *router.Attempt
	return s.engine(name), func() { attempt = s.place.Placed(choice, name) }, func(time.Duration, int) { attempt.Finished(0, 0) }
}

// load is how many requests the router believes e is carrying.
func (s *sim) load(e *engine) int {
	if s.poll > 0 {
		return e.reported + e.pending
	}
	return e.busy() + len(e.queue)
}

// rate is the prefill rate the router believes e has.
func (s *sim) rate(e *engine) float64 {
	if s.poll > 0 {
		return e.published
	}
	return e.measuredRate()
}

// pollAll is the mesh's poll: every engine's true load and measured rate, as
// of now, become what the router knows until the next one.
func (s *sim) pollAll(float64) {
	for _, e := range s.engines {
		e.reported, e.pending = e.busy()+len(e.queue), 0
		e.published = e.measuredRate()
	}
	// With nothing else due the run is over; a poll that kept itself alive
	// would run the clock for ever.
	if s.q.Len() > 0 {
		s.at(s.now+s.poll, s.pollAll)
	}
}

// orderFor is the router's own ordering of the engines for a request.
func (s *sim) orderFor(body []byte) router.Choice {
	cands := make([]mesh.Candidate, 0, len(s.engines))
	for _, e := range s.engines {
		cands = append(cands, mesh.Candidate{
			// Local, so that the mesh scores it on Inflight: a peer's load is
			// a field only the mesh can set.
			Local: true, Node: e.name, Name: e.name,
			Inflight: int64(s.load(e)), Slots: int64(len(e.slots)),
			PrefillTokS: s.rate(e),
		})
	}
	mesh.Rank(cands, true, 0, "")
	return s.place.Order(cands, s.model, body, "")
}

// experiment is the engine for conv under one of the x- policies.
//
//	x-sticky-lb     home while it has a free slot, else fewest in flight
//	x-sticky-free   home while it has a free slot, else the fastest engine
//	                with a free slot, else the shortest queue per slot
//	x-sticky-fill   home while it has a free slot, else the emptiest engine
//	                by requests per slot
//	x-fill          the emptiest engine by requests per slot, every time
func (s *sim) experiment(conv string) *engine {
	load := s.load
	free := func(e *engine) bool { return load(e) < len(e.slots) }
	fill := func(e *engine) float64 { return float64(load(e)) / float64(len(e.slots)) }
	if s.policy != "x-fill" {
		if h := s.engine(s.home[conv]); h != nil && free(h) {
			return h
		}
	}
	best := s.engines[0]
	for _, e := range s.engines[1:] {
		var better bool
		switch s.policy {
		case "x-sticky-lb":
			better = load(e) < load(best)
		case "x-sticky-lb-fast": // ties to the faster engine
			better = load(e) < load(best) || (load(e) == load(best) && e.prefill > best.prefill)
		case "x-sticky-lb-free": // a free slot first, then fewest in flight, ties to the faster
			switch {
			case free(e) != free(best):
				better = free(e)
			default:
				better = load(e) < load(best) || (load(e) == load(best) && e.prefill > best.prefill)
			}
		case "x-sticky-free":
			switch {
			case free(e) != free(best):
				better = free(e)
			case free(e):
				better = e.prefill > best.prefill
			default:
				better = fill(e) < fill(best) || (fill(e) == fill(best) && e.prefill > best.prefill)
			}
		default: // x-sticky-fill, x-fill
			better = fill(e) < fill(best) || (fill(e) == fill(best) && e.prefill > best.prefill)
		}
		if better {
			best = e
		}
	}
	return best
}

func (s *sim) send(conv string, t turn, done func(now float64)) {
	s.sendAt(conv, t, done, s.now, true)
}

// sendAt places a request that arrived at arrived. With holding on it may
// instead wait a moment and try again.
func (s *sim) sendAt(conv string, t turn, done func(now float64), arrived float64, first bool) {
	e, placed, finished := s.choose(conv, t.body)
	if s.holdMax > 0 {
		home := s.engine(s.home[conv])
		atHome := home != nil && home.busy()+len(home.queue) < len(home.slots)
		switch {
		case atHome:
			// its own slot is free: nothing to wait for
		case s.now-arrived >= s.holdMax:
			// waited long enough; go where the policy says
		default:
			// The fastest engine with a slot that has stayed free.
			var free *engine
			for _, c := range s.engines {
				if c.busy()+len(c.queue) < len(c.slots) && c.settled(s.now, s.holdGrace) && (free == nil || c.prefill > free.prefill) {
					free = c
				}
			}
			if free == nil {
				if first {
					s.held++
				}
				s.at(s.now+0.25, func(float64) { s.sendAt(conv, t, done, arrived, false) })
				return
			}
			if free != e {
				e = free
				name := e.name
				if s.place != nil && !strings.HasPrefix(s.policy, "x-") && s.policy == "router" {
					choice := s.orderFor(t.body)
					var attempt *router.Attempt
					placed = func() { attempt = s.place.Placed(choice, name) }
					finished = func(time.Duration, int) { attempt.Finished(0, 0) }
				}
			}
		}
	}
	placed()
	if h, ok := s.home[conv]; ok && h != e.name {
		s.migrations++
	}
	s.home[conv] = e.name
	s.lastSeen[conv] = s.now
	e.pending++
	r := &request{conv: conv, prompt: t.prompt, out: t.out, arrived: arrived}
	r.done = func(now float64) {
		s.latencies = append(s.latencies, now-r.arrived)
		finished(time.Duration((now-r.arrived)*float64(time.Second)), r.out)
		done(now)
	}
	s.start(e, r)
}

func (s *sim) start(e *engine, r *request) {
	if e.busy() >= len(e.slots) {
		e.queue = append(e.queue, r)
		return
	}
	s.settle(e)
	s.waited += s.now - r.arrived
	started := s.now
	finish := r.done
	r.done = func(now float64) {
		s.engineSec += now - started
		finish(now)
	}
	e.serve(r, s.now)
	s.wake(e)
}

// settle brings e up to now and deals with whatever finished: its slot is
// freed, the next queued request takes it, and its caller is answered.
func (s *sim) settle(e *engine) {
	for _, r := range e.advance(s.now) {
		e.release(r.conv, s.now)
		if len(e.queue) > 0 {
			next := e.queue[0]
			e.queue = e.queue[1:]
			s.start(e, next)
		}
		r.done(s.now)
	}
}

// wake sets the next moment something on e changes. Every start and finish
// changes the rates of everything else running, so each one replaces the
// wake-up before it.
func (s *sim) wake(e *engine) {
	e.gen++
	gen := e.gen
	if t, ok := e.next(); ok {
		s.at(s.now+t, func(float64) {
			if gen != e.gen {
				return
			}
			s.settle(e)
			s.wake(e)
		})
	}
}

// run plays every task through workers that each take the next task when
// they finish one, as the agent harness does, and returns the wall clock.
func (s *sim) run(tasks []task, workers int) float64 {
	next := 0
	var work func(now float64)
	var step func(t task, i int)
	step = func(t task, i int) {
		if i == len(t.turns) {
			work(s.now)
			return
		}
		s.send(t.id, t.turns[i], func(now float64) {
			s.at(now+t.turns[i].pause, func(float64) { step(t, i+1) })
		})
	}
	work = func(float64) {
		if next >= len(tasks) {
			return
		}
		t := tasks[next]
		next++
		step(t, 0)
	}
	for range workers {
		s.at(0, work)
	}
	for s.q.Len() > 0 {
		ev := heap.Pop(&s.q).(event)
		s.now = ev.at
		if s.trace > 0 && s.now >= s.nextTrace {
			s.traceLine()
			s.nextTrace = s.now + s.trace
		}
		ev.fn(ev.at)
	}
	return s.now
}

// defaultFleet is the three machines of the 2026-10-05 runs as they ran that
// day: slots, the KV pool each engine reported, and the rates in the table at
// engine.rates. The RAM cache is set to nothing, because in those runs a
// conversation pushed out of its slot was read again in full whatever
// -cache-ram was given.
const defaultFleet = "helion:1:65536:256:22:1,minion:4:262144:530:62:1:1.4:0.65:6,xpredator:2:131072:1500:80:1:1.25:1:18"

const fleetUsage = "name:slots:pool_tokens:prefill_tok_s:decode_tok_s:cache_ram_mib[:pshare:dshare:dstarve]"

// parseFleet reads the engines. The last three fields say how an engine's
// requests slow each other and may be left off for one that serves a single
// request at a time: pshare is the total prefill throughput of two or more
// requests reading at once, relative to one alone; dshare is one request's
// decode rate beside another that is also writing, relative to alone; dstarve
// is its decode rate, in tokens a second, while another slot reads a prompt.
func parseFleet(spec string) ([]*engine, error) {
	var out []*engine
	for _, part := range strings.Split(spec, ",") {
		f := strings.Split(strings.TrimSpace(part), ":")
		if len(f) != 6 && len(f) != 9 {
			return nil, fmt.Errorf("engine %q: want %s", part, fleetUsage)
		}
		n := make([]float64, len(f)-1)
		for i, v := range f[1:] {
			x, err := strconv.ParseFloat(v, 64)
			if err != nil || x <= 0 {
				return nil, fmt.Errorf("engine %q: %q is not a positive number", part, v)
			}
			n[i] = x
		}
		e := &engine{
			name: f[0], slots: make([]slot, int(n[0])), pool: int(n[1]), prefill: n[2], decode: n[3],
			ramTokens: int(n[4] * 1024 / kibPerToken), pshare: 1, dshare: 1, dstarve: n[3],
		}
		if len(n) == 8 {
			e.pshare, e.dshare, e.dstarve = n[5], n[6], n[7]
		}
		out = append(out, e)
	}
	return out, nil
}

func pct(xs []float64, p float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	return xs[min(int(float64(len(xs))*p), len(xs)-1)]
}

func main() {
	runDir := flag.String("run", "", "a benchmark run directory holding */*.traj.json")
	policy := flag.String("policy", "router", "router (the router's own placement), least-busy, or random")
	fleet := flag.String("fleet", defaultFleet, "engines as "+fleetUsage+", comma separated")
	workers := flag.Int("workers", 8, "agent workers")
	model := flag.String("model", "qwen/qwen3.8-27b", "model name in the replayed requests")
	poll := flag.Float64("poll", 0, "how old the router's view of the engines is, seconds; 2 replays the router before it counted its own requests out again (runs recorded up to 2026-10-05)")
	nodes := flag.String("nodes", "b11153-9710a3217=xpredator,b1-7ceed87=minion,b1-a25c986=helion", "with -check: the engine builds in the recording and the node each was, build=node comma separated")
	seed := flag.Int64("seed", 1, "seed for the policies that leave ties to chance")
	holdGrace := flag.Float64("hold-grace", 1, "with -hold-max: how long a slot must have stayed free before a held request takes it, seconds")
	holdMax := flag.Float64("hold-max", 0, "hold a request whose engine is full at the router for up to this many seconds, 0 for never")
	trace := flag.Float64("trace", 0, "print each engine's conversations, load and cache hit every this many seconds")
	check := flag.Bool("check", false, "also print what the recorded run measured, to judge the model against")
	agreeFlag := flag.Bool("agree", false, "do not simulate: ask placement where it would send each recorded call, given the recorded state, and compare with where it went")
	homeSlot := flag.Bool("home-slot", false, "with -agree: use the earlier home-slot placement rule")
	noRoom := flag.Bool("no-room-rule", false, "with -agree: leave out the room step, which is not llm-d's")
	callsFile := flag.String("calls", "", "with -agree: a recorded replay as one call per line (task, sent, end, node, prompt, out, cached, body) in place of -run")
	flag.Parse()
	if *runDir == "" && *callsFile == "" {
		fmt.Fprintln(os.Stderr, "usage: routesim -run RUN_DIR [-policy router|least-busy|random] [-check]")
		os.Exit(2)
	}
	engines, err := parseFleet(*fleet)
	if err != nil {
		fmt.Fprintln(os.Stderr, "routesim:", err)
		os.Exit(2)
	}
	var tasks []task
	if *callsFile != "" {
		tasks, err = loadCalls(*callsFile)
		*nodes = "helion=helion,minion=minion,xpredator=xpredator"
	} else {
		tasks, err = loadRun(*runDir, *model)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "routesim:", err)
		os.Exit(1)
	}

	if *agreeFlag {
		debugAgree = *trace > 0
		agree(tasks, engines, *model, *nodes, *homeSlot, *noRoom)
		return
	}
	s := &sim{engines: engines, policy: *policy, model: *model, rng: rand.New(rand.NewSource(*seed)), home: map[string]string{}, lastSeen: map[string]float64{}, trace: *trace, holdGrace: *holdGrace, holdMax: *holdMax}
	if *policy == "router" || strings.HasPrefix(*policy, "x-") {
		s.poll = *poll
	}
	epoch := time.Unix(0, 0)
	s.place = router.NewPlacement(func() time.Time { return epoch.Add(time.Duration(s.now * float64(time.Second))) })
	if s.poll > 0 {
		s.at(0, s.pollAll)
	}
	wall := s.run(tasks, *workers)

	calls, recCached, recPrompt := 0, 0, 0
	for _, t := range tasks {
		for _, tn := range t.turns {
			calls++
			recCached += tn.recCached
			recPrompt += tn.prompt
		}
	}
	fmt.Printf("%s: %d tasks, %d calls, %d workers, policy %s\n\n", filepath.Base(*runDir), len(tasks), calls, *workers, *policy)
	fmt.Printf("  %-10s %8s %12s %12s %7s %10s\n", "engine", "calls", "read", "from cache", "hit", "generated")
	var read, cached int
	for _, e := range engines {
		hit := 0.0
		if e.prefilled+e.cached > 0 {
			hit = 100 * float64(e.cached) / float64(e.prefilled+e.cached)
		}
		fmt.Printf("  %-10s %8d %12d %12d %6.1f%% %10d\n", e.name, e.requests, e.prefilled, e.cached, hit, e.generated)
		read += e.prefilled
		cached += e.cached
	}
	sort.Float64s(s.latencies)
	fmt.Printf("\n  wall clock  %.0f min\n", wall/60)
	fmt.Printf("  read again  %d tokens (%.1f%% of all prompt tokens came from cache)\n", read, 100*float64(cached)/float64(read+cached))
	if s.holdMax > 0 {
		fmt.Printf("  held        %d of %d calls waited at the router for a slot\n", s.held, calls)
	}
	fmt.Printf("  moves       %d of %d calls went to a different engine than the conversation's last\n", s.migrations, calls)
	fmt.Printf("  call time   p50 %.0fs  p90 %.0fs  p99 %.0fs  max %.0fs\n", pct(s.latencies, .5), pct(s.latencies, .9), pct(s.latencies, .99), pct(s.latencies, 1))
	fmt.Printf("  worker time %.0f min being served, %.0f min queued for a slot\n", s.engineSec/60, s.waited/60)
	if *check {
		printRecorded(tasks, *nodes)
	}
}

// printRecorded prints what the recorded run measured, in the shape of the
// simulated summary above it. Replaying a run under the policy and fleet it was
// recorded with should land near these; how near is how far to trust the model
// on a policy that has not been run.
func printRecorded(tasks []task, nodes string) {
	name := map[string]string{}
	for _, kv := range strings.Split(nodes, ",") {
		if k, v, ok := strings.Cut(strings.TrimSpace(kv), "="); ok {
			name[k] = v
		}
	}
	type row struct{ calls, read, cached, out int }
	rows := map[string]*row{}
	var first, last, served, waited float64
	moves, read, cached := 0, 0, 0
	for _, t := range tasks {
		prev := ""
		for _, tn := range t.turns {
			n := tn.recEngine
			if v, ok := name[n]; ok {
				n = v
			}
			if rows[n] == nil {
				rows[n] = &row{}
			}
			r := rows[n]
			r.calls++
			r.read += tn.prompt - tn.recCached
			r.cached += tn.recCached
			r.out += tn.out
			read += tn.prompt - tn.recCached
			cached += tn.recCached
			if prev != "" && prev != n {
				moves++
			}
			prev = n
			served += tn.recSecs
			waited += tn.recWait
			if first == 0 || tn.recEnd-tn.recSecs < first {
				first = tn.recEnd - tn.recSecs
			}
			last = max(last, tn.recEnd)
		}
	}
	var names []string
	for n := range rows {
		names = append(names, n)
	}
	sort.Strings(names)
	fmt.Printf("\nas recorded\n\n")
	for _, n := range names {
		r := rows[n]
		fmt.Printf("  %-10s %8d %12d %12d %6.1f%% %10d\n", n, r.calls, r.read, r.cached, 100*float64(r.cached)/float64(r.read+r.cached), r.out)
	}
	fmt.Printf("\n  wall clock  %.0f min\n", (last-first)/60)
	fmt.Printf("  read again  %d tokens (%.1f%% of all prompt tokens came from cache)\n", read, 100*float64(cached)/float64(read+cached))
	fmt.Printf("  moves       %d\n", moves)
	fmt.Printf("  worker time %.0f min being served, %.0f min queued or on the wire\n", served/60, waited/60)
}
