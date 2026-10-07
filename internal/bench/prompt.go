package bench

import (
	"context"
	"fmt"
)

// prompter builds prompts of an exact length in tokens, as the engine counts
// them: "pp4096" means 4096 prompt tokens, chat template included, not 4096
// words or some multiple of characters. It cuts the corpus with the engine's
// own tokenizer and subtracts what the template adds, measured once.
type prompter struct {
	c        *client
	corpus   *Corpus
	toks     []int // the corpus, tokenized, as far as anyone has needed
	overhead int   // tokens the chat template adds around a message
	exact    bool  // false: the engine has no tokenizer endpoint; lengths are estimates
}

const charsPerToken = 4 // the estimate when the engine cannot count

func (p *prompter) calibrate(ctx context.Context) error {
	probe := "Reply with one word."
	toks, err := p.c.tokenize(ctx, probe)
	p.exact = err == nil
	s := p.c.generate(ctx, probe, 1)
	if s.err != nil {
		return fmt.Errorf("the engine did not answer a one-token request: %w", s.err)
	}
	if p.exact {
		p.overhead = max(0, s.promptTokens-len(toks))
	} else {
		p.overhead = max(0, s.promptTokens-len(probe)/charsPerToken)
	}
	return nil
}

// build combines a tagged opening, corpus text, and instruction into n tokens.
// Distinct tags prevent prefix reuse between independent requests.
func (p *prompter) build(ctx context.Context, n int, tag string) (string, error) {
	opening := fmt.Sprintf("Reference text %q follows.\n\n", tag)
	ask := "\n\n" + p.corpus.Ask
	if !p.exact {
		body := max(0, n-p.overhead)*charsPerToken - len(opening) - len(ask)
		return opening + p.corpus.Text(max(0, body)) + ask, nil
	}
	fixed, err := p.c.tokenize(ctx, opening+ask)
	if err != nil {
		return "", err
	}
	want := n - p.overhead - len(fixed)
	if want <= 0 {
		return opening + ask, nil
	}
	if err := p.ensure(ctx, want); err != nil {
		return "", err
	}
	body, err := p.c.detokenize(ctx, p.toks[:want])
	if err != nil {
		return "", err
	}
	return opening + body + ask, nil
}

func (p *prompter) ensure(ctx context.Context, n int) error {
	if len(p.toks) >= n {
		return nil
	}
	size := max(n*5, 64<<10)
	for {
		toks, err := p.c.tokenize(ctx, p.corpus.Text(size))
		if err != nil {
			return err
		}
		if len(toks) >= n {
			p.toks = toks
			return nil
		}
		size *= 2
	}
}
