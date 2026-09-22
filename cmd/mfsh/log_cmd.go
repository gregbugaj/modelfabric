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
	"strings"
	"syscall"
	"time"

	"github.com/gregbugaj/modelfabric/internal/llmd"
)

// routerBypassed explains why this node's router may see nothing, or "" when it
// is in the path.
//
// Only llm-d does that now: Envoy dials the engines itself, so a request it
// schedules never passes through ModelFabric's router and cannot appear in this log.
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

// logCmd streams routed requests, like `lms log stream`, or shows an engine's
// own output with `mfsh log engine`.
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
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := ensureNode(*addr); err != nil {
		return err
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
	// A node whose router is not in the path has nothing to stream, and the
	// banner above used to promise requests and then sit silent for hours: a
	// benchmark ran on three engines while this printed one line and waited.
	// Whether ModelFabric sees a request depends on whether llm-d owns the model, so say so
	// here rather than leave "quiet" and "blind" looking identical.
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

// engineLogCmd prints (and optionally follows) an instance's engine output.
// That output is where load timings, VRAM decisions and engine warnings live —
// it is what made the load-time regression diagnosable.
func engineLogCmd(args []string) error {
	fs := flag.NewFlagSet("log engine", flag.ExitOnError)
	addr := fs.String("addr", defaultAddr, "address of the ModelFabric node")
	follow := fs.Bool("f", false, "follow the log")
	lines := fs.Int("n", 40, "lines to show")
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
