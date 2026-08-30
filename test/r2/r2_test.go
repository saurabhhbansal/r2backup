//go:build realr2

package r2

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/saurabhhbansal/r2backup/internal/backup"
	"github.com/saurabhhbansal/r2backup/internal/trash"
	"github.com/saurabhhbansal/r2backup/test/fixtures"
)

// TestRealR2FullCycle is this suite's headline test: generate a tree, back
// it up to a real bucket, mutate it, back it up again, restore it, and
// diff every byte. MinIO already proves this path works against an
// S3-compatible server; this proves it against the one R2 actually is,
// where multipart limits, throttling, and error shapes can legitimately
// differ.
//
// The tree is kept small and the large file modest -- comfortably past
// internal/remote's multipartThreshold, so a real multipart upload against
// real R2 is exercised, and no further -- on purpose: this suite is meant to
// run before a release, not to spend minutes and real egress on every
// invocation.
func TestRealR2FullCycle(t *testing.T) {
	h := newHarness(t, "full-cycle")

	manifest, err := fixtures.Build(h.root, fixtures.Spec{
		SmallFiles:    40,
		SmallFileSize: 2048,
		ZeroByteFiles: 2,
		EmptyDirs:     2,
		AwkwardNames:  true,
		LargeFileSize: 80 << 20, // five parts against real R2, not just MinIO
		Seed:          1,
	})
	if err != nil {
		t.Fatalf("build fixture: %v", err)
	}

	first := h.backupRun(t)
	if !first.Succeeded() {
		t.Fatalf("first backup reported failures: %+v", first.Failures)
	}
	if first.Uploaded != len(manifest.Files)+len(manifest.Dirs) {
		t.Logf("Uploaded = %d, fixture recorded %d files + %d dirs (collisions/skips can explain a gap): %v",
			first.Uploaded, len(manifest.Files), len(manifest.Dirs), manifest.Skipped)
	}

	edited, deleted, added, err := fixtures.Mutate(h.root, 5, 3, 4, 2)
	if err != nil {
		t.Fatalf("mutate fixture: %v", err)
	}
	future := time.Now().Add(time.Hour)
	for _, rel := range edited {
		p := filepath.Join(h.root, filepath.FromSlash(rel))
		if err := os.Chtimes(p, future, future); err != nil {
			t.Fatalf("touch %s: %v", rel, err)
		}
	}

	second := h.backupRun(t)
	if !second.Succeeded() {
		t.Fatalf("second backup reported failures: %+v", second.Failures)
	}
	if second.Deleted != len(deleted) {
		t.Errorf("Deleted = %d, want %d", second.Deleted, len(deleted))
	}
	wantMin := len(edited) + len(added)
	if second.Uploaded < wantMin {
		t.Errorf("Uploaded = %d, want at least %d (%d edited + %d added)", second.Uploaded, wantMin, len(edited), len(added))
	}

	target := t.TempDir()
	restoreRep := h.restoreInto(t, target)
	if !restoreRep.Succeeded() {
		t.Fatalf("restore reported failures: %+v", restoreRep.Failures)
	}

	diffs, err := fixtures.Compare(h.root, target, 5*time.Second)
	if err != nil {
		t.Fatalf("fixtures.Compare: %v", err)
	}
	if len(diffs) != 0 {
		for _, d := range diffs {
			t.Logf("DIFF: %s", d)
		}
		t.Fatalf("restore did not round-trip byte for byte against real R2: %d differences (see log)", len(diffs))
	}
}

// TestRealR2TrashAndPrune exercises the safety net end to end against a
// real bucket: overwriting a file must leave the old version recoverable
// from trash, and Prune must actually remove it once its retention window
// has passed. The retention window is faked forward via trash.Clock rather
// than by actually waiting sets.DefaultRetentionDays days for a CI run.
func TestRealR2TrashAndPrune(t *testing.T) {
	h := newHarness(t, "trash-prune")

	manifest, err := fixtures.Build(h.root, fixtures.Spec{
		SmallFiles:    10,
		SmallFileSize: 512,
		Seed:          2,
	})
	if err != nil {
		t.Fatalf("build fixture: %v", err)
	}
	if len(manifest.Files) == 0 {
		t.Fatal("fixture built no files to overwrite")
	}

	h.backupRun(t, func(o *backup.Options) {
		o.Trash = backup.NewTrash(h.client, h.set.RetentionDays)
	})

	// Overwrite one file so there is something to find in trash.
	victim := filepath.Join(h.root, filepath.FromSlash(manifest.Files[0]))
	if err := os.WriteFile(victim, []byte("overwritten for the trash test"), 0o644); err != nil {
		t.Fatalf("overwrite victim: %v", err)
	}
	if err := os.Chtimes(victim, time.Now().Add(time.Hour), time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("touch victim: %v", err)
	}

	rep := h.backupRun(t, func(o *backup.Options) {
		o.Trash = backup.NewTrash(h.client, h.set.RetentionDays)
	})
	if !rep.Succeeded() {
		t.Fatalf("overwrite backup reported failures: %+v", rep.Failures)
	}

	tr := trash.New(h.client, trash.Clock{})
	entries, err := tr.List(context.Background(), h.set.Prefix, h.set.RetentionDays)
	if err != nil {
		t.Fatalf("trash.List: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("nothing was trashed by the overwrite")
	}

	// Fast-forward Prune's clock past the retention window and confirm the
	// trashed object is actually gone afterward.
	future := time.Now().AddDate(0, 0, h.set.RetentionDays+1)
	prTr := trash.New(h.client, trash.Clock{Now: func() time.Time { return future }})
	pruneRes, err := prTr.Prune(context.Background(), h.set.Prefix, h.set.RetentionDays)
	if err != nil {
		t.Fatalf("trash.Prune: %v", err)
	}
	if pruneRes.KeysDeleted == 0 {
		t.Fatal("Prune reported deleting nothing, but a trashed object was past its retention window")
	}

	after, err := trash.New(h.client, trash.Clock{}).List(context.Background(), h.set.Prefix, h.set.RetentionDays)
	if err != nil {
		t.Fatalf("trash.List after prune: %v", err)
	}
	if len(after) != 0 {
		t.Fatalf("%d trashed objects survived Prune past their retention window", len(after))
	}
}
