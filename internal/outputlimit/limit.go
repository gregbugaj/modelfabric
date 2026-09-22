// Package outputlimit supplies a default generation limit when a request has
// none. The router and engine shim share the rule because llm-d bypasses the
// router, and a fix applied on just one path leaves the other unbounded.
package outputlimit

import (
	"bytes"
	"encoding/json"
	"strconv"
)

// Apply fills in max_tokens only for JSON objects without a stated limit.
// A request without a ceiling once generated 130,000 tokens for ninety minutes
// on a shared engine. Explicit limits remain the caller's decision; malformed
// requests remain the engine's to reject. changed reports a rewritten body.
func Apply(body []byte, limit int) (out []byte, changed bool) {
	if limit <= 0 {
		return body, false
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil || fields == nil {
		return body, false
	}
	for _, key := range []string{"max_tokens", "max_completion_tokens", "n_predict"} {
		// Generated clients use null for an omitted optional field.
		if raw, ok := fields[key]; ok && !bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return body, false
		}
	}
	fields["max_tokens"] = json.RawMessage(strconv.Itoa(limit))
	out, err := json.Marshal(fields)
	if err != nil {
		return body, false
	}
	return out, true
}
