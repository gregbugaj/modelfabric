// Package engineshim proxies inference to an engine, applies output defaults
// and disk-cache slot placement, and publishes synthesized KV metrics.
//
// llama.cpp reports no KV gauge on /metrics — measured on b11026 and b11040,
// whose whole set is token and request counters plus n_busy_slots_per_decode.
// The number is not missing, only differently shaped: /slots reports each
// slot's capacity and the prompt resident in it. ModelFabric computes utilization
// from that.
//
// What it cannot do is hand it to llm-d. The EPP scrapes each endpoint at the
// address and port it routes to (measured: a per-endpoint metricsPort is
// ignored, and a port on the metrics data source stops scraping altogether), so
// the metric has to come from the thing llm-d dials. Hence this: a listener
// that answers /metrics with the engine's own metrics plus the synthesized
// gauge, on the same port that proxies inference.
//
// llm-d dials the shim, and ModelFabric's router uses it when disk prompt cache
// placement is enabled. Inference bodies may be rewritten to add an output
// limit or select a slot; they are not always passed through unchanged.
package engineshim

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/gregbugaj/modelfabric/internal/prefixchain"
	"github.com/gregbugaj/modelfabric/internal/tokentap"
)

// KVUsageMetric is the gauge this package adds, named after llama.cpp's own
// metrics so an operator reading /metrics sees one consistent set.
const KVUsageMetric = "llamacpp:kv_cache_usage_ratio"

// Slot is one of llama.cpp's parallel slots, as /slots reports it.
//
// NPromptTokens is what the slot's cache holds. It is reported while a request
// is being served and stays afterwards, because llama.cpp keeps a finished
// slot's KV so the next request sharing that prefix skips prefill. IsProcessing
// is not a usable gate on it: measured on b11026, it is true during prefill and
// false while the model is generating, with the tokens resident throughout.
type Slot struct {
	ID            int  `json:"id"`
	NCtx          int  `json:"n_ctx"`
	IsProcessing  bool `json:"is_processing"`
	NPromptTokens int  `json:"n_prompt_tokens"`
}

// BusySlots counts the slots serving a request, which is how many requests the
// engine has in flight.
//
// ModelFabric's own counter only sees requests its router dispatched, and under
// llm-d nothing goes that way: Envoy dials the engine directly, so the
// dashboard showed 0 in flight on an engine whose KV cache was visibly
// climbing. The engine is the only thing that knows, exactly as it is for KV.
//
// Measured on 2.40.0 with a 700-token generation: is_processing stays true
// throughout, matching llamacpp:requests_processing. An older note here said
// it goes false while generating, which was about whether it gates
// n_prompt_tokens; it does not hold as a description of the flag on this
// build, and requests in flight is what this counts.
func BusySlots(slots []Slot) (int, bool) {
	if len(slots) == 0 {
		return 0, false
	}
	n := 0
	for _, s := range slots {
		if s.IsProcessing {
			n++
		}
	}
	return n, true
}

// KVUsage is the share of the KV pool a llama.cpp engine currently holds: the
// tokens resident in its slots over the capacity of those slots.
//
// It counts caches kept for reuse, not just requests in flight, because that is
// what occupies the pool — a warm engine with idle slots is not an empty one,
// and the prefix those slots hold is exactly what routing tries to reuse. What
// it cannot count is tokens generated after the prompt: /slots reports the
// prompt a slot holds and nothing about the completion, so a long generation
// reads lower than the memory it really occupies.
func KVUsage(slots []Slot) (float64, bool) {
	if len(slots) == 0 {
		return 0, false
	}
	pool, held := 0, 0
	for _, s := range slots {
		if s.NCtx <= 0 {
			return 0, false // an engine that does not say cannot be guessed at
		}
		pool += s.NCtx
		held += s.NPromptTokens
	}
	if pool <= 0 {
		return 0, false
	}
	return min(float64(held)/float64(pool), 1), true
}

// Augment appends the gauge to an engine's metrics exposition. A metric the
// engine already exports is left alone: if llama.cpp ever publishes this
// itself, its own number wins and this becomes a no-op.
func Augment(metrics []byte, ratio float64, ok bool) []byte {
	if !ok || hasSample(metrics, KVUsageMetric) {
		return metrics
	}
	out := make([]byte, 0, len(metrics)+160)
	out = append(out, metrics...)
	if len(out) > 0 && out[len(out)-1] != '\n' {
		out = append(out, '\n')
	}
	out = append(out, "# HELP "+KVUsageMetric+" KV cache tokens resident in the engine's slots, over their capacity; synthesized by ModelFabric from /slots\n"...)
	out = append(out, "# TYPE "+KVUsageMetric+" gauge\n"...)
	out = append(out, KVUsageMetric+" "+strconv.FormatFloat(ratio, 'f', 6, 64)+"\n"...)
	return out
}

// hasSample reports whether an exposition already carries a sample for name.
// A substring search was both too loose and too tight: it matched the metric's
// own "# HELP" line, so a HELP without a sample suppressed the gauge, and it
// missed a labelled sample ("name{slot=\"0\"} 1") or one separated by a tab.
func hasSample(metrics []byte, name string) bool {
	for _, line := range strings.Split(string(metrics), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if !strings.HasPrefix(line, name) {
			continue
		}
		switch rest := line[len(name):]; {
		case rest == "":
			return true
		case rest[0] == ' ', rest[0] == '\t', rest[0] == '{':
			return true
		}
	}
	return false
}

// Shim fronts one engine.
type Shim struct {
	engine *url.URL
	log    *slog.Logger
	client *http.Client
	srv    *http.Server
	ln     net.Listener
	// tap, when set, carries the model's output to whoever is watching on this
	// machine. Under llm-d the shim is the one place the node that *runs* the
	// model is in the request path at all: Envoy dials it from another
	// machine, and without it a serving node sees nothing of the replies its
	// own GPU is writing.
	tap *tokentap.Tap
	// maxOutputTokens is filled into a generating request that states no limit
	// of its own; zero leaves it alone. Set by the supervisor from config.
	maxOutputTokens int
	// cache, when set, is the disk tier of the prompt cache, placing each
	// generating request in a slot. Here rather than in the router because the
	// shim is the one hop every routing method shares: ModelFabric's router
	// dials it when the cache is on, a peer's forwarded request arrives
	// through that router, and llm-d's Envoy dials it always. In the router,
	// requests llm-d scheduled bypassed the cache, which then distrusted every
	// slot they touched.
	cache atomic.Pointer[cacheHook]
}

// Placer is the disk tier of the prompt cache (internal/slotcache): it picks
// the slot a request runs in, saving or restoring that slot first as needed,
// and is told how the request ended (the engine's status, 0 for no answer).
type Placer interface {
	Place(ctx context.Context, engine string, chain [][32]byte) (slot int, done func(status int))
}

type cacheHook struct {
	name   string // the engine's instance id, as the cache knows it
	placer Placer
}

// SetCache attaches the disk prompt cache to this engine's shim; a nil placer
// detaches it. Safe while serving.
func (s *Shim) SetCache(name string, p Placer) {
	if p == nil {
		s.cache.Store(nil)
		return
	}
	s.cache.Store(&cacheHook{name: name, placer: p})
}

// Caching reports whether the disk prompt cache places this shim's requests.
func (s *Shim) Caching() bool { return s.cache.Load() != nil }

// place pins a generating request to the slot the cache chooses, and returns
// what to call with the engine's status when the response ends. A request
// that pins its own slot, or whose body cannot be read, is left to the engine.
func (s *Shim) place(r *http.Request) func(status int) {
	h := s.cache.Load()
	if h == nil || r.Body == nil {
		return func(int) {}
	}
	body, err := io.ReadAll(r.Body)
	_ = r.Body.Close()
	if err != nil {
		r.Body = io.NopCloser(bytes.NewReader(body))
		return func(int) {}
	}
	var probe struct {
		Model  string `json:"model"`
		IDSlot *int   `json:"id_slot"`
	}
	done := func(int) {}
	if json.Unmarshal(body, &probe) == nil && probe.IDSlot == nil {
		// The router's own chain, so a conversation hashes the same whichever
		// way it arrived.
		chain := prefixchain.Chain(probe.Model, body, prefixchain.ColdMaxPrefix)
		var slot int
		if slot, done = h.placer.Place(r.Context(), h.name, chain); slot >= 0 {
			body = prefixchain.WithSlot(body, slot)
		}
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	r.ContentLength = int64(len(body))
	r.Header.Set("Content-Length", strconv.Itoa(len(body)))
	return done
}

// statusWriter records the status the engine answered with, for the cache.
// Flush and Unwrap keep a streamed reply streaming through it.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// SetMaxOutputTokens sets the ceiling filled into requests that name none.
func (s *Shim) SetMaxOutputTokens(n int) { s.maxOutputTokens = n }

// MaxOutputTokens is the ceiling it fills in, 0 for none.
func (s *Shim) MaxOutputTokens() int { return s.maxOutputTokens }

// Watching reports whether the shim carries replies to the live token view.
func (s *Shim) Watching() bool { return s.tap != nil }

// Watch makes the shim publish the replies it carries. nil disables it.
func (s *Shim) Watch(t *tokentap.Tap) { s.tap = t }

// New returns a shim for the engine at base ("http://127.0.0.1:18000").
// peekModel reads the model out of a request body and puts the body back, so
// a watcher can say which model is writing. A body it cannot read is not an
// error here: the reply still proxies, it is just labelled less.
func peekModel(r *http.Request) string {
	if r.Body == nil || r.ContentLength <= 0 || r.ContentLength > 1<<20 {
		return ""
	}
	buf, err := io.ReadAll(io.LimitReader(r.Body, r.ContentLength))
	if err != nil {
		return ""
	}
	_ = r.Body.Close()
	r.Body = io.NopCloser(bytes.NewReader(buf))
	var probe struct {
		Model string `json:"model"`
	}
	if json.Unmarshal(buf, &probe) != nil {
		return ""
	}
	return probe.Model
}

func New(base string, log *slog.Logger) (*Shim, error) {
	u, err := url.Parse(base)
	if err != nil {
		return nil, fmt.Errorf("engine address %q: %w", base, err)
	}
	// url.Parse accepts plenty that is not an origin: "127.0.0.1:18000" parses
	// with scheme "127.0.0.1", and an empty host parses fine. Both used to be
	// accepted here and fail later, during a scrape, as a mystery.
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("engine address %q needs an http or https scheme", base)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("engine address %q has no host", base)
	}
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Shim{engine: u, log: log, client: &http.Client{Timeout: 3 * time.Second}}, nil
}

// Listen starts the shim on addr, returning the port it bound.
func (s *Shim) Listen(addr string) (int, error) {
	// Listening twice used to overwrite s.srv/s.ln, leaving the first server
	// running with nothing able to shut it down.
	if s.srv != nil {
		return 0, fmt.Errorf("shim for %s is already listening on %s", s.engine, s.ln.Addr())
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return 0, err
	}
	proxy := httputil.NewSingleHostReverseProxy(s.engine)
	// Inference streams token by token; buffering it would add latency to
	// every request llm-d schedules.
	proxy.FlushInterval = -1
	mux := http.NewServeMux()
	mux.HandleFunc("GET /metrics", s.handleMetrics)
	mux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// An output ceiling for a request that states none. Here rather than
		// only in the router because the router is not always in the path
		// (llm-d dials this shim directly) and this is the last hop before
		// the engine (see ceiling.go).
		if r.Method == http.MethodPost && generates(r.URL.Path) {
			if capOutput(r, s.maxOutputTokens) {
				s.log.Debug("filled in an output ceiling for a request that named none",
					"engine", s.engine.String(), "max_tokens", s.maxOutputTokens)
			}
			// After the ceiling, so the slot is chosen for the body the engine
			// will actually see.
			if s.Caching() {
				done := s.place(r)
				sw := &statusWriter{ResponseWriter: w}
				w = sw
				defer func() { done(sw.status) }()
			}
		}
		// Costs one atomic read when nobody is watching, which is the normal
		// case: a request llm-d schedules must not pay for an idle tap.
		if s.tap != nil && s.tap.Active() && r.Method == http.MethodPost &&
			strings.HasSuffix(r.URL.Path, "/chat/completions") {
			tw, done := s.tap.Wrap(w, r.Header.Get("X-Fabric-Trace"), peekModel(r))
			defer done()
			w = tw
		}
		proxy.ServeHTTP(w, r)
	}))
	s.ln = ln
	s.srv = &http.Server{Handler: mux}
	go func() {
		if err := s.srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			s.log.Warn("engine metrics shim stopped", "engine", s.engine.String(), "err", err)
		}
	}()
	return ln.Addr().(*net.TCPAddr).Port, nil
}

// Close stops the shim.
func (s *Shim) Close() error {
	if s.srv == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return s.srv.Shutdown(ctx)
}

func (s *Shim) handleMetrics(w http.ResponseWriter, r *http.Request) {
	body, ct, err := s.get(r.Context(), "/metrics")
	if err != nil {
		// Without the engine's own metrics there is nothing to publish: the
		// scraper must see the engine as unreachable, not as idle.
		http.Error(w, "engine metrics unavailable: "+err.Error(), http.StatusBadGateway)
		return
	}
	ratio, ok := s.kvUsage(r.Context())
	if ct == "" {
		ct = "text/plain; version=0.0.4"
	}
	w.Header().Set("Content-Type", ct)
	_, _ = w.Write(Augment(body, ratio, ok))
}

// kvUsage reads /slots. A failure is reported as "no value" rather than zero,
// so a scheduler never reads a broken probe as an empty cache.
func (s *Shim) kvUsage(ctx context.Context) (float64, bool) {
	body, _, err := s.get(ctx, "/slots")
	if err != nil {
		return 0, false
	}
	var slots []Slot
	if err := json.Unmarshal(body, &slots); err != nil {
		return 0, false
	}
	return KVUsage(slots)
}

func (s *Shim) get(ctx context.Context, path string) ([]byte, string, error) {
	// JoinPath, not concatenation: a base with a trailing slash, a path or a
	// query turned "…?x=y" + "/metrics" into a request for "…?x=y/metrics".
	target := s.engine.JoinPath(path)
	target.RawQuery, target.Fragment = "", ""
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return nil, "", err
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("GET %s: %s", path, resp.Status)
	}
	// One byte past the cap tells truncation from a body that merely fits.
	// Returning the first 4 MiB as if it were the whole thing published
	// invalid Prometheus text, and made an oversized /slots look like "no
	// gauge" for no visible reason.
	const cap = 4 << 20
	b, err := io.ReadAll(io.LimitReader(resp.Body, cap+1))
	if err != nil {
		return nil, "", err
	}
	if len(b) > cap {
		return nil, "", fmt.Errorf("GET %s: response larger than %d bytes", path, cap)
	}
	return b, resp.Header.Get("Content-Type"), nil
}
