// Package docslint checks the prose in this repository for the two kinds of
// slop that have actually appeared in it.
//
// It exists because the audit that found them was run by hand, after the fact,
// and three of the four offending paragraphs had been written that same day.
// A rule nobody runs is a rule that decays, so these run with the tests.
//
// It checks filler phrases that can be deleted without losing meaning, real
// tailnet addresses anywhere in the Markdown, and em dashes in the docs site.
//
// Filler first. Every pattern below matched something real here; none is included on
// principle, because a style linter that flags every adverb gets switched off.
//
// It deliberately does NOT check paragraph length, and that is worth recording.
// The hand audit found four bloated paragraphs that were really lists written
// as prose, so a >75-word rule looked obvious. Run against the tree it flagged
// four more of 81-117 words that are well-argued prose — the flyout's design
// rationale, the quantization control's — sitting in exactly the same range as
// the genuine offenders. Length does not separate an argument from a disguised
// list; only reading does. A rule that would have forced those four into
// bullets is worse than no rule, so that judgement stays in review.
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

// filler is text that can be deleted without losing meaning. Each pattern here
// matched something real in this repository; none is included on principle.
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

// tailnetAddr is any address in Tailscale's 100.64.0.0/10 range. Docs use
// 100.64.0.x as the example range; anything else is a real machine. A real one
// reached the published benchmark page inside a code block, which is why this
// scans code too: a leaked address in a command is still leaked.
var tailnetAddr = regexp.MustCompile(`\b100\.(6[4-9]|[7-9][0-9]|1[01][0-9]|12[0-7])\.\d{1,3}\.\d{1,3}\b`)

func isExampleAddr(a string) bool { return strings.HasPrefix(a, "100.64.0.") }

// emDash is checked only under site/content. The published docs were
// rewritten on 2026-09-29 to drop them because a page full of em dashes reads
// as generated; repository notes are working documents and keep their own
// style. Code and inline code are exempt: the dashboard renders "—" for an
// unknown value, and the docs have to be able to show that.
const emDash = "—"

func isPublishedDoc(path string) bool {
	return strings.Contains(filepath.ToSlash(path), "site/content/")
}

// hijackedDomain is the project's previous domain, registered by someone else
// in September 2026, when the project became ModelFabric. The install command was `curl -fsSL https://<it>/install.sh
// | sh`, so any copy left anywhere pipes a stranger's script into a shell.
// Assembled from parts so this file does not itself contain it.
var hijackedDomain = "modelfabric" + ".ai"

// ScanHijacked walks root and reports every text file naming the hijacked
// domain: code, scripts and config as well as prose, because the dangerous
// copies were in install.sh and the site's metadata, not only the docs.
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
			return nil // binary
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

// Finding is one thing worth changing.
type Finding struct {
	File string
	Line int
	Why  string
}

func (f Finding) String() string { return fmt.Sprintf("%s:%d: %s", f.File, f.Line, f.Why) }

// Scan walks root and reports slop in every Markdown file under it.
func Scan(root string) ([]Finding, error) {
	var out []Finding
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// Generated and vendored trees are nobody's prose.
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
