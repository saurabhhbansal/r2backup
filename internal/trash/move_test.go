package trash

import (
	"context"
	"strings"
	"testing"
	"time"
)

func fixedClock(t time.Time) Clock {
	return Clock{Now: func() time.Time { return t }}
}

func TestMoveCopiesToDatedKeyAndKeepsSource(t *testing.T) {
	fb := newFakeBackend()
	fb.seed("myset/current/docs/report.pdf", 1024)

	now := time.Date(2026, 8, 28, 14, 30, 22, 0, time.UTC)
	tr := New(fb, fixedClock(now))

	res, err := tr.Move(context.Background(), "myset", []string{"docs/report.pdf"}, 30)
	if err != nil {
		t.Fatalf("Move: %v", err)
	}
	if len(res.Moved) != 1 {
		t.Fatalf("Moved = %v, want 1 entry", res.Moved)
	}
	entry := res.Moved[0]
	if entry.RelPath != "docs/report.pdf" {
		t.Errorf("RelPath = %q, want %q", entry.RelPath, "docs/report.pdf")
	}
	wantPrefix := "myset/trash/2026-08-28/docs/report~"
	if !strings.HasPrefix(entry.TrashKey, wantPrefix) {
		t.Errorf("TrashKey = %q, want prefix %q", entry.TrashKey, wantPrefix)
	}
	if !strings.HasSuffix(entry.TrashKey, ".pdf") {
		t.Errorf("TrashKey = %q, want suffix %q", entry.TrashKey, ".pdf")
	}

	// The source must still be there: Move copies, the caller deletes.
	if !fb.has("myset/current/docs/report.pdf") {
		t.Error("Move deleted or otherwise removed the source; it must leave that to the caller")
	}
	if !fb.has(entry.TrashKey) {
		t.Error("trashed key not present in backend")
	}
}

func TestMoveUsesCopyNeverGetOrPut(t *testing.T) {
	// Backend does not even declare Get or Put, so there is no call path
	// for Move to reach them -- this is enforced by the compiler, not
	// just convention. What's worth asserting is that Move issues exactly
	// one Copy per key, with nothing else in between.
	fb := newFakeBackend()
	fb.seed("myset/current/a.txt", 10)
	fb.seed("myset/current/b.txt", 20)

	tr := New(fb, fixedClock(time.Date(2026, 8, 28, 9, 0, 0, 0, time.UTC)))
	res, err := tr.Move(context.Background(), "myset", []string{"a.txt", "b.txt"}, 30)
	if err != nil {
		t.Fatalf("Move: %v", err)
	}
	if fb.copyCalls != 2 {
		t.Errorf("copyCalls = %d, want 2", fb.copyCalls)
	}
	if fb.deleteCalls != 0 || fb.deleteBatchCalls != 0 {
		t.Errorf("Move must never delete: deleteCalls=%d deleteBatchCalls=%d", fb.deleteCalls, fb.deleteBatchCalls)
	}
	if res.ClassAOps != 2 {
		t.Errorf("ClassAOps = %d, want 2", res.ClassAOps)
	}
}

func TestMoveSamePathTwiceSameDayBothRecoverable(t *testing.T) {
	fb := newFakeBackend()
	fb.seed("myset/current/notes.md", 100)

	// A frozen clock: this is the adversarial case (09:00 and 15:00
	// collapsed into the exact same instant), which is exactly what a
	// timestamp-only disambiguator would fail on.
	now := time.Date(2026, 8, 28, 9, 0, 0, 0, time.UTC)
	tr := New(fb, fixedClock(now))
	ctx := context.Background()

	res1, err := tr.Move(ctx, "myset", []string{"notes.md"}, 30)
	if err != nil {
		t.Fatalf("first Move: %v", err)
	}
	key1 := res1.Moved[0].TrashKey

	// Simulate the caller's sync overwriting the live copy with a new
	// version before the second trashing.
	fb.seed("myset/current/notes.md", 200)

	res2, err := tr.Move(ctx, "myset", []string{"notes.md"}, 30)
	if err != nil {
		t.Fatalf("second Move: %v", err)
	}
	key2 := res2.Moved[0].TrashKey

	if key1 == key2 {
		t.Fatalf("both trashings produced the same key %q; the second silently overwrote the first", key1)
	}
	if !fb.has(key1) {
		t.Errorf("first trashed object %q was lost", key1)
	}
	if !fb.has(key2) {
		t.Errorf("second trashed object %q was lost", key2)
	}

	for _, key := range []string{key1, key2} {
		_, relPath, ok := parseTrashKey("myset", key)
		if !ok {
			t.Fatalf("parseTrashKey(%q) failed to parse a key this package produced", key)
		}
		if relPath != "notes.md" {
			t.Errorf("parseTrashKey(%q) relPath = %q, want %q (extension must stay recoverable)", key, relPath, "notes.md")
		}
	}
}

func TestMoveRetentionZeroIsNoOp(t *testing.T) {
	fb := newFakeBackend()
	fb.seed("myset/current/a.txt", 10)

	tr := New(fb, fixedClock(time.Now()))
	res, err := tr.Move(context.Background(), "myset", []string{"a.txt"}, 0)
	if err != nil {
		t.Fatalf("Move: %v", err)
	}
	if len(res.Moved) != 0 {
		t.Errorf("Moved = %v, want none", res.Moved)
	}
	if res.ClassAOps != 0 {
		t.Errorf("ClassAOps = %d, want 0", res.ClassAOps)
	}
	if fb.copyCalls != 0 {
		t.Errorf("copyCalls = %d, want 0 -- retention 0 must spend nothing", fb.copyCalls)
	}
}

func TestMoveHonoursContextCancellation(t *testing.T) {
	fb := newFakeBackend()
	fb.seed("myset/current/a.txt", 10)

	tr := New(fb, fixedClock(time.Now()))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := tr.Move(ctx, "myset", []string{"a.txt"}, 30)
	if err == nil {
		t.Fatal("Move with a canceled context returned nil error")
	}
	if fb.copyCalls != 0 {
		t.Errorf("copyCalls = %d, want 0 -- Move should not have issued any work", fb.copyCalls)
	}
}

func TestEstimateMoveOpsMatchesActualCopies(t *testing.T) {
	fb := newFakeBackend()
	relPaths := []string{"a.txt", "b.txt", "c.txt"}
	for _, p := range relPaths {
		fb.seed("myset/current/"+p, 5)
	}

	tr := New(fb, fixedClock(time.Now()))
	estimate := EstimateMoveOps(relPaths, 30)

	res, err := tr.Move(context.Background(), "myset", relPaths, 30)
	if err != nil {
		t.Fatalf("Move: %v", err)
	}
	if estimate != fb.copyCalls {
		t.Errorf("EstimateMoveOps = %d, actual Copy calls = %d", estimate, fb.copyCalls)
	}
	if estimate != res.ClassAOps {
		t.Errorf("EstimateMoveOps = %d, MoveResult.ClassAOps = %d", estimate, res.ClassAOps)
	}

	if got := EstimateMoveOps(relPaths, 0); got != 0 {
		t.Errorf("EstimateMoveOps with retention 0 = %d, want 0", got)
	}
}
