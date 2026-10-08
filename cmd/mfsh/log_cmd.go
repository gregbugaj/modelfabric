package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/gregbugaj/modelfabric/internal/llmd"
)

// routerBypassed explains when llm-d bypasses this node's router, or returns "".
// Envoy dials engines directly, so those requests are absent from the router log.
func routerBypassed(addr string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var st llmd.Status
	if err := call(ctx, addr, http.MethodGet, "/api/v1/llmd", nil, &st); err != nil {
		return ""
	}
	if st.State != "running" || st.Model == "" {
		return ""
	}
	return fmt.Sprintf("llm-d is scheduling %s: Envoy dials the engines itself, so requests for that model "+
		"do not pass through this node's router and will not appear here. Other models still do, and "+
		"`mfsh log -engines` follows every engine instead.", st.Model)
}

func logCmd(args []string) error {
	if len(args) > 0 && args[0] == "engine" {
		return engineLogCmd(args[1:])
	}
	fs := flag.NewFlagSet("log", flag.ExitOnError)
	addr := fs.String("addr", defaultAddr, "address of the ModelFabric node")
	backlog := fs.Bool("backlog", true, "replay recent requests before following")
	engines := fs.Bool("engines", false, "follow what every engine in the mesh is doing instead of this node's requests")
	asJSON := fs.Bool("json", false, "one JSON object per line, for recording a stream alongside a benchmark run")
	tokens := fs.Bool("tokens", false, "follow replies as they are generated, on this node only")
	text := fs.Bool("text", false, "with -tokens, print the generated text instead of counting it")
	dev := fs.Bool("dev", false, "the developer log: each request as it arrives, where it is sent and why, the engine's progress, and how it ends")
	debug := fs.Bool("debug", false, "with -dev, include every line the engines write and the router's per-attempt detail")
	node := fs.String("node", "", "with -dev, follow another node's developer log by name, or `all` for every node merged into one stream")
	fs.Usage = func() { fmt.Fprint(fs.Output(), logUsage); fs.PrintDefaults() }
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := ensureNode(*addr); err != nil {
		return err
	}
	if *dev {
		return devLogCmd(*addr, *node, *debug, *backlog, *asJSON)
	}
	if *debug || *node != "" {
		return fmt.Errorf("-debug and -node only apply with -dev")
	}
	if *engines {
		return engineActivityCmd(*addr, *asJSON)
	}
	if *tokens {
		return tokenStreamCmd(*addr, *text, *asJSON)
	}
	if *text {
		return fmt.Errorf("-text only applies with -tokens")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	u := strings.TrimRight(*addr, "/") + "/z/log/stream"
	if *backlog {
		u += "?backlog=1"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	// Anything but 200 is not a stream: the scanner would simply find no
	// "data:" lines and the command would sit there looking like a quiet node.
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return fmt.Errorf("stream refused: %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	fmt.Fprintln(os.Stderr, dim("Streaming routed requests. Metadata always; prompts and replies only while capture is on. Ctrl-C to stop."))
	if why := routerBypassed(*addr); why != "" {
		fmt.Fprintln(os.Stderr, dim(why))
	}

	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var e struct {
			Time     time.Time `json:"time"`
			Path     string    `json:"path"`
			Model    string    `json:"model"`
			Node     string    `json:"node"`
			Engine   string    `json:"engine"`
			Local    bool      `json:"local"`
			Status   int       `json:"status"`
			Millis   int64     `json:"ms"`
			BytesOut int64     `json:"bytes_out"`
			Affine   bool      `json:"affine"`
			Trace    string    `json:"trace"`
			Error    string    `json:"error"`
		}
		if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &e) != nil {
			continue
		}
		status := green(fmt.Sprint(e.Status))
		if e.Status >= 400 {
			status = red(fmt.Sprint(e.Status))
		}
		where := e.Node + "/" + e.Engine
		if e.Engine == "" {
			where = "—"
		}
		tags := ""
		if !e.Local && e.Engine != "" {
			tags += cyan(" remote")
		}
		if e.Affine {
			tags += dim(" cache-affine")
		}
		// The first half of the trace is enough to pair two lines by eye, and
		// the full id is in the dashboard and the response header.
		if len(e.Trace) >= 8 {
			tags += dim(" " + e.Trace[:8])
		}
		fmt.Printf("%s  %s  %-26s %-22s %s  %6dms  %8s%s",
			dim(e.Time.Local().Format("15:04:05")), status, e.Path, e.Model, dim(where),
			e.Millis, humanBytes(e.BytesOut), tags)
		if e.Error != "" {
			fmt.Printf("  %s", red(e.Error))
		}
		fmt.Println()
	}
	if ctx.Err() != nil {
		return nil
	}
	return sc.Err()
}

// logUsage leads with the five logs, because the flags alone do not say that
// they are five different things. Someone looking for the dashboard's
// Developer log reached for `mfsh log engine -f`, which is one engine's file.
const logUsage = `Usage: mfsh log [flags]            follow a log of this node or the mesh
       mfsh log engine [instance]  print one engine's own log file

Which log:
  mfsh log                    one line per finished request: status, model, where it ran, how long
  mfsh log -dev               the developer log, as on the dashboard: each request as it arrives,
                              where it is sent and why, the engine's progress, how it ends
  mfsh log -engines           every engine in the mesh, refreshed: read and write rates, slots, cache
  mfsh log -tokens            replies as they are generated, on this node
  mfsh log engine [instance]  an engine's raw output on this machine  [-f follow, -n N lines]

The developer log:
  mfsh log -dev -node all            every node, merged into one stream
  mfsh log -dev -node all -debug     the same, with every line the engines write
  mfsh log -dev -node NAME           one other node
  mfsh log -dev -backlog=false       only what happens from now on
  mfsh log -dev -node all -json      one JSON object per line, for jq or a file
  mfsh log -dev -node all | grep 906ebc    one request across nodes, by its id

Flags:
`

func engineLogCmd(args []string) error {
	fs := flag.NewFlagSet("log engine", flag.ExitOnError)
	addr := fs.String("addr", defaultAddr, "address of the ModelFabric node")
	follow := fs.Bool("f", false, "follow the log")
	lines := fs.Int("n", 40, "lines to show")
	fs.Usage = func() {
		fmt.Fprint(fs.Output(), "Usage: mfsh log engine [instance] [-f] [-n N]\n\n"+
			"Prints an engine's own output, from its log file on this machine. With one engine\n"+
			"loaded the instance can be left out. For requests across the mesh, with the engines'\n"+
			"lines beside them, use `mfsh log -dev -node all -debug`.\n\nFlags:\n")
		fs.PrintDefaults()
	}
	positional, err := parsePositional(fs, args)
	if err != nil {
		return err
	}

	id := ""
	if len(positional) == 1 {
		id = positional[0]
	} else {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		picked, _, err := pickModel(ctx, *addr, nil, true)
		cancel()
		if err != nil {
			return err
		}
		id = picked
	}
	path := filepath.Join(logDir(), filepath.Base(id)+".log")
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("no engine log for %s (%v)", id, err)
	}
	defer f.Close()

	if *lines < 0 {
		// A negative count reached lastLines, where all[len(all)-n:] slices
		// past the end and panics on what the user typed.
		return fmt.Errorf("-n must not be negative (got %d)", *lines)
	}
	tail, err := lastLines(f, *lines)
	if err != nil {
		return err
	}
	fmt.Print(tail)
	if !*follow {
		return nil
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	buf := make([]byte, 32<<10)
	for ctx.Err() == nil {
		n, err := f.Read(buf)
		if n > 0 {
			// `mfsh logs -f | head` closes the pipe; without this the loop
			// polled the file until interrupted, writing into nothing.
			if _, werr := os.Stdout.Write(buf[:n]); werr != nil {
				return nil
			}
		}
		if err == io.EOF {
			time.Sleep(250 * time.Millisecond)
			continue
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// lastLines returns the final n lines of f and leaves the offset at the end.
func lastLines(f *os.File, n int) (string, error) {
	info, err := f.Stat()
	if err != nil {
		return "", err
	}
	// The window grows with n so `-n 5000` is not quietly answered with
	// whatever fit in 256 KiB. Still bounded: a log can be enormous.
	window := int64(256 << 10)
	if want := int64(n) * 512; want > window {
		window = min(want, 64<<20)
	}
	start := info.Size() - window
	if start < 0 {
		start = 0
	}
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return "", err
	}
	b, err := io.ReadAll(f)
	if err != nil {
		return "", err
	}
	all := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	if len(all) > n {
		all = all[len(all)-n:]
	}
	if len(all) == 1 && all[0] == "" {
		return "", nil
	}
	return strings.Join(all, "\n") + "\n", nil
}

// devLogCmd follows a node's developer log. Unlike the request log, which has
// one line per request written when it ends, this prints a request's arrival,
// its placement with the reason, the engine's progress on it, and its end, as
// each happens.
// devEntry is a developer log entry as the terminal needs it. raw is the
// entry as the node sent it, for -json.
type devEntry struct {
	Time      time.Time `json:"time"`
	Level     string    `json:"level"`
	Source    string    `json:"source"`
	Node      string    `json:"node"`
	Trace     string    `json:"trace"`
	Model     string    `json:"model"`
	Engine    string    `json:"engine"`
	Msg       string    `json:"msg"`
	Body      string    `json:"body"`
	Truncated bool      `json:"truncated"`
	raw       string
}

// allNodes is what -node takes to mean every node, merged. A machine named
// "all" can still be followed through its own -addr.
const allNodes = "all"

func devLogCmd(addr, node string, debug, backlog, asJSON bool) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	level := "info"
	if debug {
		level = "debug"
	}
	show := func(e devEntry, withNode bool) {
		if asJSON {
			fmt.Println(e.raw)
			return
		}
		name := ""
		if withNode {
			name = e.Node
		}
		fmt.Println(devLogLine(e.Time, e.Level, e.Source, name, e.Model, e.Trace, e.Msg))
		if e.Body != "" {
			fmt.Println(devLogBody(e.Body, e.Truncated))
		}
	}
	const about = "Sizes and counts always; prompts and replies only while capture is on. -debug adds the engines' own lines. Ctrl-C to stop."
	if node != allNodes {
		if !asJSON {
			fmt.Fprintln(os.Stderr, dim("Developer log. "+about))
		}
		err := devLogStream(ctx, addr, node, level, backlog, func(e devEntry) { show(e, false) })
		if ctx.Err() != nil {
			return nil
		}
		return err
	}

	self, names, err := meshNodeNames(ctx, addr)
	if err != nil {
		return err
	}
	if !asJSON {
		fmt.Fprintln(os.Stderr, dim(fmt.Sprintf("Developer log of %d nodes, merged: %s. %s", len(names), strings.Join(names, ", "), about)))
	}
	in := make(chan devEntry, 1024)
	for _, name := range names {
		go func() {
			peer := name
			if name == self {
				peer = ""
			}
			first, warned := true, false
			for ctx.Err() == nil {
				// Only the first connection replays what the node holds: a
				// reconnect that did would print every line a second time.
				err := devLogStream(ctx, addr, peer, level, backlog && first, func(e devEntry) {
					if e.Node == "" {
						e.Node = name
					}
					select {
					case in <- e:
					case <-ctx.Done():
					}
				})
				first = false
				if ctx.Err() != nil {
					return
				}
				// A node that is down, restarting or on an older build is said
				// once, and the others keep going. It is picked up again when
				// it answers.
				if !warned {
					why := "its stream ended"
					if err != nil {
						why = err.Error()
					}
					fmt.Fprintln(os.Stderr, yellow(fmt.Sprintf("%s: %s; trying again every 5s", name, why)))
					warned = true
				}
				select {
				case <-time.After(5 * time.Second):
				case <-ctx.Done():
				}
			}
		}()
	}
	// Each node's lines arrive in order but the nodes do not arrive together,
	// so entries are held briefly and printed by their own time. The first
	// hold is longer: every node sends what it already holds at once.
	var held []devEntry
	tick := time.NewTimer(1500 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case e := <-in:
			held = append(held, e)
		case <-tick.C:
			for _, e := range devLogOrder(held) {
				show(e, true)
			}
			held = held[:0]
			tick.Reset(250 * time.Millisecond)
		case <-ctx.Done():
			return nil
		}
	}
}

// devLogOrder sorts entries from several nodes by their own time. Entries
// from one node keep the order that node gave them, since two can carry the
// same instant.
func devLogOrder(es []devEntry) []devEntry {
	sort.SliceStable(es, func(i, j int) bool { return es[i].Time.Before(es[j].Time) })
	return es
}

// meshNodeNames is this node's name and every node it knows, itself first.
func meshNodeNames(ctx context.Context, addr string) (string, []string, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var mesh struct {
		Self  meshNodeView   `json:"self"`
		Peers []meshNodeView `json:"peers"`
	}
	if err := call(ctx, addr, http.MethodGet, "/z/mesh", nil, &mesh); err != nil {
		return "", nil, err
	}
	names := []string{mesh.Self.Node}
	for _, p := range mesh.Peers {
		if p.Node != "" {
			names = append(names, p.Node)
		}
	}
	return mesh.Self.Node, names, nil
}

// devLogStream follows one node's developer log, handing each entry to emit,
// until the stream ends or ctx does. node "" is the node at addr.
func devLogStream(ctx context.Context, addr, node, level string, backlog bool, emit func(devEntry)) error {
	path := "/api/v1/devlog/stream"
	if node != "" {
		path = "/api/v1/nodes/" + node + "/devlog/stream"
	}
	u := strings.TrimRight(addr, "/") + path + "?level=" + level
	if backlog {
		u += "&backlog=1"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		if resp.StatusCode == http.StatusNotFound {
			return fmt.Errorf("no developer log: the node is running a build from before the feature; restart it on the current one")
		}
		return fmt.Errorf("stream refused: %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	sc := bufio.NewScanner(resp.Body)
	// A captured body can be the capture cap long, escaped.
	sc.Buffer(make([]byte, 64<<10), 8<<20)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		e := devEntry{raw: strings.TrimPrefix(line, "data: ")}
		if json.Unmarshal([]byte(e.raw), &e) != nil {
			continue
		}
		emit(e)
	}
	return sc.Err()
}

// devLogLine is one entry as the terminal shows it:
//
//	14:02:11 INFO  qwen/qwen3.8-27b  a1b2c3  sent to minion: it holds 94% of this prompt
//
// node is the machine that wrote it, shown when several are merged.
// The trace is cut to six characters: enough to follow one request down the
// screen, and the full id is in -json.
func devLogLine(at time.Time, level, source, node, model, trace, msg string) string {
	tag := fmt.Sprintf("%-5s", strings.ToUpper(level))
	switch level {
	case "error":
		tag = red(tag)
	case "warn":
		tag = yellow(tag)
	case "debug":
		tag = dim(tag)
	default:
		tag = green(tag)
	}
	if level == "debug" {
		msg = dim(msg)
	}
	parts := []string{dim(at.Local().Format("15:04:05")), tag}
	if node != "" {
		parts = append(parts, bold(fmt.Sprintf("%-9s", node)))
	}
	if model != "" {
		parts = append(parts, cyan(model))
	}
	if len(trace) > 6 {
		trace = trace[:6]
	}
	if trace != "" {
		parts = append(parts, dim(trace))
	} else if source == "engine" {
		parts = append(parts, dim("engine"))
	}
	return strings.Join(append(parts, msg), "  ")
}

// devLogBody is a captured body, indented under its entry, as indented JSON
// where it is JSON.
func devLogBody(body string, truncated bool) string {
	var pretty strings.Builder
	var v any
	if !truncated && json.Unmarshal([]byte(body), &v) == nil {
		if b, err := json.MarshalIndent(v, "    ", "  "); err == nil {
			body = string(b)
		}
	}
	for _, l := range strings.Split(body, "\n") {
		pretty.WriteString("    " + dim(l) + "\n")
	}
	out := strings.TrimRight(pretty.String(), "\n")
	if truncated {
		out += "\n    " + dim("... (cut at the capture limit)")
	}
	return out
}
