package engineshim

import (
	"bytes"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/gregbugaj/modelfabric/internal/outputlimit"
)

// Enforce the output ceiling at the shim because llm-d routes directly
// from Envoy to the shim, bypassing the router.

// maxPeek bounds how much of a body is read to inspect it. A chat request
// carrying images is large, and reading an unbounded body into memory to add
// one field would trade a runaway generation for a runaway allocation.
const maxPeek = 8 << 20

// capOutput fills in max_tokens when the request names no limit of its own, and
// reports whether it rewrote the body. The body is always left readable by the
// proxy, rewritten or not.
func capOutput(r *http.Request, limit int) bool {
	if limit <= 0 || r.Body == nil || r.ContentLength <= 0 || r.ContentLength > maxPeek {
		return false
	}
	buf, err := io.ReadAll(io.LimitReader(r.Body, r.ContentLength))
	if err != nil {
		return false
	}
	_ = r.Body.Close()
	// Restored before every return: the proxy must be able to read the body
	// whatever this function decides.
	restore := func(b []byte) {
		r.Body = io.NopCloser(bytes.NewReader(b))
		r.ContentLength = int64(len(b))
		r.Header.Set("Content-Length", strconv.Itoa(len(b)))
	}

	out, changed := outputlimit.Apply(buf, limit)
	restore(out)
	return changed
}

// generates reports whether a path produces tokens. Matched by suffix because
// whoever routed the request decides the prefix: llm-d's Envoy passes paths
// through, but another scheduler may mount them elsewhere.
func generates(path string) bool {
	for _, p := range []string{"/chat/completions", "/completions", "/responses", "/messages"} {
		if strings.HasSuffix(path, p) {
			return true
		}
	}
	return false
}
