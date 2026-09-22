package mesh

import (
	"strings"
	"testing"
)

// The three builds this fleet actually runs, measured 2026-09-25. The same
// /props field is a pool on two of them and a per-slot figure on the third, so
// ModelFabric has to accept either reading rather than pick one.
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

// The failure this exists to catch, in the shape it actually had: a load whose
// settings were silently ignored fell back to the model's saved defaults —
// 32768 where 65536 was asked for — `mfsh ps` reported the number ModelFabric asked
// for, and every long conversation routed there overflowed with nothing saying
// why.
func TestReconcileContextReportsAnEngineRunningSomethingElse(t *testing.T) {
	// Asked 65536 per request across 4 slots, so a pool of 262144. The engine
	// came up at 32768 per request: a pool of 131072.
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

// One combination cannot be resolved, and pretending otherwise would be worse
// than admitting it: a pool equal to what was asked, with more than one slot.
// That is either a build reporting per slot and running exactly what was asked,
// or a build reporting the pool and running a fraction of it. Nothing in /props
// distinguishes them.
//
// ModelFabric takes the optimistic reading, deliberately. The pessimistic one would
// cry wolf on every upstream-build engine in the fleet — the common case — and a
// warning that fires constantly is a warning nobody reads. The case it gives up
// on needs the fallback context to land on exactly asked/slots, which is a
// coincidence rather than the failure mode: the real one was 32768 against
// 65536 across 4 slots, and that is caught above.
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

// An engine that says nothing leaves what ModelFabric asked for, unverified. mlx-lm
// serves no /props, and answering "unknown" would take the headroom figure away
// from every caller rather than leave it unconfirmed.
func TestReconcileContextKeepsTheAskedValueWhenTheEngineIsSilent(t *testing.T) {
	got, mismatch := reconcileContext(65536, 2, 0)
	if got != 65536 || mismatch != "" {
		t.Errorf("silent engine: got %d %q, want 65536 and no complaint", got, mismatch)
	}
}

// Nothing was asked for — an engine ModelFabric fronts but did not launch. The
// engine's own number is all there is, and it is not a disagreement.
func TestReconcileContextTakesTheEnginesWordWhenNothingWasAsked(t *testing.T) {
	got, mismatch := reconcileContext(0, 1, 40960)
	if got != 40960 || mismatch != "" {
		t.Errorf("got %d %q, want 40960 and no complaint", got, mismatch)
	}
}

// A pool that does not divide by the slot count is taken as it stands rather
// than rounded into something plausible-looking.
func TestReconcileContextDoesNotInventADivision(t *testing.T) {
	got, _ := reconcileContext(65536, 3, 100000)
	if got != 100000 {
		t.Errorf("per-request = %d, want the engine's figure unchanged", got)
	}
}
