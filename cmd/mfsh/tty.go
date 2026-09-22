package main

import (
	"bufio"
	"fmt"
	"os"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

// Minimal terminal handling, stdlib only — no x/term dependency, so the binary
// stays a single static file with nothing to vendor.

// The ioctl numbers and the termios layout are per-OS: 0x5401/0x5402 are
// Linux's TCGETS/TCSETS, and using them on macOS (TIOCGETA/TIOCSETA, with a
// different struct) made every ioctl fail — so isTTY answered false on every
// Mac and ModelFabric never offered an interactive prompt there. The constants now
// live in tty_linux.go and tty_darwin.go, and the struct comes from syscall,
// which declares the right shape for each platform.
type termios = syscall.Termios

func ioctlTermios(fd uintptr, req uintptr, t *termios) error {
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, req, uintptr(unsafe.Pointer(t)))
	if errno != 0 {
		return errno
	}
	return nil
}

// sanitizeTTY strips control characters from text that came from elsewhere
// before it is written to a terminal. Printable text is untouched.
func sanitizeTTY(s string) string {
	if !strings.ContainsFunc(s, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			b.WriteRune('\uFFFD')
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// isTTY reports whether stdin and stdout are both a terminal. Anything piped
// or redirected must not get an interactive prompt.
// isTTY answers "can this prompt interactively?", which needs both ends. For
// "may this draw?" — a spinner, a redrawn status line — the question is about
// output alone, and isStdoutTTY (color.go) is the one to ask: keying a
// progress indicator on this one lost it for `mfsh chat < script.txt` run in a
// terminal.
func isTTY() bool {
	var t termios
	if ioctlTermios(os.Stdin.Fd(), ioctlReadTermios, &t) != nil {
		return false
	}
	if ioctlTermios(os.Stdout.Fd(), ioctlReadTermios, &t) != nil {
		return false
	}
	return true
}

// makeRaw puts the terminal in cbreak mode and returns a restore function.
func makeRaw(fd uintptr) (func(), error) {
	var old termios
	if err := ioctlTermios(fd, ioctlReadTermios, &old); err != nil {
		return nil, err
	}
	raw := old
	// Unbuffered, unechoed input, and ISIG off so Ctrl-C arrives as byte 3
	// rather than as a signal. The read loop already treats byte 3 as cancel;
	// with ISIG on that branch was unreachable, and the terminal driver killed
	// the process before the deferred restore could run — leaving the shell
	// with no echo.
	raw.Lflag &^= syscall.ECHO | syscall.ICANON | syscall.ISIG
	// VMIN=0 with VTIME=1 makes every read return within 100ms, with whatever
	// arrived. That is what lets a lone Esc be told from the start of an arrow
	// key: with VMIN=1 a read could return the Esc of "ESC [ A" on its own and
	// the loop cancelled the prompt the user was trying to navigate.
	raw.Cc[syscall.VMIN] = 0
	raw.Cc[syscall.VTIME] = 1
	if err := ioctlTermios(fd, ioctlWriteTermios, &raw); err != nil {
		return nil, err
	}
	return func() { _ = ioctlTermios(fd, ioctlWriteTermios, &old) }, nil
}

// Choice is one row in an interactive picker.
type Choice struct {
	Value string
	Label string
	Note  string // dimmed, right of the label
}

// ErrCancelled is returned when the user aborts a picker.
var ErrCancelled = fmt.Errorf("cancelled")

// selectOne renders an arrow-key picker and returns the chosen value.
//
// Falls back to a numbered prompt when the terminal cannot be put into raw
// mode, and refuses to prompt at all when stdin is not a terminal.
func selectOne(prompt string, choices []Choice) (string, error) {
	if len(choices) == 0 {
		return "", fmt.Errorf("nothing to choose from")
	}
	if len(choices) == 1 {
		return choices[0].Value, nil
	}
	if !isTTY() {
		return "", fmt.Errorf("not a terminal; name the target explicitly")
	}
	restore, err := makeRaw(os.Stdin.Fd())
	if err != nil {
		return selectNumbered(prompt, choices)
	}
	defer restore()

	// Display columns, not bytes: a CJK label is three bytes per character
	// but two columns, and padding by bytes misaligns every note after it.
	width := 0
	for _, c := range choices {
		if w := visibleWidth(c.Label); w > width {
			width = w
		}
	}

	sel := 0
	draw := func(first bool) {
		if !first {
			// Move back over the list to redraw it in place.
			fmt.Fprintf(os.Stderr, "\x1b[%dA", len(choices))
		}
		for i, c := range choices {
			marker, style := "  ", "\x1b[0m"
			if i == sel {
				marker, style = "\x1b[36m❯ \x1b[0m", "\x1b[36m"
			}
			note := ""
			if c.Note != "" {
				note = fmt.Sprintf("  \x1b[2m%s\x1b[0m", sanitizeTTY(c.Note))
			}
			// Labels come from catalog and peer data, which is to say from
			// another machine. Written raw into an ANSI UI, an escape in a
			// model name could move the cursor or recolour the prompt.
			fmt.Fprintf(os.Stderr, "\r\x1b[K%s%s%s\x1b[0m%s\n", marker, style, padVisible(sanitizeTTY(c.Label), width), note)
		}
	}

	fmt.Fprintf(os.Stderr, "\n\x1b[1m?\x1b[0m %s\n", prompt)
	draw(true)
	fmt.Fprint(os.Stderr, "\x1b[2m  ↑↓ navigate • ⏎ select • esc cancel\x1b[0m\n")

	buf := make([]byte, 3)
	idle := newIdleReads()
	for {
		// Reposition above the hint line before redrawing.
		n, err := os.Stdin.Read(buf)
		if n == 0 {
			// VTIME makes a read with no keypress return zero bytes, and Go
			// turns a zero-byte read into io.EOF — so "nobody has typed for
			// 100ms" and "stdin closed" arrive identically. Treating the error
			// as cancel meant the picker cancelled itself the moment it was
			// shown unless a key was already pending.
			if idle.closed(err) {
				return "", ErrCancelled
			}
			continue
		}
		if err != nil {
			return "", ErrCancelled
		}
		// A lone Esc may be the first byte of an arrow key that has not
		// arrived yet. One more read settles it: empty means it really was Esc.
		if buf[0] == 27 && n == 1 {
			if more, err := os.Stdin.Read(buf[1:]); err == nil && more > 0 {
				n += more
			}
		}
		switch {
		case buf[0] == 3, buf[0] == 27 && n == 1: // Ctrl-C, Esc
			fmt.Fprintln(os.Stderr)
			return "", ErrCancelled
		case buf[0] == 13, buf[0] == 10: // Enter
			fmt.Fprintln(os.Stderr)
			return choices[sel].Value, nil
		case n == 3 && buf[0] == 27 && buf[1] == 91:
			switch buf[2] {
			case 65: // up
				sel = (sel - 1 + len(choices)) % len(choices)
			case 66: // down
				sel = (sel + 1) % len(choices)
			default:
				continue
			}
		case buf[0] == 'k':
			sel = (sel - 1 + len(choices)) % len(choices)
		case buf[0] == 'j':
			sel = (sel + 1) % len(choices)
		default:
			continue
		}
		// Step up over the hint line, redraw, and put it back.
		fmt.Fprint(os.Stderr, "\x1b[1A")
		draw(false)
		fmt.Fprint(os.Stderr, "\x1b[2m  ↑↓ navigate • ⏎ select • esc cancel\x1b[0m\n")
	}
}

// selectNumbered is the fallback when raw mode is unavailable.
func selectNumbered(prompt string, choices []Choice) (string, error) {
	fmt.Fprintf(os.Stderr, "\n%s\n", prompt)
	for i, c := range choices {
		note := ""
		if c.Note != "" {
			note = "  " + c.Note
		}
		fmt.Fprintf(os.Stderr, "  %2d) %s%s\n", i+1, c.Label, note)
	}
	fmt.Fprint(os.Stderr, "\nSelect [1]: ")

	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil {
		return "", ErrCancelled
	}
	line = strings.TrimSpace(line)
	if line == "" {
		return choices[0].Value, nil
	}
	var n int
	if _, err := fmt.Sscanf(line, "%d", &n); err != nil || n < 1 || n > len(choices) {
		return "", fmt.Errorf("invalid selection %q", line)
	}
	return choices[n-1].Value, nil
}

// confirm asks a yes/no question, defaulting to yes.
func confirm(question string) bool {
	if !isTTY() {
		return false
	}
	fmt.Fprintf(os.Stderr, "%s [Y/n] ", question)
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "", "y", "yes":
		return true
	}
	return false
}

// idleReads tells a timed-out read from a closed stdin.
//
// With VMIN=0 and VTIME=1 a read returns every 100ms whether or not a key was
// pressed, and a zero-byte read reaches Go as io.EOF. Waiting and hanging up
// therefore look the same. They differ in pace: the timeout paces itself at
// ten a second, while a closed descriptor returns instantly and would spin.
type idleReads struct {
	first time.Time
	n     int
}

func newIdleReads() *idleReads { return &idleReads{first: time.Now()} }

// closed reports whether these empty reads mean stdin has gone away.
func (i *idleReads) closed(err error) bool {
	if err == nil {
		i.n, i.first = 0, time.Now()
		return false
	}
	i.n++
	// Twenty empty reads should take about two seconds. Far quicker than that
	// and nothing is pacing them, which means there is nothing to wait for.
	if i.n < 20 {
		return false
	}
	spun := time.Since(i.first) < time.Duration(i.n)*20*time.Millisecond
	i.n, i.first = 0, time.Now()
	return spun
}
