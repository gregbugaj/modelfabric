package router

import (
	"strings"
	"testing"

	"github.com/gregbugaj/modelfabric/internal/mesh"
)

// A chat request to an embedding model came back as a 502 quoting llama.cpp's
// "the current context does not logits computation", and an embeddings
// request to a chat model as a 502 quoting a 501. Both are the caller's
// mistake and must say so; a peer's kind is not known here and is left to it.
func TestWrongKindOfModelIsRefusedByName(t *testing.T) {
	local := func(embedding bool) mesh.Candidate { return mesh.Candidate{Local: true, Embedding: embedding} }
	peer := mesh.Candidate{}
	tests := []struct {
		name string
		path string
		cs   []mesh.Candidate
		want string
	}{
		{name: "chat on an embedding model is refused", path: "/v1/chat/completions", cs: []mesh.Candidate{local(true)}, want: "is an embedding model"},
		{name: "responses on an embedding model is refused", path: "/v1/responses", cs: []mesh.Candidate{local(true)}, want: "is an embedding model"},
		{name: "embeddings on a chat model is refused", path: "/v1/embeddings", cs: []mesh.Candidate{local(false)}, want: "is not an embedding model"},
		{name: "embeddings on an embedding model passes", path: "/v1/embeddings", cs: []mesh.Candidate{local(true)}},
		{name: "chat on a chat model passes", path: "/v1/chat/completions", cs: []mesh.Candidate{local(false)}},
		{name: "a peer is left to judge its own engines", path: "/v1/embeddings", cs: []mesh.Candidate{local(false), peer}},
		{name: "rerank is not judged", path: "/v1/rerank", cs: []mesh.Candidate{local(false)}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := wrongKind(tc.path, "m", tc.cs)
			if (tc.want == "") != (got == "") || !strings.Contains(got, tc.want) {
				t.Fatalf("got %q, want it to contain %q", got, tc.want)
			}
		})
	}
}
