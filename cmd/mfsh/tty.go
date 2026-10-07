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

// ioctl constants and termios layouts differ by OS. Keep constants in
// tty_linux.go and tty_darwin.go and use syscall's platform-specific struct;
// Linux values make macOS TTY detection fail.
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

// isTTY requires both stdin and stdout to be terminals for interactive prompts.
// Use isStdoutTTY for progress displays so redirected input does not hide them.
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
	// Disable ISIG so Ctrl-C reaches the read loop as byte 3 and deferred terminal restoration runs.
	raw.Lflag &^= syscall.ECHO | syscall.ICANON | syscall.ISIG
	// VMIN=0 and VTIME=1 bound reads to 100ms. A follow-up read distinguishes
	// lone Esc from an arrow-key sequence arriving in separate reads.
	raw.Cc[syscall.VMIN] = 0
	raw.Cc[syscall.VTIME] = 1
	if err := ioctlTermios(fd, ioctlWriteTermios, &raw); err != nil {
		return nil, err
	}
	return func() { _ = ioctlTermios(fd, ioctlWriteTermios, &old) }, nil
}

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
			// Strip controls from catalog and peer labels to prevent terminal escape injection.
			fmt.Fprintf(os.Stderr, "\r\x1b[K%s%s%s\x1b[0m%s\n", marker, style, padVisible(sanitizeTTY(c.Label), width), note)
		}
	}

	fmt.Fprintf(os.Stderr, "\n\x1b[1m?\x1b[0m %s\n", prompt)
	draw(true)
	fmt.Fprint(os.Stderr, "\x1b[2m  ↑↓ navigate • ⏎ select • esc cancel\x1b[0m\n")

	buf := make([]byte, 3)
	idle := newIdleReads()
	for {
		n, err := os.Stdin.Read(buf)
		if n == 0 {
			// A VTIME timeout produces zero bytes and io.EOF; treating it as cancellation closes an idle picker.
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
		fmt.Fprint(os.Stderr, "\x1b[1A")
		draw(false)
		fmt.Fprint(os.Stderr, "\x1b[2m  ↑↓ navigate • ⏎ select • esc cancel\x1b[0m\n")
	}
}

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

// idleReads distinguishes VTIME timeouts from closed stdin. Both produce
// zero bytes and io.EOF; timeouts arrive about every 100ms, while closed
// stdin returns immediately and would spin.
type idleReads struct {
	first time.Time
	n     int
}

func newIdleReads() *idleReads { return &idleReads{first: time.Now()} }

func (i *idleReads) closed(err error) bool {
	if err == nil {
		i.n, i.first = 0, time.Now()
		return false
	}
	i.n++
	// Twenty VTIME expirations take about two seconds; much faster reads indicate closed stdin.
	if i.n < 20 {
		return false
	}
	spun := time.Since(i.first) < time.Duration(i.n)*20*time.Millisecond
	i.n, i.first = 0, time.Now()
	return spun
}
