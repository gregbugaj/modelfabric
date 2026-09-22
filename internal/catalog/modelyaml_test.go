package catalog

import (
	"math"
	"testing"
)

// model.yaml is a document from disk, so its numbers are input: a NaN, an
// infinity or 1e100 used to be narrowed straight into engine arguments and
// memory estimates.
func TestModelYAMLNumbersAreValidated(t *testing.T) {
	if _, ok := number(math.NaN()); ok {
		t.Error("NaN was accepted as a number")
	}
	if _, ok := number(math.Inf(1)); ok {
		t.Error("+Inf was accepted as a number")
	}
	if _, ok := number(1e308); !ok {
		t.Error("a large but finite number should still be accepted")
	}
	if _, ok := wholeInRange(3.5, 0, 100); ok {
		t.Error("a fraction was accepted as a whole number")
	}
	if _, ok := wholeInRange(1e30, 0, 1<<20); ok {
		t.Error("a value past the range was accepted")
	}
	if k, ok := wholeInRange(40, 0, 1<<20); !ok || k != 40 {
		t.Errorf("an ordinary top-k was rejected: %d %v", k, ok)
	}
}

// LM Studio writes {checked: false} with no value for a sampler that is off.
func TestUncheckedSamplerNeedsNoValue(t *testing.T) {
	_, on, err := checkedValue(map[string]any{"checked": false})
	if err != nil {
		t.Fatalf("an unchecked sampler with no value was rejected: %v", err)
	}
	if on {
		t.Error("an unchecked sampler reported as on")
	}
	// A checked one still has to carry a number.
	if _, _, err := checkedValue(map[string]any{"checked": true}); err == nil {
		t.Error("a checked sampler with no value was accepted")
	}
}
