package backup_test

import (
	"context"
	"testing"

	"github.com/saurabhhbansal/r2backup/internal/backup"
	"github.com/saurabhhbansal/r2backup/test/fixtures"
)

func (h *harness) runAt(t *testing.T, place string) *backup.Report {
	t.Helper()
	rep, err := backup.Run(context.Background(), backup.Options{
		Set: h.set, Index: h.db, Client: h.client, DetectMoves: true, Place: place,
	})
	if err != nil {
		t.Fatalf("backup.Run: %v", err)
	}
	return rep
}

// After backups start going to other storage, a file that storage lacks is
// sent again, even though it has not changed on disk. Files it already holds
// are not.
func TestChangingStorageSendsWhatTheNewStorageLacks(t *testing.T) {
	h := setup(t, fixtures.Spec{SmallFiles: 20, SmallFileSize: 256, Seed: 9})

	first := h.runAt(t, "old")
	if first.Uploaded == 0 {
		t.Fatal("first run uploaded nothing")
	}

	// The new storage is missing three of the files (its copy was behind).
	live := h.liveKeys(t)
	var missing []string
	for k := range live {
		if len(missing) == 3 {
			break
		}
		missing = append(missing, k)
	}
	if err := h.client.DeleteBatch(context.Background(), missing); err != nil {
		t.Fatal(err)
	}

	// Same storage: the records are trusted, nothing is listed or sent.
	if same := h.runAt(t, "old"); same.Uploaded != 0 || same.Operations != 0 {
		t.Fatalf("run against the same storage did work: %d uploaded, %d operations", same.Uploaded, same.Operations)
	}

	moved := h.runAt(t, "new")
	if moved.Uploaded != len(missing) {
		t.Fatalf("after changing storage %d files were sent, want the %d it lacks", moved.Uploaded, len(missing))
	}
	if got := len(h.liveKeys(t)); got != len(live) {
		t.Fatalf("storage holds %d files, want %d", got, len(live))
	}

	// Checked once: the next run is free again.
	if again := h.runAt(t, "new"); again.Uploaded != 0 || again.Operations != 0 {
		t.Fatalf("second run after the change did work: %d uploaded, %d operations", again.Uploaded, again.Operations)
	}
}

// An index from before places were recorded is taken to match, so updating
// does not list every backup on the first run.
func TestAnIndexWithNoPlaceRecordedIsTrusted(t *testing.T) {
	h := setup(t, fixtures.Spec{SmallFiles: 5, SmallFileSize: 128, Seed: 3})
	h.run(t) // no place, as before this existed
	live := h.liveKeys(t)
	for k := range live {
		if err := h.client.Delete(context.Background(), k); err != nil {
			t.Fatal(err)
		}
		break
	}
	if rep := h.runAt(t, "first"); rep.Uploaded != 0 {
		t.Fatalf("first run with a place re-sent %d files", rep.Uploaded)
	}
	if p, _ := h.db.Place(h.set.Name); p != "first" {
		t.Fatalf("place recorded as %q", p)
	}
}
