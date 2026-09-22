package server

import (
	"fmt"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gregbugaj/modelfabric/internal/router"
)

func peekOf(t *testing.T, body string) (peeked, string) {
	t.Helper()
	r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	p := peekRequest(r)
	// Whatever was peeked, the request must still yield the whole body: ModelFabric
	// is a proxy, and a body it has half-consumed is a request it has broken.
	rest, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatalf("reading the body back: %v", err)
	}
	return p, string(rest)
}

// The defect this fixes: a body over the old 64KiB cap was given up on, the
// model read as empty, llmdTarget("") declined it, and the request went to
// ModelFabric's own router with nothing said — so llm-d never saw a long agent
// conversation, which is the workload it exists for.
func TestTheModelIsReadFromABodyOfAnySize(t *testing.T) {
	for _, n := range []int{0, 1 << 10, 60 << 10, 70 << 10, 300 << 10, bodyPeekMax + 1} {
		body := fmt.Sprintf(`{"model":"qwen/qwen3.8-27b","messages":[{"role":"user","content":%q}]}`,
			strings.Repeat("x", n))
		p, rest := peekOf(t, body)
		if p.Model != "qwen/qwen3.8-27b" {
			t.Errorf("%d-byte filler: model read as %q", n, p.Model)
		}
		if rest != body {
			t.Errorf("%d-byte filler: the body did not survive the peek (%d bytes back, want %d)",
				n, len(rest), len(body))
		}
		// Only a body ModelFabric held entirely may be rewritten.
		if want := len(body) <= bodyPeekMax; p.Whole != want {
			t.Errorf("%d-byte filler: Whole=%v, want %v (body %d, cap %d)",
				n, p.Whole, want, len(body), bodyPeekMax)
		}
	}
}

// The boundary itself, at the sizes that were measured failing.
func TestTheOldCapNoLongerDecidesAnything(t *testing.T) {
	for _, n := range []int{61510, 71750} {
		body := `{"model":"m","messages":[{"role":"user","content":"` +
			strings.Repeat("y", n) + `"}]}`
		if p, _ := peekOf(t, body); p.Model != "m" {
			t.Errorf("a %d-byte body read no model, which is how llm-d was bypassed", len(body))
		}
	}
}

// A body with the model last, which nothing guarantees against: JSON has no
// field order, and a client that serialises messages first is within its rights.
func TestTheModelIsFoundWhereverItIs(t *testing.T) {
	body := `{"messages":[{"role":"user","content":"hello"}],"stream":false,"model":"m"}`
	if p, _ := peekOf(t, body); p.Model != "m" {
		t.Errorf("model last: read %q", p.Model)
	}
}

// "model" occurs inside conversations — a request asking about one, a tool call
// naming one — and the request's own field is the root object's.
func TestAModelNamedInsideTheConversationIsNotTheRequestsModel(t *testing.T) {
	for _, body := range []string{
		`{"messages":[{"role":"user","content":"what does \"model\": \"gpt-4\" mean?"}],"model":"m"}`,
		`{"messages":[{"role":"user","content":[{"type":"text","text":"x"}],"model":"decoy"}],"model":"m"}`,
		`{"tools":[{"function":{"name":"f","parameters":{"model":"decoy"}}}],"model":"m"}`,
	} {
		if p, _ := peekOf(t, body); p.Model != "m" {
			t.Errorf("read %q from %s", p.Model, body)
		}
	}
}

// Past the cap the model comes out of the prefix, and the decoy is put first so
// the scan has to reject it rather than merely reach the real one sooner.
func TestThePrefixScanOnlyReadsTheRootObject(t *testing.T) {
	filler := strings.Repeat("z", bodyPeekMax+1)
	body := fmt.Sprintf(
		`{"messages":[{"role":"user","model":"decoy","content":"short"}],`+
			`"model":"real","padding":%q}`, filler)
	p, rest := peekOf(t, body)
	if p.Model != "real" {
		t.Errorf("read %q, want the root object's model", p.Model)
	}
	if p.Whole {
		t.Error("a body past the cap is not whole")
	}
	if rest != body {
		t.Errorf("the oversized body did not survive: %d bytes back, want %d", len(rest), len(body))
	}
}

// And when the root's own field is past the prefix, nothing is returned — a
// nested one must not stand in for it.
func TestADecoyNeverStandsInForAModelBeyondThePrefix(t *testing.T) {
	filler := strings.Repeat("z", bodyPeekMax+1)
	body := fmt.Sprintf(
		`{"messages":[{"role":"user","model":"decoy","content":%q}],"model":"real"}`, filler)
	p, rest := peekOf(t, body)
	if p.Model != "" {
		t.Errorf("read %q from beyond the prefix; only the root object counts", p.Model)
	}
	if rest != body {
		t.Errorf("the oversized body did not survive: %d bytes back, want %d", len(rest), len(body))
	}
}

// Truncation in the middle of the field being looked for reads as not found,
// rather than as half a model id.
func TestATruncatedModelIsNotAModel(t *testing.T) {
	for _, buf := range []string{
		`{"messages":[],"mod`,
		`{"messages":[],"model"`,
		`{"messages":[],"model":`,
		`{"messages":[],"model":"qwen/qwen3`,
	} {
		if got, ok := topLevelString([]byte(buf), "model"); ok {
			t.Errorf("%q yielded %q", buf, got)
		}
	}
}

// A field that is present and not a string is not a model either.
func TestANonStringModelIsRefused(t *testing.T) {
	for _, buf := range []string{`{"model":7}`, `{"model":null}`, `{"model":{"name":"m"}}`, `{"model":["m"]}`} {
		if got, ok := topLevelString([]byte(buf), "model"); ok {
			t.Errorf("%s yielded %q", buf, got)
		}
	}
}

// Not JSON, or not a JSON body at all: the peek reports nothing and hands the
// body on untouched. An audio upload is multipart, and buffering one to look for
// a field it does not have is the thing the old cap was protecting against.
func TestANonJSONBodyIsLeftAlone(t *testing.T) {
	r := httptest.NewRequest("POST", "/v1/audio/transcriptions", strings.NewReader("--boundary\r\n"))
	r.Header.Set("Content-Type", "multipart/form-data; boundary=boundary")
	if p := peekRequest(r); p.Model != "" || p.Body != nil || p.Whole {
		t.Errorf("a multipart upload was peeked: %+v", p)
	}
	if got, _ := io.ReadAll(r.Body); string(got) != "--boundary\r\n" {
		t.Errorf("the body was consumed: %q", got)
	}
	p, rest := peekOf(t, `not json at all`)
	if p.Model != "" {
		t.Errorf("a model came out of non-JSON: %q", p.Model)
	}
	if !p.Whole || rest != "not json at all" {
		t.Errorf("a small non-JSON body should still be whole and intact: %+v %q", p, rest)
	}
}

// Image detection has to work past the cap too, or the vision profile ModelFabric
// sends to llm-d is never sent for a real image — an image request is usually
// larger than any prefix worth holding. Past the cap it is deliberately the
// loose direction: a text request on an engine that can read images works, and
// the reverse fails inside llama.cpp.
func TestAnImageIsDetectedPastTheCap(t *testing.T) {
	big := strings.Repeat("A", bodyPeekMax+1) // a data URI the size of a real photo
	body := fmt.Sprintf(`{"model":"m","messages":[{"role":"user","content":[`+
		`{"type":"text","text":"what is this?"},`+
		`{"type":"image_url","image_url":{"url":"data:image/png;base64,%s"}}]}]}`, big)
	p, _ := peekOf(t, body)
	if p.Whole {
		t.Fatal("this body is meant to exceed the cap")
	}
	if !carriesImage(p, router.CarriesImage) {
		t.Error("an image past the cap was not detected, so llm-d would be told nothing")
	}
}

// And a long text conversation is not an image, or every request would be
// confined to the engines holding a projector. Past the cap, so this is the loose
// path rather than the precise one.
func TestALongTextConversationIsNotAnImage(t *testing.T) {
	body := fmt.Sprintf(`{"model":"m","messages":[{"role":"user","content":%q}]}`,
		strings.Repeat("the quick brown fox ", (bodyPeekMax/20)+1))
	p, _ := peekOf(t, body)
	if p.Whole {
		t.Fatal("this body is meant to exceed the cap, to exercise the loose check")
	}
	if carriesImage(p, router.CarriesImage) {
		t.Error("a text conversation was read as carrying an image")
	}
}

// The precise path is used whenever the body is whole, so prose about images
// does not confine a text request.
func TestProseAboutAnImageIsNotAnImage(t *testing.T) {
	body := `{"model":"m","messages":[{"role":"user","content":"explain the \"image_url\" content part"}]}`
	p, _ := peekOf(t, body)
	if !p.Whole {
		t.Fatal("a small body should be whole")
	}
	if carriesImage(p, router.CarriesImage) {
		t.Error("a request that mentions image_url was read as carrying an image")
	}
}

func TestBodyPeekDoesNotHoldMoreThanItsCap(t *testing.T) {
	body := `{"model":"m","x":"` + strings.Repeat("q", 2*bodyPeekMax) + `"}`
	p, rest := peekOf(t, body)
	if len(p.Body) > bodyPeekMax+1 {
		t.Errorf("held %d bytes for a cap of %d", len(p.Body), bodyPeekMax)
	}
	if rest != body {
		t.Errorf("the body did not survive: %d bytes, want %d", len(rest), len(body))
	}
}
