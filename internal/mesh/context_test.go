package mesh

import (
	"strings"
	"testing"
)

// Captured /props responses use both total-pool and per-slot context units;
// reconciliation must accept either when it matches the requested capacity.
func TestReconcileContextAcceptsEitherBuildsReading(t *testing.T) {
	for _, tc := range []struct {
		name               string
		asked, slots, pool int
	}{
		{"LM Studio cuda12-avx2@2.40.0 reports the pool", 65536, 4, 262144},
		{"Metal advsimd@2.41.0, one slot, pool and per-slot alike", 65536, 1, 65536},
		{"upstream b11153 reports per slot", 65536, 2, 65536},
	} {
		got, mismatch := reconcileContext(tc.asked, tc.slots, tc.pool)
		if got != tc.asked {
			t.Errorf("%s: per-request = %d, want %d", tc.name, got, tc.asked)
		}
		if mismatch != "" {
			t.Errorf("%s: reported a disagreement that is not one: %s", tc.name, mismatch)
		}
	}
}

// Regression: report the effective engine context when load settings are ignored,
// rather than advertising the larger requested context and routing oversized prompts.
func TestReconcileContextReportsAnEngineRunningSomethingElse(t *testing.T) {
	got, mismatch := reconcileContext(65536, 4, 131072)
	if got != 32768 {
		t.Errorf("per-request = %d, want 32768: the engine is what will refuse the request", got)
	}
	if mismatch == "" {
		t.Fatal("no disagreement reported for an engine running half what was asked")
	}
	for _, want := range []string{"65536", "131072", "4 slots", "32768"} {
		if !strings.Contains(mismatch, want) {
			t.Errorf("the message does not name %s: %q", want, mismatch)
		}
	}
}

// A reported value equal to the requested per-slot context is ambiguous with
// multiple slots: some builds report per-slot capacity, others report the pool.
// Accept the per-slot interpretation because /props cannot distinguish the cases.
func TestReconcileContextIsHonestAboutTheAmbiguousCase(t *testing.T) {
	got, mismatch := reconcileContext(65536, 2, 65536)
	if got != 65536 {
		t.Errorf("per-request = %d, want the asked value on the optimistic reading", got)
	}
	if mismatch != "" {
		t.Errorf("this combination is ambiguous, not wrong; complaining here would "+
			"fire on every upstream-build engine: %q", mismatch)
	}
}

// Without reported capacity, preserve the requested context as unverified;
// mlx-lm does not expose /props.
func TestReconcileContextKeepsTheAskedValueWhenTheEngineIsSilent(t *testing.T) {
	got, mismatch := reconcileContext(65536, 2, 0)
	if got != 65536 || mismatch != "" {
		t.Errorf("silent engine: got %d %q, want 65536 and no complaint", got, mismatch)
	}
}

func TestReconcileContextTakesTheEnginesWordWhenNothingWasAsked(t *testing.T) {
	got, mismatch := reconcileContext(0, 1, 40960)
	if got != 40960 || mismatch != "" {
		t.Errorf("got %d %q, want 40960 and no complaint", got, mismatch)
	}
}

func TestReconcileContextDoesNotInventADivision(t *testing.T) {
	got, _ := reconcileContext(65536, 3, 100000)
	if got != 100000 {
		t.Errorf("per-request = %d, want the engine's figure unchanged", got)
	}
}
