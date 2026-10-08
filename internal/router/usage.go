package router

import (
	"bytes"
	"strconv"
)

// usage is what an engine reported about a request, zero where it did not
// say. prompt and completion are token counts every OpenAI-shaped engine
// gives; the rest come from llama.cpp's own "timings" block and are there for
// the developer log.
type usage struct {
	prompt, completion int64
	// cached is how many of the prompt tokens were served from cache.
	cached int64
	// promptMillis is the time spent reading the uncached prompt, and
	// tokensPerSec the rate of generation.
	promptMillis, tokensPerSec float64
	// drafted and draftAccepted count speculative decoding's tokens.
	drafted, draftAccepted int64
}

// Usage arrives at the end of JSON responses or in the final SSE chunks;
// retaining a bounded tail avoids buffering the full response.
const usageTail = 8 << 10

func keepTail(tail, chunk []byte) []byte {
	if len(chunk) >= usageTail {
		return append(tail[:0], chunk[len(chunk)-usageTail:]...)
	}
	if over := len(tail) + len(chunk) - usageTail; over > 0 {
		tail = append(tail[:0], tail[over:]...)
	}
	return append(tail, chunk...)
}

// readUsage extracts counts from a possibly truncated JSON or SSE tail.
// Missing or malformed counts are zero (unknown).
func readUsage(tail []byte) usage {
	u := usage{prompt: lastNumber(tail, `"prompt_tokens"`), completion: lastNumber(tail, `"completion_tokens"`)}
	// OpenAI's name for it, then llama.cpp's own.
	u.cached = lastNumber(tail, `"cached_tokens"`)
	if u.cached == 0 {
		u.cached = lastNumber(tail, `"cache_n"`)
	}
	u.promptMillis = lastFloat(tail, `"prompt_ms"`)
	u.tokensPerSec = lastFloat(tail, `"predicted_per_second"`)
	u.drafted, u.draftAccepted = lastNumber(tail, `"draft_n"`), lastNumber(tail, `"draft_n_accepted"`)
	// A stream carries a usage block only when the caller asked for one
	// (stream_options.include_usage), and most agents do not. llama.cpp puts
	// its timings in the last chunk regardless, and they hold the same
	// counts: tokens read, tokens taken from cache, tokens generated. Without
	// this a streamed request finished with no size at all, in the developer
	// log and in what placement remembers of the conversation.
	if u.prompt == 0 {
		if read, hit := lastNumber(tail, `"prompt_n"`), lastNumber(tail, `"cache_n"`); read+hit > 0 {
			u.prompt, u.cached = read+hit, hit
		}
	}
	if u.completion == 0 {
		u.completion = lastNumber(tail, `"predicted_n"`)
	}
	return u
}

// lastNumber is the integer after the last occurrence of key and a colon.
func lastNumber(b []byte, key string) int64 {
	i := bytes.LastIndex(b, []byte(key))
	if i < 0 {
		return 0
	}
	b = bytes.TrimLeft(b[i+len(key):], " \t")
	if len(b) == 0 || b[0] != ':' {
		return 0
	}
	b = bytes.TrimLeft(b[1:], " \t")
	j := 0
	for j < len(b) && b[j] >= '0' && b[j] <= '9' {
		j++
	}
	n, err := strconv.ParseInt(string(b[:j]), 10, 64)
	if err != nil {
		return 0
	}
	return n
}

// lastFloat is lastNumber for a value with a fraction.
func lastFloat(b []byte, key string) float64 {
	i := bytes.LastIndex(b, []byte(key))
	if i < 0 {
		return 0
	}
	b = bytes.TrimLeft(b[i+len(key):], " \t")
	if len(b) == 0 || b[0] != ':' {
		return 0
	}
	b = bytes.TrimLeft(b[1:], " \t")
	j := 0
	for j < len(b) && (b[j] >= '0' && b[j] <= '9' || b[j] == '.') {
		j++
	}
	f, err := strconv.ParseFloat(string(b[:j]), 64)
	if err != nil {
		return 0
	}
	return f
}
