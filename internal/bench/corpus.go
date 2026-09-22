package bench

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"io"
	"sync"
)

// The text a benchmark sends is part of what it measures. Prefill speed does
// not care what the tokens say, but generation can: with speculative
// decoding, measured on one RTX 5090, the same model ran 8% slower on
// unguessable text and twice as fast on predictable code. So the prompts are
// fixed texts shipped in the binary, named and versioned, and every report
// records which one it used and its checksum. Two runs agree on what they
// sent, or the report says they did not.
//
//   prose: ModelFabric's own docs, markup stripped, asked for a summary.
//   code:  ModelFabric's own Go source, asked to continue the file.
//
// A new snapshot is a new version (prose-v2), never an edit of v1: a result
// is only comparable to another made from the same text.

//go:embed corpus/*.txt.gz
var corpusFS embed.FS

// Corpus is one named prompt text.
type Corpus struct {
	Name    string `json:"name"`    // prose | code
	Version string `json:"version"` // v1
	SHA256  string `json:"sha256"`  // of the text, so a report names exactly what was sent
	// Ask is the instruction after the text. It decides what the model
	// generates, and so how much a speculative drafter can guess.
	Ask  string `json:"ask"`
	text string
}

var corpusAsk = map[string]string{
	"prose": "Summarise the document above in detail, section by section.",
	"code":  "Continue writing this Go source file from where it stops.",
}

const corpusVersion = "v1"

var (
	corpusMu    sync.Mutex
	corpusCache = map[string]*Corpus{}
)

// LoadCorpus returns the named corpus.
func LoadCorpus(name string) (*Corpus, error) {
	corpusMu.Lock()
	defer corpusMu.Unlock()
	if c, ok := corpusCache[name]; ok {
		return c, nil
	}
	ask, ok := corpusAsk[name]
	if !ok {
		return nil, fmt.Errorf("no prompt set %q; use prose or code", name)
	}
	gz, err := corpusFS.ReadFile("corpus/" + name + "-" + corpusVersion + ".txt.gz")
	if err != nil {
		return nil, err
	}
	r, err := gzip.NewReader(bytes.NewReader(gz))
	if err != nil {
		return nil, err
	}
	b, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(b)
	c := &Corpus{Name: name, Version: corpusVersion, SHA256: hex.EncodeToString(sum[:]), Ask: ask, text: string(b)}
	corpusCache[name] = c
	return c, nil
}

// Text is at least n bytes of the corpus, from the start, wrapping around
// when n is longer than the text: prefill speed does not depend on whether
// the words repeat, and the 64K test is longer than either snapshot.
func (c *Corpus) Text(n int) string {
	if n <= len(c.text) {
		return c.text[:n]
	}
	var b bytes.Buffer
	for b.Len() < n {
		b.WriteString(c.text)
		b.WriteString("\n\n")
	}
	return b.String()[:n]
}
