package docslint

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The rules, without needing a tree.
func TestScanTextRules(t *testing.T) {
	for _, c := range []struct {
		name, text string
		want       bool
	}{
		{"plain prose", "The router picks the least loaded engine.\n", false},
		{"filler", "You must do this in order to route.\n", true},
		{"simply", "It simply works.\n", true},
		{"marketing", "A comprehensive solution.\n", true},
		{"announcing", "In this section we will explore routing.\n", true},

		// A flag inside a code sample is not prose, and neither is a long
		// command. Blanking fences is what keeps this linter usable.
		{"code fence is exempt", "Run it:\n\n```\nmfsh load --simply --in-order-to\n```\n", false},
		{"inline code is exempt", "Pass `--simply` to the engine.\n", false},

		// Lists, tables and JSX are legitimately long and are not paragraphs.
		// Long prose is not itself a fault: see the package comment for the
		// paragraph-length rule that was tried and dropped.
		{"long argued paragraph", strings.Repeat("word ", 200) + "\n", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := scanText("x.md", c.text)
			if (len(got) > 0) != c.want {
				t.Errorf("got %v, want findings=%v", got, c.want)
			}
		})
	}
}

// A real tailnet address shipped on the benchmark page inside a "reproduce
// this run" code block, because nothing checked for one. Addresses are
// checked in code as well as prose, and outside the docs site too.
func TestTailnetAddresses(t *testing.T) {
	for _, c := range []struct {
		name, path, text string
		want             bool
	}{
		{"example range is fine", "site/content/x.mdx", "Dial 100.64.0.2:1234.\n", false},
		{"real address in prose", "site/content/x.mdx", "Dial 100.69.171.44:1234.\n", true},
		{"real address in a code block", "site/content/x.mdx", "```\nADDR=http://100.69.171.44:1234\n```\n", true},
		{"real address outside the docs site", "bench/README.md", "curl 100.101.2.3\n", true},
		{"top of the range", "README.md", "100.127.255.1\n", true},
		{"outside the tailnet range", "README.md", "100.128.0.1 and 100.63.0.1\n", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := scanText(c.path, c.text)
			if (len(got) > 0) != c.want {
				t.Errorf("got %v, want findings=%v", got, c.want)
			}
		})
	}
}

// The published docs were rewritten without em dashes; this keeps them that way.
func TestEmDashes(t *testing.T) {
	for _, c := range []struct {
		name, path, text string
		want             bool
	}{
		{"em dash in published prose", "site/content/x.mdx", "It routes — mostly.\n", true},
		{"em dash in a component prop", "site/content/x.mdx", "<Card title=\"a — b\" />\n", true},
		{"inline code shows the glyph", "site/content/x.mdx", "Unknown reads as `—`.\n", false},
		{"sample output in a fence", "site/content/x.mdx", "```\nSTATE  —\n```\n", false},
		{"repository notes keep their style", "README.md", "It routes — mostly.\n", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := scanText(c.path, c.text)
			if (len(got) > 0) != c.want {
				t.Errorf("got %v, want findings=%v", got, c.want)
			}
		})
	}
}

// A finding has to name the file and line, or it is a chore rather than a fix.
func TestFindingPointsAtTheLine(t *testing.T) {
	text := "First line.\n\nSecond line.\n\nThis one is simply wrong.\n"
	got := scanText("docs/x.md", text)
	if len(got) != 1 {
		t.Fatalf("got %d findings, want 1: %v", len(got), got)
	}
	if got[0].Line != 5 {
		t.Errorf("line %d, want 5", got[0].Line)
	}
	if !strings.Contains(got[0].String(), "docs/x.md:5") {
		t.Errorf("%q should locate the problem", got[0])
	}
}

// The tree itself. This is the check that matters: it runs with every other
// test, so prose written today is held to the same rule as prose audited by
// hand after the fact.
func TestRepositoryProseIsClean(t *testing.T) {
	for _, root := range []string{"../../site/content", "../.."} {
		found, err := Scan(root)
		if err != nil {
			t.Fatalf("scan %s: %v", root, err)
		}
		for _, f := range found {
			t.Errorf("%s", f)
		}
	}
}

// The former domain was registered by someone else while the install command still read
// "curl -fsSL https://<it>/install.sh | sh", in the README, the docs, the
// landing page and install.sh itself. Any copy that survives is a stranger's
// script in someone's shell.
func TestHijackedDomainIsGoneEverywhere(t *testing.T) {
	found, err := ScanHijacked("../..")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range found {
		t.Errorf("%s", f)
	}
}

func TestHijackedDomainIsFoundInCode(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "install.sh"), []byte("curl https://"+hijackedDomain+"/install.sh | sh\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	found, err := ScanHijacked(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 1 || found[0].Line != 1 {
		t.Errorf("want one finding on line 1, got %v", found)
	}
}
