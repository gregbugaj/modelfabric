package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gregbugaj/modelfabric/internal/nodekey"
	"github.com/gregbugaj/modelfabric/internal/router"
)

const chatHelp = `  /model              pick a model from the mesh
  /node               pin to one node, or back to the whole mesh
  /system [text]      set the system prompt (no text clears it)
  /reasoning on|off   show the model's thinking as it arrives
  drop a file         attach an image — drag it onto the terminal
  /image <path>       the same, typed (Tab completes paths)
  /stats              routing and timing for the last reply, in full
  /clear              forget the conversation so far
  /help               this
  /exit               leave (Ctrl-D does too)`

// chatCommands is what Tab and the suggestion list offer. Kept next to
// chatHelp so the two cannot drift.
func chatCommands() []completion {
	return []completion{
		{Value: "/model", Label: "/model", Note: "pick a model from the mesh"},
		{Value: "/node", Label: "/node", Note: "pin to one node, or back to the mesh"},
		{Value: "/image", Label: "/image", Note: "attach an image to the next message"},
		{Value: "/system", Label: "/system", Note: "set the system prompt"},
		{Value: "/reasoning", Label: "/reasoning", Note: "show the model's thinking"},
		{Value: "/stats", Label: "/stats", Note: "routing and timing for the last reply"},
		{Value: "/clear", Label: "/clear", Note: "forget the conversation"},
		{Value: "/help", Label: "/help", Note: "the commands"},
		{Value: "/exit", Label: "/exit", Note: "leave"},
	}
}

type chatTurn struct {
	at                       time.Time
	node, engine, via, trace string
	ttft                     time.Duration // to the first token
	total                    time.Duration
	reasoning, content       int // tokens, counted as text-carrying chunks
	promptTokens             int
	completionTokens         int
	cachedTokens             int
	// contextLen is the serving engine's per-conversation token limit; contextNote
	// records a mismatch with its loaded settings. Zero means unknown (for example,
	// mlx-lm has no /props), so headroom is omitted.
	contextLen  int
	contextNote string
}

type chatSession struct {
	addr      string
	model     string
	system    string
	pinned    string // a node's base URL, empty for the whole mesh
	pinnedTo  string // its name, for the prompt
	key       string
	pending   []attachment // images to send with the next message
	showThink bool
	msgs      []map[string]any
	last      *chatTurn
	client    *http.Client
}

func chatCmd(args []string) error {
	fs := flag.NewFlagSet("chat", flag.ExitOnError)
	addr := fs.String("addr", defaultAddr, "address of the ModelFabric node")
	model := fs.String("model", "", "model to talk to (default: the only one loaded, or pick)")
	system := fs.String("system", "", "system prompt")
	think := fs.Bool("reasoning", false, "show the model's thinking as it arrives")
	key := fs.String("key", "", "API key, when the node requires one (default: this machine's, if readable)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := ensureNode(*addr); err != nil {
		return err
	}
	// No timeout: a long reply on a slow engine can take minutes.
	s := &chatSession{
		addr: *addr, model: *model, system: *system, showThink: *think,
		key: *key, client: &http.Client{},
	}
	// Use the local configured key by default; nodes without authentication ignore it.
	if s.key == "" {
		if k, err := nodekey.Key(fabricHome()); err == nil {
			s.key = k
		}
	}
	if rest := fs.Args(); len(rest) > 0 && s.model == "" {
		s.model = rest[0]
	}
	if s.model == "" {
		m, err := s.pickModel()
		if err != nil {
			return err
		}
		s.model = m
	}
	return s.run()
}

func (s *chatSession) pickModel() (string, error) {
	models, err := s.models()
	if err != nil {
		return "", err
	}
	if len(models) == 0 {
		return "", fmt.Errorf("no models are loaded anywhere in the mesh; load one with `mfsh load <model>`")
	}
	if len(models) == 1 {
		return models[0].Value, nil
	}
	return selectOne("Which model?", models)
}

func (s *chatSession) models() ([]Choice, error) {
	var out struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := call(context.Background(), s.addr, http.MethodGet, "/v1/models", nil, &out); err != nil {
		return nil, err
	}
	var cs []Choice
	for _, m := range out.Data {
		cs = append(cs, Choice{Value: m.ID, Label: m.ID})
	}
	return cs, nil
}

func (s *chatSession) run() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	fmt.Println()
	fmt.Println("  " + bold("mfsh chat") + dim("  — a conversation against the mesh"))
	fmt.Println("  " + dim("model ") + s.model + dim("   via ") + s.addr)
	fmt.Println()
	fmt.Println(dim(chatHelp))
	fmt.Println()
	fmt.Println(dim("  Every reply says which node and engine answered. That is the point of this"))
	fmt.Println(dim("  rather than a chat client."))
	fmt.Println()

	ed := newLineEditor(completeChat(chatCommands()))
	for {
		raw, ok := ed.readLine(s.prompt())
		if !ok {
			fmt.Println()
			return nil // Ctrl-D
		}
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		// Check existing image paths before slash commands: dropped absolute paths
		// begin with a slash and would otherwise be rejected as unknown commands.
		if paths, rest := dropped(line); len(paths) > 0 {
			for _, p := range paths {
				if err := s.attach(p); err != nil {
					fmt.Println("  " + red(err.Error()))
				}
			}
			if rest == "" {
				continue
			}
			line = rest
		} else if strings.HasPrefix(line, "/") {
			done, err := s.command(line)
			if err != nil {
				fmt.Println("  " + red(err.Error()))
			}
			if done {
				return nil
			}
			continue
		}
		if err := s.say(ctx, line); err != nil {
			if ctx.Err() != nil {
				fmt.Println()
				return nil
			}
			fmt.Println("  " + red(err.Error()))
		}
	}
}

func (s *chatSession) prompt() string {
	where := ""
	if s.pinnedTo != "" {
		where = dim("@" + s.pinnedTo)
	}
	return cyan("› ") + where
}

// command runs a slash command; the bool says whether to leave.
func (s *chatSession) command(line string) (bool, error) {
	cmd, rest, _ := strings.Cut(line, " ")
	rest = strings.TrimSpace(rest)
	switch cmd {
	case "/exit", "/quit":
		return true, nil
	case "/help":
		fmt.Println(dim(chatHelp))
	case "/clear":
		s.msgs = nil
		fmt.Println("  " + dim("conversation cleared; the engines' caches still hold the old prefix"))
	case "/system":
		s.system = rest
		if rest == "" {
			fmt.Println("  " + dim("system prompt cleared"))
		} else {
			fmt.Println("  " + dim("system prompt set — it takes effect on the next message"))
		}
	case "/reasoning":
		switch rest {
		case "on":
			s.showThink = true
		case "off":
			s.showThink = false
		default:
			return false, fmt.Errorf("/reasoning on|off (currently %s)", onOff(s.showThink))
		}
		fmt.Println("  " + dim("thinking "+onOff(s.showThink)))
	case "/model":
		m, err := s.pickModel()
		if err != nil {
			return false, err
		}
		if m != s.model {
			s.model, s.msgs = m, nil
			fmt.Println("  " + dim("model is now "+m+"; conversation cleared"))
		} else {
			fmt.Println("  " + dim("still "+m+" — the only model the mesh serves"))
		}
	case "/node":
		return false, s.pickNode()
	case "/image":
		return false, s.attach(rest)
	case "/stats":
		s.printStats()
	default:
		return false, fmt.Errorf("no such command: %s", cmd)
	}
	return false, nil
}

// pickNode pins the conversation to one node to isolate routing from engine failures.
func (s *chatSession) pickNode() error {
	// Pinning needs node addresses, which meshNodeView does not include.
	type chatNode struct {
		Node      string `json:"node"`
		Addr      string `json:"addr"`
		Instances []struct {
			Model string `json:"model"`
			State string `json:"state"`
		} `json:"instances"`
	}
	var mesh struct {
		Self  chatNode   `json:"self"`
		Peers []chatNode `json:"peers"`
	}
	if err := call(context.Background(), s.addr, http.MethodGet, "/z/mesh", nil, &mesh); err != nil {
		return err
	}
	choices := []Choice{{Value: "", Label: "the whole mesh", Note: "let ModelFabric route"}}
	for _, n := range append([]chatNode{mesh.Self}, mesh.Peers...) {
		serves := false
		for _, i := range n.Instances {
			if i.Model == s.model && i.State == "ready" {
				serves = true
			}
		}
		if !serves {
			continue
		}
		choices = append(choices, Choice{
			Value: n.Node, Label: n.Node,
			Note: fmt.Sprintf("%d engine(s)", len(n.Instances)),
		})
	}
	if len(choices) == 1 {
		return fmt.Errorf("no node reports %s as ready", s.model)
	}
	pick, err := selectOne("Send to which node?", choices)
	if err != nil {
		return err
	}
	if pick == "" {
		s.pinned, s.pinnedTo = "", ""
		fmt.Println("  " + dim("routing to the whole mesh again"))
		return nil
	}
	// Use the peer entrypoint for this conversation; mfsh prefer changes routing for all local requests.
	for _, n := range append([]chatNode{mesh.Self}, mesh.Peers...) {
		if n.Node == pick {
			s.pinned, s.pinnedTo = "http://"+n.Addr, pick
			fmt.Println("  " + dim("pinned to "+pick+" ("+s.pinned+")"))
			return nil
		}
	}
	return fmt.Errorf("no address for %s", pick)
}

func (s *chatSession) target() string {
	if s.pinned != "" {
		return s.pinned
	}
	return s.addr
}

func (s *chatSession) printStats() {
	t := s.last
	if t == nil {
		fmt.Println("  " + dim("nothing generated yet"))
		return
	}
	// Timestamp the last successful reply so a failed turn does not appear to own its metrics.
	fmt.Printf("  %s\n", dim("the last reply that completed, "+
		time.Since(t.at).Round(time.Second).String()+" ago"))
	row := func(k, v string) { fmt.Printf("  %-18s %s\n", dim(k), v) }
	row("served by", pick(t.node == "", dim("not reported"), t.node+dim("/")+t.engine))
	row("chosen by", pick(t.via == "", dim("this node's router"), t.via))
	row("trace", dim(t.trace))
	row("to first token", fmt.Sprintf("%.2fs", t.ttft.Seconds()))
	row("total", fmt.Sprintf("%.2fs", t.total.Seconds()))
	if t.promptTokens > 0 {
		cached := ""
		if t.cachedTokens > 0 {
			cached = fmt.Sprintf(dim("  (%d from cache, %.0f%%)"),
				t.cachedTokens, 100*float64(t.cachedTokens)/float64(t.promptTokens))
		}
		row("prompt", fmt.Sprintf("%d tokens%s", t.promptTokens, cached))
	}
	if t.completionTokens > 0 {
		row("generated", fmt.Sprintf("%d tokens", t.completionTokens))
	}
	row("chunks", fmt.Sprintf("%d thinking, %d reply", t.reasoning, t.content))
	turns := len(s.msgs) / 2
	note := ""
	if imgs := s.imagesInHistory(); imgs > 0 {
		note = dim(fmt.Sprintf(", %d image(s) re-sent each turn", imgs))
	}
	row("conversation", fmt.Sprintf("%d turn(s)%s", turns, note))
	// Estimate the next prompt, including the reply just received.
	if t.contextLen > 0 && t.promptTokens > 0 {
		next := t.promptTokens + t.completionTokens
		pct := 100 * float64(next) / float64(t.contextLen)
		line := fmt.Sprintf("%s of %s tokens (%.0f%%)",
			comma(next), comma(t.contextLen), pct)
		switch {
		case pct >= 90:
			row("headroom", red(line)+dim("  — /clear or start a new conversation"))
		case pct >= 75:
			row("headroom", yellow(line))
		default:
			row("headroom", line)
		}
	}
	if t.contextNote != "" {
		// Flag a context mismatch because the reported headroom may use the wrong limit.
		row("context", yellow(t.contextNote))
	}
	if t.total > 0 && t.completionTokens > 0 {
		row("rate", fmt.Sprintf("%.0f tok/s", float64(t.completionTokens)/t.total.Seconds()))
	}
}

// imagesInHistory counts the attachments the conversation still carries, since
// each one rides along on every later turn.
func (s *chatSession) imagesInHistory() int {
	n := 0
	for _, m := range s.msgs {
		parts, ok := m["content"].([]map[string]any)
		if !ok {
			continue
		}
		for _, p := range parts {
			if p["type"] == "image_url" {
				n++
			}
		}
	}
	return n
}

func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

func pick(cond bool, a, b string) string {
	if cond {
		return a
	}
	return b
}

func (s *chatSession) say(ctx context.Context, text string) error {
	s.msgs = append(s.msgs, map[string]any{"role": "user", "content": s.userContent(text)})
	sent := s.pending
	s.pending = nil

	msgs := s.msgs
	if s.system != "" {
		msgs = append([]map[string]any{{"role": "system", "content": s.system}}, msgs...)
	}
	body, err := json.Marshal(map[string]any{
		"model": s.model, "messages": msgs, "stream": true,
		"stream_options": map[string]any{"include_usage": true},
	})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(s.target(), "/")+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	// Nodes without configured authentication ignore the key.
	if s.key != "" {
		req.Header.Set("Authorization", "Bearer "+s.key)
	}

	start := time.Now()
	resp, err := s.client.Do(req)
	if err != nil {
		// The turn did not happen, so it must not stay in the history: the
		// next message would carry a user turn the model never answered.
		s.msgs = s.msgs[:len(s.msgs)-1]
		if len(sent) > 0 {
			fmt.Printf("  %s\n", dim(fmt.Sprintf("%d image(s) were not sent and are no longer attached", len(sent))))
		}
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// Drop failed attachments so subsequent text turns do not repeat the same image error.
		s.msgs = s.msgs[:len(s.msgs)-1]
		if len(sent) > 0 {
			defer fmt.Printf("  %s\n", dim(fmt.Sprintf("%d image(s) were not sent and are no longer attached", len(sent))))
		}
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
		return fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(b)))
	}

	t := &chatTurn{
		node:   resp.Header.Get(router.NodeHeader),
		engine: resp.Header.Get(router.EngineHeader),
		via:    resp.Header.Get("X-Fabric-Via"),
		trace:  resp.Header.Get(router.TraceHeader),
	}

	var reply strings.Builder
	var thinking bool
	first := true
	fmt.Println()
	status := startChatStatus(start)
	defer status.end()
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), 8<<20)
	for sc.Scan() {
		data, ok := strings.CutPrefix(strings.TrimSpace(sc.Text()), "data:")
		if !ok {
			continue
		}
		data = strings.TrimSpace(data)
		if data == "" || data == "[DONE]" {
			continue
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content          string `json:"content"`
					ReasoningContent string `json:"reasoning_content"`
					Reasoning        string `json:"reasoning"`
				} `json:"delta"`
			} `json:"choices"`
			Usage *struct {
				PromptTokens        int `json:"prompt_tokens"`
				CompletionTokens    int `json:"completion_tokens"`
				PromptTokensDetails *struct {
					CachedTokens int `json:"cached_tokens"`
				} `json:"prompt_tokens_details"`
			} `json:"usage"`
		}
		if json.Unmarshal([]byte(data), &chunk) != nil {
			continue
		}
		if u := chunk.Usage; u != nil {
			t.promptTokens, t.completionTokens = u.PromptTokens, u.CompletionTokens
			if u.PromptTokensDetails != nil {
				t.cachedTokens = u.PromptTokensDetails.CachedTokens
			}
		}
		if len(chunk.Choices) == 0 {
			continue
		}
		d := chunk.Choices[0].Delta
		think := d.ReasoningContent
		if think == "" {
			think = d.Reasoning
		}
		if think != "" {
			t.reasoning++
			if first {
				t.ttft, first = time.Since(start), false
			}
			if s.showThink {
				if !thinking {
					status.end()
					fmt.Print(dim("  thinking  "))
					thinking = true
				}
				fmt.Print(dim(think))
			} else {
				status.set("thinking", int64(t.reasoning))
			}
		}
		if d.Content != "" {
			t.content++
			if first {
				t.ttft, first = time.Since(start), false
			}
			status.end()
			if thinking {
				fmt.Print("\n\n")
				thinking = false
			}
			fmt.Print(d.Content)
			reply.WriteString(d.Content)
		}
	}
	if err := sc.Err(); err != nil {
		return err
	}
	t.total, t.at = time.Since(start), time.Now()
	// Read context after the reply identifies its serving node; later turns may route elsewhere.
	t.contextLen, t.contextNote = s.contextOf(t.node, t.engine)
	s.last = t
	s.msgs = append(s.msgs, map[string]any{"role": "assistant", "content": reply.String()})

	fmt.Print("\n\n")
	fmt.Println("  " + s.footer(t))
	fmt.Println()
	return nil
}

func (s *chatSession) footer(t *chatTurn) string {
	where := dim("served by ") + dim("not reported")
	if t.node != "" {
		where = dim("served by ") + green(t.node)
		if t.engine != "" {
			where += dim("/" + t.engine)
		}
	}
	parts := []string{where}
	if t.via != "" {
		parts = append(parts, dim("via ")+t.via)
	}
	if t.completionTokens > 0 && t.total > 0 {
		parts = append(parts, cyan(fmt.Sprintf("%.0f tok/s", float64(t.completionTokens)/t.total.Seconds())))
	}
	parts = append(parts, dim(fmt.Sprintf("%.1fs to first token, %.1fs total", t.ttft.Seconds(), t.total.Seconds())))
	if t.promptTokens > 0 && t.cachedTokens > 0 {
		parts = append(parts, dim(fmt.Sprintf("%.0f%% of prompt cached",
			100*float64(t.cachedTokens)/float64(t.promptTokens))))
	}
	if t.reasoning > 0 && !s.showThink {
		parts = append(parts, dim(fmt.Sprintf("%d thinking chunks hidden", t.reasoning)))
	}
	return strings.Join(parts, dim("  ·  "))
}

// Image requests require a vision-enabled engine; llama.cpp rejects images with draft decoding.

type attachment struct {
	path string
	mime string
	data []byte
}

func (a attachment) dataURL() string {
	return "data:" + a.mime + ";base64," + base64.StdEncoding.EncodeToString(a.data)
}

// attach reads an image for the next message. Read now rather than at send
// time so a bad path is an error where it was typed.
func (s *chatSession) attach(path string) error {
	if path == "" {
		if len(s.pending) == 0 {
			return fmt.Errorf("/image <path> — attaches an image to the next message")
		}
		s.pending = nil
		fmt.Println("  " + dim("attachments cleared"))
		return nil
	}
	if strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		path = filepath.Join(home, path[2:])
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	mime := mimeOfImage(data, path)
	if mime == "" {
		return fmt.Errorf("%s does not look like an image llama.cpp can read (png, jpeg, webp, gif, bmp)", filepath.Base(path))
	}
	s.pending = append(s.pending, attachment{path: path, mime: mime, data: data})
	// Warn about image cost: base64 adds a third to the body size, and image tokens require prefill.
	fmt.Printf("  %s %s %s\n", dim("attached"), filepath.Base(path),
		dim(fmt.Sprintf("(%s, %s as sent)", mime, humanBytes(int64(len(data)*4/3)))))
	return nil
}

// mimeOfImage detects the content type from bytes to avoid engine errors from incorrect extensions.
func mimeOfImage(data []byte, path string) string {
	if t := http.DetectContentType(data); strings.HasPrefix(t, "image/") {
		return t
	}
	switch strings.ToLower(filepath.Ext(path)) {
	case ".webp":
		return "image/webp" // older Go does not sniff webp
	}
	return ""
}

// userContent builds one message: plain text when nothing is attached, and the
// array form when something is, because the two shapes are not interchangeable
// and an engine given the array form for a text-only turn may still try to
// load a projector.
func (s *chatSession) userContent(text string) any {
	if len(s.pending) == 0 {
		return text
	}
	parts := []map[string]any{{"type": "text", "text": text}}
	for _, a := range s.pending {
		parts = append(parts, map[string]any{
			"type":      "image_url",
			"image_url": map[string]any{"url": a.dataURL()},
		})
	}
	return parts
}

// Keep progress visible while reasoning text is hidden. Redraw on a timer,
// independent of token arrival, until the first reply text arrives.
type chatStatus struct {
	stop chan struct{}
	done chan struct{}
	// The read loop and ticker share these values under mu so each redraw
	// sees the label and count from the same update.
	tokens *int64
	label  *string
	mu     *sync.Mutex
}

var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

func startChatStatus(start time.Time) *chatStatus {
	var n int64
	label := "waiting for the first token"
	s := &chatStatus{
		stop: make(chan struct{}), done: make(chan struct{}),
		tokens: &n, label: &label, mu: &sync.Mutex{},
	}
	if !isStdoutTTY() {
		close(s.done) // avoid terminal escapes in redirected output
		return s
	}
	go func() {
		defer close(s.done)
		t := time.NewTicker(100 * time.Millisecond)
		defer t.Stop()
		for i := 0; ; i++ {
			select {
			case <-s.stop:
				fmt.Print("\r\033[K")
				return
			case <-t.C:
				s.mu.Lock()
				what, count := label, n
				s.mu.Unlock()
				line := fmt.Sprintf("  %s %s", dim(spinnerFrames[i%len(spinnerFrames)]), dim(what))
				if count > 0 {
					line += dim(fmt.Sprintf(" · %d tokens", count))
				}
				line += dim(fmt.Sprintf(" · %.0fs", time.Since(start).Seconds()))
				fmt.Print("\r\033[K" + line)
			}
		}
	}()
	return s
}

func (s *chatStatus) set(what string, tokens int64) {
	s.mu.Lock()
	*s.label, *s.tokens = what, tokens
	s.mu.Unlock()
}

// end clears the line and waits for the ticker to stop, so nothing is printed
// over the reply that follows.
func (s *chatStatus) end() {
	select {
	case <-s.done:
		return // never started, or already stopped
	default:
	}
	close(s.stop)
	<-s.done
}

func comma(n int) string {
	s := strconv.Itoa(n)
	if n < 0 {
		return "-" + comma(-n)
	}
	var b strings.Builder
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	return b.String()
}

// contextOf returns an engine's per-request context and any reported mismatch.
// Zero means unknown: missing /props, an unavailable node or no node header.
// The footer omits headroom when the limit is unknown.
func (s *chatSession) contextOf(node, engine string) (int, string) {
	if node == "" {
		return 0, ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var mesh struct {
		Self  meshNodeView   `json:"self"`
		Peers []meshNodeView `json:"peers"`
	}
	if call(ctx, s.addr, http.MethodGet, "/z/mesh", nil, &mesh) != nil {
		return 0, ""
	}
	for _, n := range append([]meshNodeView{mesh.Self}, mesh.Peers...) {
		if n.Node != node {
			continue
		}
		for _, i := range n.Instances {
			// By instance when the reply named one, since a node may hold
			// several engines for the same model at different contexts.
			if engine != "" && i.ID != engine {
				continue
			}
			return i.ContextLength, i.ContextNote
		}
	}
	return 0, ""
}
