package ops

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"testing/synctest"
	"time"
)

func TestRunningJournalPrunesWithoutNewOperations(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		j, err := NewJournal(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		op, _ := j.Begin("load", "model", "key")
		j.Succeed(op.ID, "instance")
		running, _ := j.Begin("load", "slow-model", "running-key")
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := make(chan struct{})
		go func() { j.RunPruner(ctx); close(done) }()
		synctest.Wait()
		time.Sleep(24*time.Hour + 2*time.Minute)
		synctest.Wait()
		if _, ok := j.Get(op.ID); ok {
			t.Error("expired operation was not pruned while idle")
		}
		if _, err := os.Stat(j.path(op.ID)); !os.IsNotExist(err) {
			t.Errorf("expired file still exists: %v", err)
		}
		if _, ok := j.Get(running.ID); !ok {
			t.Error("running operation was pruned")
		}
		cancel()
		synctest.Wait()
		select {
		case <-done:
		default:
			t.Error("pruner did not stop on cancellation")
		}
	})
}

func TestStartupPrunesOnlyExpiredSettledOperations(t *testing.T) {
	for _, tc := range []struct {
		name  string
		state State
		age   time.Duration
		ended bool
		keep  bool
	}{
		{"old success expires", StateSucceeded, 48 * time.Hour, true, false},
		{"old failure expires", StateFailed, 48 * time.Hour, true, false},
		{"old cancellation expires", StateCancelled, 48 * time.Hour, true, false},
		{"recent success stays", StateSucceeded, time.Hour, true, true},
		{"interrupted work is reported before expiry", StateRunning, 48 * time.Hour, false, true},
		{"unknown end time stays", StateSucceeded, 48 * time.Hour, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			at := time.Now().Add(-tc.age)
			op := Operation{ID: "op-old", State: tc.state, StartedAt: at}
			if tc.ended {
				op.EndedAt = &at
			}
			b, err := json.Marshal(op)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, "op-old.json")
			if err := os.WriteFile(path, b, 0o600); err != nil {
				t.Fatal(err)
			}
			j, err := NewJournal(dir)
			if err != nil {
				t.Fatal(err)
			}
			if _, ok := j.Get(op.ID); ok != tc.keep {
				t.Errorf("entry exists=%v, want %v", ok, tc.keep)
			}
			_, err = os.Stat(path)
			if tc.keep && err != nil || !tc.keep && !os.IsNotExist(err) {
				t.Errorf("journal file: %v, keep=%v", err, tc.keep)
			}
		})
	}
}

// A failed removal must remain eligible for another pass; dropping the index
// first made the failed file invisible until the next process restart.
func TestPruneRetriesFailedRemoval(t *testing.T) {
	j, err := NewJournal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	at := time.Now().Add(-48 * time.Hour)
	j.byID["op-old"] = &Operation{ID: "op-old", State: StateSucceeded, EndedAt: &at}
	path := j.path("op-old")
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	child := filepath.Join(path, "block-removal")
	if err := os.WriteFile(child, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	j.Prune()
	if _, ok := j.Get("op-old"); !ok {
		t.Fatal("failed removal was forgotten")
	}
	if err := os.Remove(child); err != nil {
		t.Fatal(err)
	}
	j.Prune()
	if _, ok := j.Get("op-old"); ok {
		t.Fatal("successful retry did not remove entry")
	}
}

// A journal write that fails used to be dropped: Marshal, WriteFile and Rename
// errors all returned silently, so the in-memory index reported an operation
// as recorded while nothing had reached disk and nothing would survive a
// restart. The failure is now visible on the operation itself.
func TestAFailedJournalWriteIsReportedNotSwallowed(t *testing.T) {
	dir := t.TempDir()
	j, err := NewJournal(dir)
	if err != nil {
		t.Fatal(err)
	}
	op, _ := j.Begin("load", "m", "k")
	if got, _ := j.Get(op.ID); got.PersistError != "" {
		t.Fatalf("a healthy write reported an error: %q", got.PersistError)
	}
	if _, err := os.Stat(filepath.Join(dir, op.ID+".json")); err != nil {
		t.Fatalf("the operation was not written: %v", err)
	}

	// Make the directory unwritable so the next write cannot land.
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Skipf("cannot make the journal dir read-only here: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	if os.Geteuid() == 0 {
		t.Skip("root ignores the directory mode")
	}
	j.Succeed(op.ID, "done")
	got, ok := j.Get(op.ID)
	if !ok {
		t.Fatal("the operation vanished from the index")
	}
	if got.PersistError == "" {
		t.Fatal("a write that could not land was reported as journalled")
	}
}
