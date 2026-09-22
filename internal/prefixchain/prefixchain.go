// Package prefixchain identifies a request's prompt by a chain of block
// hashes: each block's hash includes the one before it, so one hash names the
// whole prompt up to that point and a longer prompt's chain starts with a
// shorter one's. ModelFabric's router uses the head of it for prefix affinity;
// the disk prompt cache uses all of it, from the engine shim. One package, so
// a conversation hashes the same whichever way it arrived — and a leaf, since
// the shim cannot import the router (mesh imports the shim).
package prefixchain

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"strconv"
	"strings"
)

// Block is how many bytes of prompt each link covers.
const Block = 256

// ColdMaxPrefix is how much of a prompt the disk cache's chain covers. Far
// more than affinity needs: affinity only has to tell conversations apart,
// while the disk cache has to tell how much of one a saved slot holds, and a
// long agent conversation is hundreds of kilobytes.
const ColdMaxPrefix = 4 << 20

// Chain is the block-hash chain over the first maxPrefix bytes of a request's
// prompt, scoped by model. It understands the OpenAI chat, completions,
// Responses and Anthropic shapes; anything else yields no chain.
func Chain(model string, body []byte, maxPrefix int) [][32]byte {
	var req struct {
		Messages []struct {
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
		System json.RawMessage `json:"system"` // Anthropic
		Prompt json.RawMessage `json:"prompt"` // completions
		Input  json.RawMessage `json:"input"`  // Responses, embeddings
	}
	if json.Unmarshal(body, &req) != nil {
		return nil
	}
	var b strings.Builder
	add := func(raw json.RawMessage) {
		if len(raw) == 0 || b.Len() >= maxPrefix {
			return
		}
		// The raw JSON is a stable identity for string and multi-part content
		// alike, without interpreting every part type.
		b.Write(raw)
		b.WriteByte(0)
	}
	add(req.System)
	for _, m := range req.Messages {
		add(m.Content)
	}
	add(req.Prompt)
	add(req.Input)
	text := b.String()
	if len(text) > maxPrefix {
		text = text[:maxPrefix]
	}

	// Only whole blocks count: a partial trailing block is where requests
	// differ, and hashing it would never match anything.
	n := len(text) / Block
	if n == 0 {
		return nil
	}
	out := make([][32]byte, 0, n)
	prev := sha256.Sum256([]byte(model))
	for i := 0; i < n; i++ {
		h := sha256.New()
		h.Write(prev[:])
		h.Write([]byte(text[i*Block : (i+1)*Block]))
		copy(prev[:], h.Sum(nil))
		out = append(out, prev)
	}
	return out
}

// WithSlot pins a request body to one of the engine's slots (llama.cpp's
// id_slot), inserted as the object's first field.
func WithSlot(body []byte, slot int) []byte {
	i := bytes.IndexByte(body, '{')
	if i < 0 {
		return body
	}
	pin := []byte(`"id_slot":` + strconv.Itoa(slot))
	// An empty object takes no comma after the new field.
	if rest := bytes.TrimLeft(body[i+1:], " \t\r\n"); len(rest) > 0 && rest[0] != '}' {
		pin = append(pin, ',')
	}
	out := make([]byte, 0, len(body)+len(pin))
	out = append(out, body[:i+1]...)
	out = append(out, pin...)
	return append(out, body[i+1:]...)
}
