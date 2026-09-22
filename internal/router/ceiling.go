package router

import "github.com/gregbugaj/modelfabric/internal/outputlimit"

func capOutput(body []byte, limit int) []byte {
	out, _ := outputlimit.Apply(body, limit)
	return out
}

// generates reports whether a path produces tokens, and so has an output
// length worth bounding. Embeddings, rerank and transcription do not.
func generates(path string) bool {
	switch path {
	case "/v1/chat/completions", "/v1/completions", "/v1/responses", "/v1/messages":
		return true
	}
	return false
}
