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

// Versioned corpora and checksums make benchmark inputs reproducible.
// Prompt predictability affects speculative decoding performance.
//
// prose: documentation text requested as a summary.
// code: Go source requested as a continuation.
//
// New snapshots require new versions; never edit an existing corpus version.

//go:embed corpus/*.txt.gz
var corpusFS embed.FS

type Corpus struct {
	Name    string `json:"name"` // prose | code
	Version string `json:"version"`
	SHA256  string `json:"sha256"` // of the text, so a report names exactly what was sent
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
