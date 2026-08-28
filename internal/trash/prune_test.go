package trash

import (
	"context"
	"testing"
	"time"
)

// seedTrashKey seeds a key laid out exactly the way buildTrashKey would
// produce, for a given trashed-on date, so Prune's date-filtering logic
// can be tested without going through Move first.
func seedTrashKey(fb *fakeBackend, prefix, date, relPath string) string {
	key := buildTrashKey(prefix, relPath, mustParseDate(date))
	fb.seed(key, 1)
	return key
}

func mustParseDate(s string) time.Time {
	d, err := time.Parse(dateLayout, s)
	if err != nil {
		panic(err)
	}
	return d
}

func TestPruneDeletesOnlyDatesOlderThanCutoff(t *testing.T) {
	fb := newFakeBackend()
	// "today" is 2026-08-28, retention 30 days: cutoff = 2026-07-29.
	// 2026-07-29 is exactly 30 days old -- still within retention, kept.
	// 2026-07-28 is 31 days old -- one day past retention, pruned.
	kept := seedTrashKey(fb, "myset", "2026-07-29", "kept-at-boundary.txt")
	pruned := seedTrashKey(fb, "myset", "2026-07-28", "pruned-one-day-past.txt")
	alsoKept := seedTrashKey(fb, "myset", "2026-08-28", "kept-today.txt")

	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	tr := New(fb, fixedClock(now))

	res, err := tr.Prune(context.Background(), "myset", 30)
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}

	if len(res.DatesPruned) != 1 || res.DatesPruned[0] != "2026-07-28" {
		t.Errorf("DatesPruned = %v, want [2026-07-28]", res.DatesPruned)
	}
	if res.KeysDeleted != 1 {
		t.Errorf("KeysDeleted = %d, want 1", res.KeysDeleted)
	}
	if fb.has(pruned) {
		t.Errorf("key trashed on 2026-07-28 (31 days old) survived Prune")
	}
	if !fb.has(kept) {
		t.Errorf("key trashed exactly 30 days ago (2026-07-29) was pruned; it is still within retention")
	}
	if !fb.has(alsoKept) {
		t.Errorf("key trashed today was pruned")
	}
}

func TestPruneRetentionZeroIsNoOp(t *testing.T) {
	fb := newFakeBackend()
	seedTrashKey(fb, "myset", "2020-01-01", "ancient.txt")

	tr := New(fb, fixedClock(time.Now()))
	res, err := tr.Prune(context.Background(), "myset", 0)
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if len(res.DatesPruned) != 0 || fb.listCalls != 0 || fb.deleteBatchCalls != 0 {
		t.Errorf("retention 0 should not touch the backend at all: %+v, listCalls=%d, deleteBatchCalls=%d",
			res, fb.listCalls, fb.deleteBatchCalls)
	}
}

func TestPruneNeverTouchesCurrent(t *testing.T) {
	fb := newFakeBackend()
	fb.panicOnCurrentDelete = true

	// A key shaped like a trash entry, but crafted to also contain
	// "/current/" partway through a relative path -- e.g. a folder
	// literally named "current" nested somewhere inside the tree being
	// backed up. Even in that adversarial case Prune's date filter must
	// still keep it: this key is not older than the cutoff, so it should
	// never even reach the delete path, let alone the panic.
	recentButShaped := seedTrashKey(fb, "myset", "2026-08-28", "current/nested/file.txt")

	// A genuinely old date directory, to confirm Prune does real work
	// around the adversarial key without ever calling Delete/DeleteBatch
	// on anything containing "/current/".
	old := seedTrashKey(fb, "myset", "2020-01-01", "old-stuff.txt")

	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	tr := New(fb, fixedClock(now))

	res, err := tr.Prune(context.Background(), "myset", 30)
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if fb.has(old) {
		t.Error("old key was not pruned")
	}
	if !fb.has(recentButShaped) {
		t.Error("recent key was pruned even though it is within retention")
	}
	if len(res.DatesPruned) != 1 || res.DatesPruned[0] != "2020-01-01" {
		t.Errorf("DatesPruned = %v, want [2020-01-01]", res.DatesPruned)
	}
}

func TestPruneClassAOpsMatchesFakeListCalls(t *testing.T) {
	fb := newFakeBackend()
	seedTrashKey(fb, "myset", "2020-01-01", "a.txt")
	seedTrashKey(fb, "myset", "2020-01-01", "b.txt")

	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	tr := New(fb, fixedClock(now))

	res, err := tr.Prune(context.Background(), "myset", 30)
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if fb.listCalls != 1 {
		t.Fatalf("fake recorded %d List calls, want 1", fb.listCalls)
	}
	if res.ClassAOps != fb.listCalls {
		t.Errorf("PruneResult.ClassAOps = %d, actual List calls = %d", res.ClassAOps, fb.listCalls)
	}
}

func TestPruneHonoursContextCancellation(t *testing.T) {
	fb := newFakeBackend()
	seedTrashKey(fb, "myset", "2020-01-01", "a.txt")

	tr := New(fb, fixedClock(time.Now()))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := tr.Prune(ctx, "myset", 30)
	if err == nil {
		t.Fatal("Prune with a canceled context returned nil error")
	}
	if fb.listCalls != 0 || fb.deleteBatchCalls != 0 {
		t.Errorf("Prune should not have touched the backend: listCalls=%d deleteBatchCalls=%d", fb.listCalls, fb.deleteBatchCalls)
	}
}
