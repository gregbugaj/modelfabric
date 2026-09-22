package main

import (
	"strings"
	"testing"
)

// visibleWidth is what keeps coloured tables aligned. The subtle case: '[' is
// 0x5B, which sits inside the CSI final-byte range 0x40-0x7E, so a naive
// terminator check ends the escape at the bracket and counts "2m...0m" as
// visible text. Every column after a styled cell then drifts.
func TestVisibleWidthIgnoresEscapes(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"plain", 5},
		{"", 0},
		{"\x1b[2m27B\x1b[0m", 3},
		{"\x1b[32m●\x1b[0m up", 4},
		{"\x1b[1m\x1b[36mboth\x1b[0m", 4},
		{"\x1b[38;5;214morange\x1b[0m", 6},
		{"—", 1},
		{"\x1b[2m—\x1b[0m", 1},
	}
	for _, c := range cases {
		if got := visibleWidth(c.in); got != c.want {
			t.Errorf("visibleWidth(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

// The styling helpers must agree with the width function, whatever the
// colour setting is.
func TestStyledStringsMeasureAsPlain(t *testing.T) {
	saved := colorEnabled
	defer func() { colorEnabled = saved }()

	colorEnabled = true
	for _, f := range []func(string) string{bold, dim, red, green, yellow, cyan} {
		if got := visibleWidth(f("abcd")); got != 4 {
			t.Fatalf("styled width = %d, want 4", got)
		}
	}
	colorEnabled = false
	if got := dim("abcd"); got != "abcd" {
		t.Fatalf("with colour off, dim() must not add escapes, got %q", got)
	}
}

// A table with coloured cells must align exactly like one without.
func TestTableAlignsWithColouredCells(t *testing.T) {
	saved := colorEnabled
	defer func() { colorEnabled = saved }()
	colorEnabled = true

	coloured := newTable("MODEL", "SIZE", "STATE")
	coloured.add("a-very-long-model-name", dim("19.0GB"), green("loaded"))
	coloured.add("short", dim("613B"), dim("—"))

	plain := newTable("MODEL", "SIZE", "STATE")
	plain.add("a-very-long-model-name", "19.0GB", "loaded")
	plain.add("short", "613B", "—")

	colouredLines := strings.Split(strings.TrimRight(coloured.String(), "\n"), "\n")
	plainLines := strings.Split(strings.TrimRight(plain.String(), "\n"), "\n")
	if len(colouredLines) != len(plainLines) {
		t.Fatalf("line counts differ: %d vs %d", len(colouredLines), len(plainLines))
	}
	for i := range plainLines {
		if got, want := visibleWidth(colouredLines[i]), visibleWidth(plainLines[i]); got != want {
			t.Fatalf("line %d: coloured width %d != plain width %d\ncoloured: %q\nplain:    %q",
				i, got, want, colouredLines[i], plainLines[i])
		}
	}

	// And the columns must start at the same offsets. Both tables dim their
	// headers, so strip both sides before comparing.
	for i, line := range colouredLines {
		got, want := stripANSI(line), stripANSI(plainLines[i])
		if got != want {
			t.Fatalf("line %d differs once stripped:\ngot:  %q\nwant: %q", i, got, want)
		}
	}
}

func stripANSI(s string) string {
	var b strings.Builder
	state := 0
	for _, r := range s {
		switch state {
		case 0:
			if r == '\x1b' {
				state = 1
				continue
			}
			b.WriteRune(r)
		case 1:
			if r == '[' {
				state = 2
			} else {
				state = 0
			}
		case 2:
			if r >= '@' && r <= '~' {
				state = 0
			}
		}
	}
	return b.String()
}

func TestNoColorEnvDisablesStyling(t *testing.T) {
	saved := colorEnabled
	defer func() { colorEnabled = saved }()
	colorEnabled = false
	if s := green("ok"); s != "ok" {
		t.Fatalf("expected no escapes when colour is disabled, got %q", s)
	}
	if s := statusDot(true, "up"); strings.Contains(s, "\x1b") {
		t.Fatalf("statusDot leaked escapes with colour off: %q", s)
	}
}

// Wide characters take two terminal columns. Counting them as one misaligned
// every column after a CJK or emoji model name.
func TestVisibleWidthCountsWideCharacters(t *testing.T) {
	cases := map[string]int{
		"qwen":             4,
		"通义千问":             8, // four CJK ideographs
		"모델":               4, // Hangul
		"モデル":              6, // Katakana
		"model 😊":          8,
		"\x1b[2m千问\x1b[0m": 4,
		"é":               1, // e + combining acute
	}
	for in, want := range cases {
		if got := visibleWidth(in); got != want {
			t.Errorf("visibleWidth(%q) = %d, want %d", in, got, want)
		}
	}
}

func TestTableAlignsWithWideCharacters(t *testing.T) {
	saved := colorEnabled
	defer func() { colorEnabled = saved }()
	colorEnabled = false
	tb := newTable("MODEL", "SIZE")
	tb.add("通义千问", "16GB")
	tb.add("qwen/qwen3-0.6b", "462MB")
	lines := strings.Split(strings.TrimRight(tb.String(), "\n"), "\n")
	// The SIZE column must start at the same display column on every row.
	col := -1
	for _, l := range lines {
		idx := strings.LastIndex(l, "  ") + 2
		if c := visibleWidth(l[:idx]); col == -1 {
			col = c
		} else if c != col {
			t.Fatalf("columns misaligned:\n%s", strings.Join(lines, "\n"))
		}
	}
}

// Choice labels come from catalog and peer data — another machine's strings.
// Written raw into the ANSI picker, an escape sequence in a model name could
// move the cursor, recolour the prompt or hide what is being selected.
func TestTTYTextFromElsewhereIsSanitized(t *testing.T) {
	if got := sanitizeTTY("qwen/qwen3-0.6b"); got != "qwen/qwen3-0.6b" {
		t.Errorf("ordinary text was changed: %q", got)
	}
	evil := "model\x1b[2Joops\r\x07"
	got := sanitizeTTY(evil)
	for _, bad := range []string{"\x1b", "\r", "\x07"} {
		if strings.Contains(got, bad) {
			t.Errorf("control character %q survived: %q", bad, got)
		}
	}
	if !strings.Contains(got, "model") || !strings.Contains(got, "oops") {
		t.Errorf("the readable text should survive: %q", got)
	}
}
