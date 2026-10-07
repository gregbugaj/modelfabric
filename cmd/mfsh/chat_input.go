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

// The chat line editor provides completion and history using the standard library.
// Piped input and terminals without raw mode fall back to line-at-a-time reading.

type lineEditor struct {
	history  []string
	fallback *bufio.Scanner // used when the terminal cannot be raw
	// complete returns candidate replacement lines for the current input.
	complete func(line string) []completion
}

type completion struct {
	Value string // the line this would become
	Label string
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
	// Read stdin directly: buffered read-ahead would lose input when control passes to selectOne.
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

func completeChat(cmds []completion) func(string) []completion {
	return func(line string) []completion {
		if !strings.HasPrefix(line, "/") {
			return nil
		}
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
			continue
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

// Terminal file drops may use file:// URIs, backslash escapes or quoted paths.
// Convert existing image paths to attachments and leave the remaining text as the message.

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

// shellish splits on spaces while honoring quotes and backslash escapes used in terminal file drops.
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

func imagePath(t token) (string, bool) {
	p := t.text
	if p == "" {
		return "", false
	}
	// VTE drops a file URI rather than a path.
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
	// Allow quoted paths with unfamiliar extensions; unquoted words must have a known image extension.
	if !isImageFile(p) && t.quote == 0 {
		return "", false
	}
	st, err := os.Stat(p)
	if err != nil || st.IsDir() {
		return "", false
	}
	return p, true
}
