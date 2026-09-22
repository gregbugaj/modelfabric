package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Every terminal pastes a dropped file differently, and a shape that is not
// recognised does not fail loudly — the path is sent to the model as a
// question about a filename.
func TestDroppedRecognisesWhatTerminalsPaste(t *testing.T) {
	dir := t.TempDir()
	plain := filepath.Join(dir, "shot.png")
	spaced := filepath.Join(dir, "Screenshot from 2026-09-24.png")
	// A 1x1 PNG, so the content sniffs as an image.
	png := []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR" + strings.Repeat("\x00", 40))
	for _, p := range []string{plain, spaced} {
		if err := os.WriteFile(p, png, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	esc := strings.ReplaceAll(spaced, " ", `\ `)

	cases := []struct {
		name, line string
		wantPaths  []string
		wantRest   string
	}{
		{"bare path", plain, []string{plain}, ""},
		{"file URI", "file://" + plain, []string{plain}, ""},
		{"single quoted with spaces", "'" + spaced + "'", []string{spaced}, ""},
		{"double quoted", `"` + plain + `"`, []string{plain}, ""},
		{"backslash escaped spaces", esc, []string{spaced}, ""},
		{"dropped then a question", plain + " what is this?", []string{plain}, "what is this?"},
		{"two files", plain + " '" + spaced + "'", []string{plain, spaced}, ""},
		{"no path at all", "just a message", nil, "just a message"},
		// A dropped path is usually absolute, so it begins with a slash and
		// was being dispatched as a slash command. The chat loop now looks for
		// drops first; these two say why that order is safe.
		{"a real command is not a path", "/help", nil, "/help"},
		{"a command with an argument", "/reasoning on", nil, "/reasoning on"},
		{"a path that does not exist", "/nope/missing.png", nil, "/nope/missing.png"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			paths, rest := dropped(c.line)
			if len(paths) != len(c.wantPaths) {
				t.Fatalf("got %d paths %v, want %d %v", len(paths), paths, len(c.wantPaths), c.wantPaths)
			}
			for i := range paths {
				if paths[i] != c.wantPaths[i] {
					t.Errorf("path %d = %q, want %q", i, paths[i], c.wantPaths[i])
				}
			}
			if rest != c.wantRest {
				t.Errorf("rest = %q, want %q", rest, c.wantRest)
			}
		})
	}
}
