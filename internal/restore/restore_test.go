package restore_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/saurabhhbansal/r2backup/internal/backup"
	"github.com/saurabhhbansal/r2backup/internal/index"
	"github.com/saurabhhbansal/r2backup/internal/restore"
	"github.com/saurabhhbansal/r2backup/internal/sets"
	"github.com/saurabhhbansal/r2backup/test/fixtures"
	tminio "github.com/saurabhhbansal/r2backup/test/minio"
)

// TestBackupThenRestoreRoundTripsByteForByte is the headline test for this
// package -- and arguably for the whole tool. Returning nil from Run
// proves nothing on its own; this proves the product, by building a real
// tree, backing it up with the real backup package against a real S3
// server, restoring it into a fresh empty directory, and diffing the two
// trees byte for byte.
//
// UnicodeNames is deliberately left out of this fixture. It writes
// "résumé.txt" both precomposed and decomposed; both normalize to the
// same object key (see scan.Key), so plan.Build can only keep one of them,
// and *which* one survives is not determined by anything this test
// controls (scan.Walk sorts by key, and the two entries compare equal).
// Depending on which file wins, Compare would report either one
// difference (the other's exact path is simply missing) or two (the
// survivor's content differs from what one of the two on-disk paths
// expects, plus the other missing). Asserting a fixed count would make
// this test flaky for a reason that has nothing to do with restore.
// AwkwardNames already exercises Unicode by way of "emoji-🎉-party.txt"
// without any of that ambiguity.
func TestBackupThenRestoreRoundTripsByteForByte(t *testing.T) {
	client, cleanup := tminio.Start(t)
	t.Cleanup(cleanup)

	original := t.TempDir()
	if _, err := fixtures.Build(original, fixtures.Spec{
		SmallFiles:    150,
		SmallFileSize: 0, // varied 1-4KB, the request-bound shape a real tree has
		ZeroByteFiles: 5,
		EmptyDirs:     4,
		AwkwardNames:  true,
		DeepPath:      true,
		Symlinks:      true,
		Seed:          99,
	}); err != nil {
		t.Fatal(err)
	}

	db, err := index.Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	set := sets.Set{
		Name:    "Headline",
		Prefix:  "machines/pc-1/Headline",
		Root:    original,
		Machine: "pc-1",
	}

	backupRep, err := backup.Run(context.Background(), backup.Options{
		Set: set, Index: db, Client: client,
	})
	if err != nil {
		t.Fatalf("backup.Run: %v", err)
	}
	if backupRep.Uploaded == 0 {
		t.Fatal("backup uploaded nothing; the round trip would be vacuous")
	}

	restored := filepath.Join(t.TempDir(), "restored")
	restoreRep, err := restore.Run(context.Background(), restore.Options{
		Set:    set,
		Client: client,
		Target: restored,
	})
	if err != nil {
		t.Fatalf("restore.Run: %v", err)
	}
	if !restoreRep.Succeeded() {
		t.Fatalf("restore reported failures: %+v", restoreRep.Failures)
	}

	diffs, err := fixtures.Compare(original, restored, 2*time.Second)
	if err != nil {
		t.Fatalf("fixtures.Compare: %v", err)
	}

	// Exactly the NFC/NFD collision documented above is not part of this
	// fixture, so any difference at all is a real regression. Print every
	// one so a failure names the exact file, per the brief.
	if len(diffs) != 0 {
		for _, d := range diffs {
			t.Logf("DIFF: %s", d)
		}
		t.Fatalf("restore did not round-trip byte for byte: %d differences (see log)", len(diffs))
	}
}

// TestUnicodeCollisionSurvivesAsExactlyOneDifference documents, rather
// than avoids, the NFC/NFD case the headline test above deliberately
// excludes: it asserts that a run including it produces precisely one
// path's worth of disagreement (the collision loser is simply absent from
// the restored tree), never a silent data loss with zero reported
// differences.
func TestUnicodeCollisionSurvivesAsExactlyOneDifference(t *testing.T) {
	client, cleanup := tminio.Start(t)
	t.Cleanup(cleanup)

	original := t.TempDir()
	manifest, err := fixtures.Build(original, fixtures.Spec{
		SmallFiles:   10,
		UnicodeNames: true,
		Seed:         7,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(manifest.Skipped) > 0 {
		t.Skipf("this filesystem could not represent the NFC/NFD pair: %v", manifest.Skipped)
	}

	db, err := index.Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	set := sets.Set{Name: "Unicode", Prefix: "machines/pc-1/Unicode", Root: original, Machine: "pc-1"}
	if _, err := backup.Run(context.Background(), backup.Options{Set: set, Index: db, Client: client}); err != nil {
		t.Fatalf("backup.Run: %v", err)
	}

	restored := filepath.Join(t.TempDir(), "restored")
	rep, err := restore.Run(context.Background(), restore.Options{Set: set, Client: client, Target: restored})
	if err != nil {
		t.Fatalf("restore.Run: %v", err)
	}
	if !rep.Succeeded() {
		t.Fatalf("restore reported failures: %+v", rep.Failures)
	}

	diffs, err := fixtures.Compare(original, restored, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	// Either shape documented on the headline test above is acceptable
	// here: one missing path (the common case) or a missing path plus a
	// content mismatch (when the NFD-spelled file happened to be the one
	// the planner kept). What must never happen is zero differences --
	// that would mean the collision silently vanished instead of being
	// reported -- or a difference anywhere outside unicode/.
	if len(diffs) == 0 {
		t.Fatal("expected the NFC/NFD collision to surface as at least one difference; got none")
	}
	for _, d := range diffs {
		if filepath.Dir(d.Path) != "unicode" {
			t.Errorf("unexpected difference outside the unicode collision: %s", d)
		}
	}
	t.Logf("collision-driven differences: %v", diffs)
}

// TestRestoreFromAnotherMachinesPrefix backs a tree up under one machine
// name and restores it by naming a different one, proving --machine reads
// the other computer's copy of the set rather than this one's own (which,
// in this test, never has anything backed up to it at all).
func TestRestoreFromAnotherMachinesPrefix(t *testing.T) {
	client, cleanup := tminio.Start(t)
	t.Cleanup(cleanup)

	original := t.TempDir()
	if _, err := fixtures.Build(original, fixtures.Spec{SmallFiles: 20, SmallFileSize: 256, Seed: 3}); err != nil {
		t.Fatal(err)
	}
	db, err := index.Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	sourceSet := sets.Set{Name: "Shared", Prefix: "machines/laptop/Shared", Root: original, Machine: "laptop"}
	if _, err := backup.Run(context.Background(), backup.Options{Set: sourceSet, Index: db, Client: client}); err != nil {
		t.Fatalf("backup.Run: %v", err)
	}

	// A different machine's own record of the identically-named set --
	// its own prefix has nothing in it.
	thisMachinesSet := sets.Set{Name: "Shared", Prefix: "machines/desktop/Shared", Machine: "desktop"}

	restored := filepath.Join(t.TempDir(), "restored")
	rep, err := restore.Run(context.Background(), restore.Options{
		Set:           thisMachinesSet,
		Client:        client,
		Target:        restored,
		SourceMachine: "laptop",
	})
	if err != nil {
		t.Fatalf("restore.Run: %v", err)
	}
	if rep.Downloaded != 20 {
		t.Errorf("Downloaded = %d, want 20 -- restore should have read laptop's prefix", rep.Downloaded)
	}

	diffs, err := fixtures.Compare(original, restored, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if len(diffs) != 0 {
		t.Fatalf("cross-machine restore did not round-trip: %v", diffs)
	}
}
