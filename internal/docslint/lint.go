// Package docslint checks Markdown for filler phrases, real tailnet addresses,
// and em dashes in site prose. Paragraph length is not a lint rule.
package docslint

import (
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

var filler = []struct {
	re  *regexp.Regexp
	why string
}{
	{regexp.MustCompile(`(?i)\bin order to\b`), `"in order to" is "to"`},
	{regexp.MustCompile(`(?i)\bsimply \w`), `"simply" tells the reader their trouble is their own fault`},
	{regexp.MustCompile(`(?i)\bdelve\b`), `"delve"`},
	{regexp.MustCompile(`(?i)\bseamless(ly)?\b`), `"seamless" is a claim, not a fact`},
	{regexp.MustCompile(`(?i)\bleverag(e|es|ing)\b`), `"leverage" is "use"`},
	{regexp.MustCompile(`(?i)\butiliz(e|es|ing)\b`), `"utilize" is "use"`},
	{regexp.MustCompile(`(?i)\bit'?s (important|crucial|worth) (to )?(note|noting)\b`), `if it were not worth noting it would not be written down`},
	{regexp.MustCompile(`(?i)\bin this (section|guide|chapter|document)\b`), `the reader knows which section they are in`},
	{regexp.MustCompile(`(?i)\bwe('| wi)ll (now )?(delve|explore|discuss|cover|look at)\b`), `say the thing instead of announcing it`},
	{regexp.MustCompile(`(?i)\b(a testament to|unlocks? the (power|potential)|best.in.class|cutting.edge)\b`), `marketing`},
	{regexp.MustCompile(`(?i)\bcomprehensive\b`), `"comprehensive" is never checked by anyone`},
}

// tailnetAddr matches Tailscale addresses in prose and code blocks.
// Only the documentation example range 100.64.0.x is allowed.
var tailnetAddr = regexp.MustCompile(`\b100\.(6[4-9]|[7-9][0-9]|1[01][0-9]|12[0-7])\.\d{1,3}\.\d{1,3}\b`)

func isExampleAddr(a string) bool { return strings.HasPrefix(a, "100.64.0.") }

// emDash applies to site/content prose. Code blocks and inline code are exempt
// so documentation can show literal UI values.
const emDash = "—"

func isPublishedDoc(path string) bool {
	return strings.Contains(filepath.ToSlash(path), "site/content/")
}

// hijackedDomain is the project's previous domain, registered by someone else
// in September 2026, when the project became ModelFabric. The install command was `curl -fsSL https://<it>/install.sh
// | sh`, so any copy left anywhere pipes a stranger's script into a shell.
// Assembled from parts so this file does not itself contain it.
var hijackedDomain = "modelfabric" + ".ai"

// ScanHijacked reports the previous domain in source, scripts, configuration,
// and prose, including executable installation URLs.
func ScanHijacked(root string) ([]Finding, error) {
	var out []Finding
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case "node_modules", ".next", "dist", ".git", "_pagefind", "public":
				// public/ holds build-time copies (install.sh is copied there
				// by the docs build); the sources they come from are scanned.
				return fs.SkipDir
			}
			return nil
		}
		b, err := readFile(path)
		if err != nil {
			return err
		}
		if bytes.IndexByte(b, 0) >= 0 {
			return nil
		}
		text := string(b)
		for off := 0; ; {
			i := strings.Index(text[off:], hijackedDomain)
			if i < 0 {
				break
			}
			out = append(out, Finding{path, lineOf(text, off+i), "the former domain, now someone else's; use modelfabric.sh, or api.example.com for an example"})
			off += i + len(hijackedDomain)
		}
		return nil
	})
	return out, err
}

type Finding struct {
	File string
	Line int
	Why  string
}

func (f Finding) String() string { return fmt.Sprintf("%s:%d: %s", f.File, f.Line, f.Why) }

func Scan(root string) ([]Finding, error) {
	var out []Finding
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case "node_modules", ".next", "dist", ".git", "_pagefind":
				return fs.SkipDir
			}
			return nil
		}
		ext := strings.ToLower(filepath.Ext(path))
		if ext != ".md" && ext != ".mdx" {
			return nil
		}
		b, err := readFile(path)
		if err != nil {
			return err
		}
		out = append(out, scanText(path, string(b))...)
		return nil
	})
	return out, err
}

func scanText(path, text string) []Finding {
	var out []Finding
	prose := proseOnly(text)

	for _, f := range filler {
		for _, m := range f.re.FindAllStringIndex(prose, -1) {
			out = append(out, Finding{path, lineOf(prose, m[0]), f.why + ": " + strings.TrimSpace(prose[m[0]:m[1]])})
		}
	}

	for _, m := range tailnetAddr.FindAllStringIndex(text, -1) {
		if a := text[m[0]:m[1]]; !isExampleAddr(a) {
			out = append(out, Finding{path, lineOf(text, m[0]), "real tailnet address, use 100.64.0.x: " + a})
		}
	}

	if isPublishedDoc(path) {
		for off := 0; ; {
			i := strings.Index(prose[off:], emDash)
			if i < 0 {
				break
			}
			out = append(out, Finding{path, lineOf(prose, off+i), "em dash: rewrite with a period, comma, colon or parentheses"})
			off += i + len(emDash)
		}
	}

	return out
}

// proseOnly blanks out code fences, inline code and JSX blocks, keeping the
// line count intact so a finding still points at the right line. A flag in a
// code sample is not filler, and a component's props are not a paragraph.
func proseOnly(text string) string {
	var b strings.Builder
	b.Grow(len(text))
	inFence := false
	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") {
			inFence = !inFence
			b.WriteByte('\n')
			continue
		}
		if inFence {
			b.WriteByte('\n')
			continue
		}
		b.WriteString(inlineCode.ReplaceAllString(line, " "))
		b.WriteByte('\n')
	}
	return b.String()
}

var inlineCode = regexp.MustCompile("`[^`]*`")

func lineOf(text string, offset int) int {
	return strings.Count(text[:offset], "\n") + 1
}

// readFile is a variable so the tests can check the rules without a tree.
var readFile = os.ReadFile
