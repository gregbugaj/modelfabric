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

// `mfsh chat`: a conversation against the mesh, from the terminal.
//
// The roadmap said no chat client — "this is infrastructure, not a client" —
// and for a product that holds. This is not that. It exists so the mesh can be
// exercised end to end without installing anything: no SDK, no aider, no
// browser, nothing whose own bugs have to be ruled out before ModelFabric's can be
// looked at. Half the evening's debugging was working out whether a symptom
// was ModelFabric's or the client's.
//
// So what it shows is not the reply. Any client shows the reply. It shows
// **which node and engine answered, and how fast** — the thing a chat client
// has no reason to report and the only reason this one exists.

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
	// contextLen is how many tokens the serving engine allows one conversation,
	// and contextNote a disagreement between what it was loaded with and what it
	// reports. Zero means the mesh could not say — mlx-lm serves no /props — and
	// then no headroom is shown rather than one computed against a guess.
	contextLen  int
	contextNote string
}

type chatSession struct {
	addr      string
	model     string
	system    string
	pinned    string       // a node's base URL, empty for the whole mesh
	pinnedTo  string       // its name, for the prompt
	key       string       // this node's API key, when it wants one
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
	// No timeout: a long reply on a slow engine runs for minutes, and a client
	// that gives up halfway is exactly the failure this exists to rule out.
	s := &chatSession{
		addr: *addr, model: *model, system: *system, showThink: *think,
		key: *key, client: &http.Client{},
	}
	// A node only checks a key when it is configured to, and this usually runs
	// on the node itself, where the key is on disk. Reading it here means the
	// common case needs no flag; a node that wants no key ignores it.
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

// pickModel offers what the mesh serves, which is the union across nodes.
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
		// Dropped files are looked for before slash commands, because a
		// dropped path is usually absolute and so begins with a slash: an
		// image dragged onto the terminal was being answered with "no such
		// command: /home/greg/Pictures/...". Only paths that exist on disk
		// count, so a real command is never mistaken for one.
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
			// Saying nothing read as a command that had failed. With one model
			// in the mesh the picker does not even appear, so this is the only
			// feedback there is.
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

// pickNode pins the conversation to one node, which is how this tells a
// routing problem from an engine problem: the same prompt, the same model, one
// machine at a time.
func (s *chatSession) pickNode() error {
	// Its own shape rather than meshNodeView: pinning needs each node's
	// address, which that one has no reason to carry.
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
	// A peer's own front door, so the request is placed by that node rather
	// than this one — pinning here must not be confused with `mfsh prefer`,
	// which changes routing for everything on the node.
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
	// Which reply this is. A failed turn leaves the previous one here, and
	// without a time the numbers read as the failure's.
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
	// How much room is left. The benchmark's only unsolved tasks died at
	// ContextWindowExceededError with no warning that they were near it, and a
	// conversation here grows the same way — faster with an image in it.
	turns := len(s.msgs) / 2
	note := ""
	if imgs := s.imagesInHistory(); imgs > 0 {
		note = dim(fmt.Sprintf(", %d image(s) re-sent each turn", imgs))
	}
	row("conversation", fmt.Sprintf("%d turn(s)%s", turns, note))
	// The headroom this file could not show until the mesh published a context
	// length. Against the *next* prompt, not the last one: what matters is
	// whether the turn you are about to send fits, and it carries everything
	// already said plus the reply just received.
	if t.contextLen > 0 && t.promptTokens > 0 {
		next := t.promptTokens + t.completionTokens
		pct := 100 * float64(next) / float64(t.contextLen)
		line := fmt.Sprintf("%s of %s tokens (%.0f%%)",
			comma(next), comma(t.contextLen), pct)
		switch {
		case pct >= 90:
			// Named, not hinted: past this a turn is likely to be refused, and
			// the refusal arrives as an error with the conversation lost.
			row("headroom", red(line)+dim("  — /clear or start a new conversation"))
		case pct >= 75:
			row("headroom", yellow(line))
		default:
			row("headroom", line)
		}
	}
	if t.contextNote != "" {
		// The engine is not running what it was loaded with, so every figure
		// above is measured against a limit that is not the one in effect.
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

// say sends one turn and prints the reply as it arrives.
//
// Always streamed, whatever the node would do for a caller that did not ask:
// the point is to watch it being written, and a spinner followed by a wall of
// text tells you nothing about where the time went.
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
	// The loopback front door checks a key only when the node is configured to;
	// sending one it does not want is harmless, and not sending one it does
	// want fails with a clear 401 rather than a mystery.
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
		// The attachments are dropped, not put back. Keeping them meant every
		// later message silently carried the image until one succeeded: a
		// question about JavaScript, typed after a failed image, failed with
		// the same image error. Dropping a file again costs one gesture.
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
	// Runs until the reply's first word, so the wait is never silent.
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
				// Hidden, so the count is the only sign anything is happening.
				status.set("thinking", int64(t.reasoning))
			}
		}
		if d.Content != "" {
			t.content++
			if first {
				t.ttft, first = time.Since(start), false
			}
			// The reply is starting: the status line has done its job and must
			// not be redrawn over the text.
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
	// What the engine that served this turn allows one conversation. Asked after
	// the reply rather than before: the node is only known from the response
	// headers, and on a mesh the next turn may land somewhere else entirely.
	t.contextLen, t.contextNote = s.contextOf(t.node, t.engine)
	s.last = t
	s.msgs = append(s.msgs, map[string]any{"role": "assistant", "content": reply.String()})

	fmt.Print("\n\n")
	fmt.Println("  " + s.footer(t))
	fmt.Println()
	return nil
}

// footer is the line this command exists for: where the reply came from, and
// what it cost. A chat client would print the reply and stop.
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

// Images, because vision is the part of this mesh most worth poking at from a
// terminal: a model loaded `-vision off` keeps no projector and cannot read
// one, ModelFabric routes image requests only to engines that can, and llama.cpp
// fails a drafted prompt carrying an image outright. All three are routing
// questions, and all three need a real image to ask.

// attachment is an image waiting to go with the next message.
type attachment struct {
	path string
	mime string
	data []byte
}

// dataURL is how an image travels in an OpenAI-shaped request.
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
	// Base64 inflates by a third, and the whole thing rides in a JSON body
	// that has to be prefilled as image tokens — worth saying before a
	// six-megabyte screenshot is sent to a Mac at 166 tok/s.
	fmt.Printf("  %s %s %s\n", dim("attached"), filepath.Base(path),
		dim(fmt.Sprintf("(%s, %s as sent)", mime, humanBytes(int64(len(data)*4/3)))))
	return nil
}

// mimeOfImage sniffs the bytes rather than trusting the extension: a .png that
// is really a JPEG is common enough, and the engine rejects the mismatch with
// an error that says nothing about which file caused it.
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

// While the model is thinking there is nothing to print: reasoning is hidden
// by default, and on this model it is most of the output. The terminal sat
// blank for a minute with no way to tell a slow engine from a dead one — the
// exact confusion this command exists to remove.
//
// So a status line runs until the first word of the reply, then gets out of
// the way. It redraws in place on its own clock rather than per token, because
// a token can be ten seconds away on a slow node and a spinner that only moves
// when one arrives is not a spinner.
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
		close(s.done) // a pipe gets no spinner; it would be escape codes in a file
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

// comma groups thousands. A context limit is a five- or six-figure number and
// "131072" next to "130900" hides the thing worth seeing.
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

// contextOf is the per-request context of one engine, and any disagreement it
// reported, read from the mesh this node can already see.
//
// Zero when it cannot be determined — an engine serving no /props, a node that
// has dropped out, a reply with no node header. The footer then shows no
// headroom at all, which is the honest answer: a percentage against a guessed
// limit is the kind of number that gets believed.
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
