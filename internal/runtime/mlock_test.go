package runtime

import "testing"

func TestFitLoadModeDropsMlockThatCannotHold(t *testing.T) {
	oldAvail, oldLimit := memAvailable, memlockLimit
	defer func() { memAvailable, memlockLimit = oldAvail, oldLimit }()
	const gb = int64(1) << 30

	cases := []struct {
		name  string
		avail int64
		limit uint64
		mode  string
		size  int64
		want  string
	}{
		{"fits", 64 * gb, ^uint64(0), "mmap+mlock", 16 * gb, "mmap+mlock"},
		{"small memlock limit", 64 * gb, 8 << 20, "mmap+mlock", 16 * gb, "mmap"},
		{"larger than RAM", 15 * gb, ^uint64(0), "mmap+mlock", 16 * gb, "mmap"},
		{"mlock alone", 15 * gb, ^uint64(0), "mlock", 16 * gb, "mmap"},
		{"no mlock requested", 1 * gb, 0, "mmap", 16 * gb, "mmap"},
		{"unknown size", 1 * gb, 0, "mmap+mlock", 0, "mmap+mlock"},
	}
	for _, c := range cases {
		memAvailable = func() int64 { return c.avail }
		memlockLimit = func() (uint64, bool) { return c.limit, c.limit == ^uint64(0) }
		if got := fitLoadMode(c.mode, c.size); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

func TestFitCacheRAM(t *testing.T) {
	old := memTotal
	defer func() { memTotal = old }()
	for _, c := range []struct {
		total int64
		want  int
	}{
		{0, 8192},        // unknown: llama.cpp's default
		{15 << 30, 1920}, // minion: an eighth of 15 GiB
		{125 << 30, 8192},
	} {
		memTotal = func() int64 { return c.total }
		if got := fitCacheRAM(); got != c.want {
			t.Errorf("total %d: fitCacheRAM = %d, want %d", c.total, got, c.want)
		}
	}
}

// "mlock" alone is try_mmap=false with keep_in_memory=true. Dropping the lock
// falls back to mmap rather than "none" on purpose: the lock was dropped
// because memory is tight, and "none" would read the whole model into
// anonymous memory on that same machine. This pins the reasoning so the
// fallback is not "corrected" into the worse option.
func TestDroppingMlockFallsBackToMmapNotNone(t *testing.T) {
	oldAvail, oldLimit := memAvailable, memlockLimit
	t.Cleanup(func() { memAvailable, memlockLimit = oldAvail, oldLimit })
	memlockLimit = func() (uint64, bool) { return 1 << 20, false } // 1 MiB limit
	memAvailable = func() int64 { return 64 << 30 }
	const big = 16 << 30
	if got := fitLoadMode("mlock", big); got != "mmap" {
		t.Errorf("fitLoadMode(\"mlock\") = %q, want \"mmap\" (paging beats an anonymous copy)", got)
	}
	if got := fitLoadMode("mmap+mlock", big); got != "mmap" {
		t.Errorf("fitLoadMode(\"mmap+mlock\") = %q, want \"mmap\"", got)
	}
	memlockLimit = func() (uint64, bool) { return 0, true }
	if got := fitLoadMode("mlock", 1<<20); got != "mlock" {
		t.Errorf("a lockable model lost its lock: %q", got)
	}
}
