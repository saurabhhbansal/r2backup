package trash

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/saurabhhbansal/r2backup/internal/remote"
)

func TestRestorePutsObjectBackAtLiveKey(t *testing.T) {
	fb := newFakeBackend()
	trashKey := buildTrashKey("myset", "docs/report.pdf", mustParseDate("2026-08-01"))
	fb.seed(trashKey, 1024)

	tr := New(fb, fixedClock(time.Now()))
	entry := Entry{RelPath: "docs/report.pdf", TrashKey: trashKey}

	if err := tr.Restore(context.Background(), "myset", entry); err != nil {
		t.Fatalf("Restore: %v", err)
	}

	wantLive := "myset/current/docs/report.pdf"
	if !fb.has(wantLive) {
		t.Errorf("live key %q not present after Restore", wantLive)
	}
	// The trashed copy is left in place -- Restore is not a move, and a
	// person recovering a file may want to keep the trashed copy around
	// too (e.g. to compare against what they just restored).
	if !fb.has(trashKey) {
		t.Error("Restore removed the trashed copy; it should leave it in place")
	}
	if fb.copyCalls != 1 {
		t.Errorf("copyCalls = %d, want 1 (Restore must use Copy, not Get/Put)", fb.copyCalls)
	}
}

func TestRestoreMissingTrashKeyReturnsNotFound(t *testing.T) {
	fb := newFakeBackend()
	tr := New(fb, fixedClock(time.Now()))

	entry := Entry{RelPath: "docs/report.pdf", TrashKey: "myset/trash/2026-08-01/docs/report~090000-abc123.pdf"}
	err := tr.Restore(context.Background(), "myset", entry)
	if err == nil {
		t.Fatal("Restore of a missing trash key returned nil error")
	}
	if !errors.Is(err, remote.ErrNotFound) {
		t.Errorf("Restore error = %v, want it to wrap remote.ErrNotFound", err)
	}
	if fb.copyCalls != 0 {
		t.Errorf("copyCalls = %d, want 0 -- Restore should not attempt the copy after a failed Head", fb.copyCalls)
	}
}

func TestRestoreHonoursContextCancellation(t *testing.T) {
	fb := newFakeBackend()
	trashKey := buildTrashKey("myset", "a.txt", mustParseDate("2026-08-01"))
	fb.seed(trashKey, 10)

	tr := New(fb, fixedClock(time.Now()))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := tr.Restore(ctx, "myset", Entry{RelPath: "a.txt", TrashKey: trashKey})
	if err == nil {
		t.Fatal("Restore with a canceled context returned nil error")
	}
	if fb.copyCalls != 0 {
		t.Errorf("copyCalls = %d, want 0", fb.copyCalls)
	}
}
