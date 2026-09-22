package main

import (
	"bufio"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// The chat prompt's line editor: enough of one to be usable, and no more.
//
// A plain bufio.Scanner was fine until slash commands existed, at which point
// the only way to discover them was to remember /help. This adds what makes a
// command surface usable — a list that narrows as you type, Tab to complete,
// history on the arrows — while staying inside the standard library, because
// ModelFabric ships one binary with no runtime dependencies and a readline package
// would be the first.
//
// Falls back to line-at-a-time reading whenever the terminal cannot be put
// into raw mode, or when input is a pipe. That path has no completion and is
// not meant to: it exists so `echo /help | mfsh chat` still works.

// lineEditor reads one line at a time, with completion and history.
type lineEditor struct {
	history  []string
	fallback *bufio.Scanner // used when the terminal cannot be raw
	// complete returns the candidates for the line so far. Each is the whole
	// replacement line, so a completer can rewrite as much as it likes.
	complete func(line string) []completion
}

type completion struct {
	Value string // the line this would become
	Label string // what the list shows
	Note  string // dimmed, to the right
}

func newLineEditor(complete func(string) []completion) *lineEditor {
	return &lineEditor{complete: complete, fallback: bufio.NewScanner(os.Stdin)}
}

// readLine prints prompt and returns what was typed. ok is false at EOF.
func (e *lineEditor) readLine(prompt string) (line string, ok bool) {
	if !isTTY() {
		fmt.Print(prompt)
		if !e.fallback.Scan() {
			return "", false
		}
		return e.fallback.Text(), true
	}
	restore, err := makeRaw(os.Stdin.Fd())
	if err != nil {
		fmt.Print(prompt)
		if !e.fallback.Scan() {
			return "", false
		}
		return e.fallback.Text(), true
	}
	defer restore()
	return e.raw(prompt)
}

func (e *lineEditor) raw(prompt string) (string, bool) {
	var buf []rune
	pos := 0               // cursor, in runes
	hist := len(e.history) // where in history; == len means "the line being typed"
	var pending string     // the line being typed, remembered while browsing history
	sel := 0               // highlighted completion
	shown := 0             // suggestion lines currently on screen, to erase

	redraw := func() {
		var cands []completion
		if e.complete != nil {
			cands = e.complete(string(buf))
		}
		if sel >= len(cands) {
			sel = 0
		}
		// \r to column 0, then erase everything below: the suggestion list
		// shrinks as the line narrows it, and leftovers read as stale matches.
		fmt.Print("\r\033[J")
		fmt.Print(prompt + string(buf))
		for i, c := range cands {
			label := "  " + c.Label
			if c.Note != "" {
				label += dim("  " + c.Note)
			}
			if i == sel {
				label = cyan("›") + " " + bold(c.Label)
				if c.Note != "" {
					label += dim("  " + c.Note)
				}
			}
			fmt.Print("\r\n" + label)
		}
		if len(cands) > 0 {
			// Back up to the input line and put the cursor where it belongs.
			fmt.Printf("\033[%dA", len(cands))
		}
		fmt.Printf("\r\033[%dC", visibleWidth(prompt)+pos)
		shown = len(cands)
	}

	clear := func() {
		fmt.Print("\r\033[J")
		shown = 0
	}

	redraw()
	// Read os.Stdin directly rather than through a bufio.Reader. A buffered
	// reader reads ahead, and anything it holds when this returns is lost to
	// whatever reads next — selectOne, for instance, which /node and /model
	// hand control to.
	buf1 := make([]byte, 1)
	idle := newIdleReads()
	readByte := func() (byte, bool) {
		for {
			n, err := os.Stdin.Read(buf1)
			if n > 0 {
				idle.closed(nil)
				return buf1[0], true
			}
			// Zero bytes is VTIME expiring, which Go reports as io.EOF; only a
			// run of them arriving faster than the timeout means stdin is gone.
			if idle.closed(err) {
				return 0, false
			}
		}
	}
	for {
		b, ok := readByte()
		if !ok {
			clear()
			return "", false
		}
		r := rune(b)
		switch r {
		case 3: // Ctrl-C: abandon the line, keep the session
			clear()
			fmt.Print(prompt + string(buf) + dim("  ^C") + "\r\n")
			return "", true
		case 4: // Ctrl-D
			if len(buf) == 0 {
				clear()
				return "", false
			}
		case '\r', '\n':
			// Enter takes the highlighted completion when the list is showing
			// something other than the line itself, which is what makes Enter
			// and Tab feel the same rather than subtly different.
			if shown > 0 && e.complete != nil {
				if cands := e.complete(string(buf)); len(cands) > 0 && sel < len(cands) &&
					cands[sel].Value != string(buf) && strings.HasPrefix(string(buf), "/") {
					buf = []rune(cands[sel].Value)
					pos = len(buf)
					redraw()
					continue
				}
			}
			clear()
			fmt.Print(prompt + string(buf) + "\r\n")
			line := string(buf)
			if s := strings.TrimSpace(line); s != "" {
				e.history = append(e.history, line)
			}
			return line, true
		case '\t':
			if e.complete == nil {
				continue
			}
			cands := e.complete(string(buf))
			if len(cands) == 0 {
				continue
			}
			if sel < len(cands) {
				buf = []rune(cands[sel].Value)
				pos = len(buf)
			}
		case 127, 8: // Backspace
			if pos > 0 {
				buf = append(buf[:pos-1], buf[pos:]...)
				pos--
			}
		case 21: // Ctrl-U
			buf, pos = nil, 0
		case 27: // escape sequence
			b1, ok1 := readByte()
			if !ok1 || b1 != '[' {
				continue
			}
			b2, ok2 := readByte()
			if !ok2 {
				continue
			}
			switch rune(b2) {
			case 'A': // up: through the suggestions if any, else history
				if shown > 1 {
					sel = (sel - 1 + shown) % shown
				} else if hist > 0 {
					if hist == len(e.history) {
						pending = string(buf)
					}
					hist--
					buf = []rune(e.history[hist])
					pos = len(buf)
				}
			case 'B': // down
				if shown > 1 {
					sel = (sel + 1) % shown
				} else if hist < len(e.history) {
					hist++
					if hist == len(e.history) {
						buf = []rune(pending)
					} else {
						buf = []rune(e.history[hist])
					}
					pos = len(buf)
				}
			case 'C': // right
				if pos < len(buf) {
					pos++
				}
			case 'D': // left
				if pos > 0 {
					pos--
				}
			}
		default:
			if r < 32 {
				continue
			}
			buf = append(buf, 0)
			copy(buf[pos+1:], buf[pos:])
			buf[pos] = r
			pos++
			sel = 0
		}
		redraw()
	}
}

// completeChat offers slash commands, and file paths once a command wants one.
func completeChat(cmds []completion) func(string) []completion {
	return func(line string) []completion {
		if !strings.HasPrefix(line, "/") {
			return nil
		}
		// Past the first space the command has been chosen and what it wants
		// is an argument. Only /image takes one that can be completed.
		if cmd, arg, found := strings.Cut(line, " "); found {
			if cmd == "/image" {
				return completePath(cmd, arg)
			}
			return nil
		}
		var out []completion
		for _, c := range cmds {
			if strings.HasPrefix(c.Value, line) {
				out = append(out, c)
			}
		}
		return out
	}
}

// completePath completes a filesystem path, which is what makes /image usable:
// nobody types an absolute path to a screenshot by hand.
func completePath(cmd, arg string) []completion {
	expanded := arg
	if strings.HasPrefix(expanded, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			expanded = filepath.Join(home, expanded[2:])
		}
	}
	dir, prefix := filepath.Split(expanded)
	if dir == "" {
		dir = "."
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []completion
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(strings.ToLower(name), strings.ToLower(prefix)) {
			continue
		}
		if strings.HasPrefix(name, ".") && !strings.HasPrefix(prefix, ".") {
			continue
		}
		full := filepath.Join(dir, name)
		note := ""
		if e.IsDir() {
			full += string(filepath.Separator)
			note = "directory"
		} else if !isImageFile(name) {
			continue // /image wants images; listing the rest is noise
		}
		out = append(out, completion{Value: cmd + " " + full, Label: name, Note: note})
		if len(out) >= 12 {
			break
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Label < out[j].Label })
	return out
}

func isImageFile(name string) bool {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".png", ".jpg", ".jpeg", ".webp", ".gif", ".bmp":
		return true
	}
	return false
}

// Dropping a file onto a terminal pastes its path into the line. Every
// terminal does it slightly differently — VTE sends a file:// URI, iTerm2
// backslash-escapes the spaces, others single-quote the whole thing — so a
// dropped path arrives as one of several shapes and none of them is a message
// the model should be asked about.
//
// So the line is read for paths that exist and are images, those become
// attachments, and whatever text is left is the message. Dragging a screenshot
// in and typing a question on the same line does what it looks like it does.

// dropped splits a line into image paths that exist on disk and the rest.
func dropped(line string) (paths []string, rest string) {
	var kept []string
	for _, tok := range shellish(line) {
		if p, ok := imagePath(tok); ok {
			paths = append(paths, p)
			continue
		}
		kept = append(kept, tok.text)
	}
	return paths, strings.TrimSpace(strings.Join(kept, " "))
}

type token struct {
	text  string // with quoting and escaping resolved
	quote byte   // the quote that wrapped it, 0 for none
}

// shellish splits on spaces the way a shell would, honouring quotes and
// backslash escapes — which is what terminals produce when they paste a path
// containing spaces, and "Screenshot from 2026-09-24.png" contains three.
func shellish(s string) []token {
	var out []token
	var cur strings.Builder
	var quote byte
	started := false
	flush := func() {
		if started || cur.Len() > 0 {
			out = append(out, token{text: cur.String(), quote: quote})
			cur.Reset()
		}
		started, quote = false, 0
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case quote != 0 && c == quote:
			flush()
		case quote != 0:
			cur.WriteByte(c)
		case c == '\'' || c == '"':
			quote, started = c, true
		case c == '\\' && i+1 < len(s):
			i++
			cur.WriteByte(s[i])
		case c == ' ' || c == '\t':
			if cur.Len() > 0 {
				flush()
			}
		default:
			cur.WriteByte(c)
		}
	}
	flush()
	return out
}

// imagePath resolves a token to an image on disk, or says it is not one.
func imagePath(t token) (string, bool) {
	p := t.text
	if p == "" {
		return "", false
	}
	// VTE and friends drop a URI rather than a path.
	if after, ok := strings.CutPrefix(p, "file://"); ok {
		if u, err := url.PathUnescape(after); err == nil {
			p = u
		} else {
			p = after
		}
		// file://localhost/path and file:///path both appear.
		p = strings.TrimPrefix(p, "localhost")
	}
	if strings.HasPrefix(p, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", false
		}
		p = filepath.Join(home, p[2:])
	}
	// A bare word that happens to name a file in the working directory is
	// still a path, but a quoted one is almost certainly a drop and worth
	// resolving even when the extension is unfamiliar.
	if !isImageFile(p) && t.quote == 0 {
		return "", false
	}
	st, err := os.Stat(p)
	if err != nil || st.IsDir() {
		return "", false
	}
	return p, true
}
