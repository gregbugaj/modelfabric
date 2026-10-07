package main

import (
	"fmt"
	"os"
	"strings"
)

// Terminal styling, stdlib only.
//
// Two rules the rest of the CLI depends on:
//
//   - Colour is disabled unless stdout is a terminal, so piping into a file or
//     another program yields clean text.
//   - NO_COLOR is honoured (https://no-color.org), and CLICOLOR_FORCE overrides
//     the TTY check for cases like `less -R`.
//
// Styling functions return the input unchanged when colour is off, so callers
// never branch on it.

var colorEnabled = detectColor()

func detectColor() bool {
	// NO_COLOR takes precedence; CLICOLOR_FORCE overrides only the TTY check.
	if _, set := os.LookupEnv("NO_COLOR"); set {
		return false
	}
	if os.Getenv("CLICOLOR_FORCE") != "" && os.Getenv("CLICOLOR_FORCE") != "0" {
		return true
	}
	if term := os.Getenv("TERM"); term == "" || term == "dumb" {
		return false
	}
	return isStdoutTTY()
}

func isStdoutTTY() bool {
	var t termios
	return ioctlTermios(os.Stdout.Fd(), ioctlReadTermios, &t) == nil
}

const (
	ansiReset  = "\x1b[0m"
	ansiBold   = "\x1b[1m"
	ansiDim    = "\x1b[2m"
	ansiRed    = "\x1b[31m"
	ansiGreen  = "\x1b[32m"
	ansiYellow = "\x1b[33m"
	ansiCyan   = "\x1b[36m"
)

func style(code, s string) string {
	if !colorEnabled || s == "" {
		return s
	}
	return code + s + ansiReset
}

func bold(s string) string   { return style(ansiBold, s) }
func dim(s string) string    { return style(ansiDim, s) }
func red(s string) string    { return style(ansiRed, s) }
func green(s string) string  { return style(ansiGreen, s) }
func yellow(s string) string { return style(ansiYellow, s) }
func cyan(s string) string   { return style(ansiCyan, s) }

// visibleWidth counts display columns, ignoring escape sequences.
// text/tabwriter counts escape bytes and misaligns colored cells.
func visibleWidth(s string) int {
	const (
		normal = iota
		afterESC
		inCSI
	)
	width, state := 0, normal
	for _, r := range s {
		switch state {
		case normal:
			if r == '\x1b' {
				state = afterESC
				continue
			}
			width += runeWidth(r)
		case afterESC:
			// '[' starts CSI despite lying in the 0x40-0x7E final-byte range; skip it before checking terminators.
			if r == '[' {
				state = inCSI
			} else {
				state = normal
			}
		case inCSI:
			// Parameter bytes are 0x30-0x3F and intermediates 0x20-0x2F; the
			// sequence ends at the first final byte in 0x40-0x7E.
			if r >= '@' && r <= '~' {
				state = normal
			}
		}
	}
	return width
}

type table struct {
	headers []string
	rows    [][]string
	indent  string
	gap     int
}

func newTable(headers ...string) *table {
	return &table{headers: headers, gap: 2}
}

func (t *table) add(cells ...string) { t.rows = append(t.rows, cells) }

func (t *table) render(w *strings.Builder) {
	cols := len(t.headers)
	for _, r := range t.rows {
		if len(r) > cols {
			cols = len(r)
		}
	}
	widths := make([]int, cols)
	measure := func(cells []string) {
		for i, c := range cells {
			if n := visibleWidth(c); n > widths[i] {
				widths[i] = n
			}
		}
	}
	measure(t.headers)
	for _, r := range t.rows {
		measure(r)
	}

	pad := strings.Repeat(" ", t.gap)
	line := func(cells []string, styleFn func(string) string) {
		w.WriteString(t.indent)
		for i, c := range cells {
			text := c
			if styleFn != nil {
				text = styleFn(c)
			}
			w.WriteString(text)
			// No trailing padding on the last column: it leaves invisible
			// whitespace that shows up when output is copied.
			if i < len(cells)-1 {
				w.WriteString(strings.Repeat(" ", widths[i]-visibleWidth(c)))
				w.WriteString(pad)
			}
		}
		w.WriteString("\n")
	}

	if len(t.headers) > 0 {
		line(t.headers, dim)
	}
	for _, r := range t.rows {
		line(r, nil)
	}
}

func (t *table) String() string {
	var b strings.Builder
	t.render(&b)
	return b.String()
}

func (t *table) print() { fmt.Print(t.String()) }

func statusDot(ok bool, label string) string {
	if ok {
		return green("●") + " " + label
	}
	return red("●") + " " + label
}

func padVisible(s string, n int) string {
	if d := n - visibleWidth(s); d > 0 {
		return s + strings.Repeat(" ", d)
	}
	return s
}

// runeWidth returns the terminal column count: two for East Asian wide
// characters and most emoji, zero for combining marks. The ranges follow
// Unicode's East Asian Width data without adding a dependency.
func runeWidth(r rune) int {
	switch {
	case r == 0:
		return 0
	case r < 0x300:
		return 1
	case r >= 0x300 && r <= 0x36F, // combining diacritics
		r >= 0x200B && r <= 0x200F, // zero-width space/joiners, direction marks
		r >= 0xFE00 && r <= 0xFE0F, // variation selectors
		r >= 0xE0100 && r <= 0xE01EF:
		return 0
	case r >= 0x1100 && r <= 0x115F, // Hangul Jamo
		r >= 0x2E80 && r <= 0x303E, // CJK radicals, punctuation
		r >= 0x3041 && r <= 0x33FF, // Kana, CJK compatibility
		r >= 0x3400 && r <= 0x4DBF, // CJK extension A
		r >= 0x4E00 && r <= 0x9FFF, // CJK unified ideographs
		r >= 0xA000 && r <= 0xA4CF, // Yi
		r >= 0xAC00 && r <= 0xD7A3, // Hangul syllables
		r >= 0xF900 && r <= 0xFAFF, // CJK compatibility ideographs
		r >= 0xFE30 && r <= 0xFE4F, // CJK compatibility forms
		r >= 0xFF00 && r <= 0xFF60, // fullwidth forms
		r >= 0xFFE0 && r <= 0xFFE6,
		r >= 0x1F300 && r <= 0x1F64F, // pictographs, emoticons
		r >= 0x1F900 && r <= 0x1F9FF, // supplemental symbols and pictographs
		r >= 0x1FA70 && r <= 0x1FAFF,
		r >= 0x20000 && r <= 0x3FFFD: // CJK extensions B and beyond
		return 2
	}
	return 1
}
