package router

import (
	"bytes"
	"strconv"
)

type usage struct{ prompt, completion int64 }

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
	return usage{prompt: lastNumber(tail, `"prompt_tokens"`), completion: lastNumber(tail, `"completion_tokens"`)}
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
