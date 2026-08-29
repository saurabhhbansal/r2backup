package e2e

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/saurabhhbansal/r2backup/internal/backup"
	"github.com/saurabhhbansal/r2backup/test/fixtures"
)

// assertCleanRoundTrip runs Compare and fails loudly, naming every
// difference, rather than just a count -- the same reporting shape
// internal/restore/restore_test.go's headline test uses, and for the same
// reason: "0 differences" only means something if a failure would have
// named them.
func assertCleanRoundTrip(t *testing.T, original, restored string) {
	t.Helper()
	diffs, err := fixtures.Compare(original, restored, 2*time.Second)
	if err != nil {
		t.Fatalf("fixtures.Compare: %v", err)
	}
	if len(diffs) != 0 {
		for _, d := range diffs {
			t.Logf("DIFF: %s", d)
		}
		t.Fatalf("restore did not round-trip byte for byte: %d differences (see log)", len(diffs))
	}
}

// TestPathOver260Characters proves a file nested past Windows' MAX_PATH
// backs up and restores intact. Go 1.20+ handles a lot of the extended-path
// plumbing on Windows automatically, but not every call in the tree agrees,
// and this is the one test that would notice a regression.
func TestPathOver260Characters(t *testing.T) {
	h := newHarness(t, "DeepPath")
	manifest, err := fixtures.Build(h.root, fixtures.Spec{
		SmallFiles:    5,
		SmallFileSize: 128,
		DeepPath:      true,
		Seed:          1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(manifest.Skipped) > 0 {
		t.Skipf("this filesystem could not represent the deep path: %v", manifest.Skipped)
	}

	rep := h.backupRun(t)
	if !rep.Succeeded() {
		t.Fatalf("backup reported failures: %+v", rep.Failures)
	}

	target := t.TempDir()
	restoreRep := h.restoreInto(t, target)
	if !restoreRep.Succeeded() {
		t.Fatalf("restore reported failures: %+v", restoreRep.Failures)
	}
	assertCleanRoundTrip(t, h.root, target)
}

// TestDeepNesting30Levels is distinct from the over-260-characters case: it
// keeps every path comfortably short but recurses through more than thirty
// directories, exercising the walk and the restore-side directory creation
// under recursion depth rather than raw path length.
func TestDeepNesting30Levels(t *testing.T) {
	h := newHarness(t, "Nesting")

	const depth = 35
	rel := ""
	for i := 0; i < depth; i++ {
		rel += "d/"
	}
	rel += "leaf.txt"
	if err := writeRelFile(h.root, rel, []byte("deep enough")); err != nil {
		t.Fatal(err)
	}
	// A sibling near the top, so a walk that mishandles depth doesn't just
	// happen to still find the one file it was looking for.
	if err := writeRelFile(h.root, "d/shallow.txt", []byte("shallow")); err != nil {
		t.Fatal(err)
	}

	rep := h.backupRun(t)
	if !rep.Succeeded() {
		t.Fatalf("backup reported failures: %+v", rep.Failures)
	}
	if rep.Uploaded != 2 {
		t.Fatalf("Uploaded = %d, want 2", rep.Uploaded)
	}

	target := t.TempDir()
	restoreRep := h.restoreInto(t, target)
	if !restoreRep.Succeeded() {
		t.Fatalf("restore reported failures: %+v", restoreRep.Failures)
	}
	assertCleanRoundTrip(t, h.root, target)
}

// TestCaseOnlyRename proves the tool asserts against what the filesystem
// actually did with Foo.txt and foo.txt, not against runtime.GOOS: on a
// case-insensitive filesystem fixtures.Build already collapses the pair to
// one file (recorded in Manifest.Skipped) before backup ever runs, and on a
// case-sensitive one both survive as two distinct objects. Either way the
// round trip must reproduce exactly what was really on disk.
func TestCaseOnlyRename(t *testing.T) {
	h := newHarness(t, "CaseOnly")
	if _, err := fixtures.Build(h.root, fixtures.Spec{
		SmallFiles:    5,
		SmallFileSize: 64,
		CaseOnlyPair:  true,
		Seed:          2,
	}); err != nil {
		t.Fatal(err)
	}

	rep := h.backupRun(t)
	if !rep.Succeeded() {
		t.Fatalf("backup reported failures: %+v", rep.Failures)
	}

	target := t.TempDir()
	restoreRep := h.restoreInto(t, target)
	if !restoreRep.Succeeded() {
		t.Fatalf("restore reported failures: %+v", restoreRep.Failures)
	}
	assertCleanRoundTrip(t, h.root, target)
}

// TestZeroByteAndEmptyDirRoundTrip asserts, as part of one full cycle rather
// than a narrower unit test, that a file with nothing in it and a directory
// with nothing in it both survive. Object storage has no directories of its
// own -- an empty one only comes back because backup writes it a marker
// object -- so this is worth pinning at the full-product level even though
// internal/restore's kind_test.go already covers the mechanism directly.
func TestZeroByteAndEmptyDirRoundTrip(t *testing.T) {
	h := newHarness(t, "ZeroAndEmpty")
	if _, err := fixtures.Build(h.root, fixtures.Spec{
		SmallFiles:    3,
		SmallFileSize: 32,
		ZeroByteFiles: 4,
		EmptyDirs:     3,
		Seed:          3,
	}); err != nil {
		t.Fatal(err)
	}

	rep := h.backupRun(t)
	if !rep.Succeeded() {
		t.Fatalf("backup reported failures: %+v", rep.Failures)
	}

	target := t.TempDir()
	restoreRep := h.restoreInto(t, target)
	if !restoreRep.Succeeded() {
		t.Fatalf("restore reported failures: %+v", restoreRep.Failures)
	}
	assertCleanRoundTrip(t, h.root, target)
}

// TestLarge200MBFile proves a real multipart upload and download round-trips
// byte-identical. fixtures.Build streams the fixture (io.CopyN from a PRNG)
// and internal/remote streams both directions, so at no point does this test
// -- or the product -- hold 200MB in memory at once.
//
// Guarded behind -short so the fast suite stays fast; it still runs by
// default (CI does not pass -short).
func TestLarge200MBFile(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping 200MB multipart round trip in -short mode")
	}
	h := newHarness(t, "Large")
	const size = 200 << 20
	if _, err := fixtures.Build(h.root, fixtures.Spec{
		LargeFileSize: size,
		Seed:          4,
	}); err != nil {
		t.Fatal(err)
	}

	rep := h.backupRun(t)
	if !rep.Succeeded() {
		t.Fatalf("backup reported failures: %+v", rep.Failures)
	}
	if rep.Bytes != size {
		t.Fatalf("Bytes = %d, want %d", rep.Bytes, size)
	}

	target := t.TempDir()
	restoreRep := h.restoreInto(t, target)
	if !restoreRep.Succeeded() {
		t.Fatalf("restore reported failures: %+v", restoreRep.Failures)
	}
	assertCleanRoundTrip(t, h.root, target)
}

// TestTwentyThousandFileTree proves correctness and throughput at the scale
// a real request-bound tree (node_modules, a photo library) actually is.
// Files are kept tiny on purpose so the whole cycle -- backup, mutate,
// backup again, restore, compare -- stays inside a couple of minutes.
//
// Guarded behind -short like the 200MB case; runs by default in CI.
func TestTwentyThousandFileTree(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping 20,000-file tree in -short mode")
	}
	h := newHarness(t, "Scale")
	const n = 20000
	if _, err := fixtures.Build(h.root, fixtures.Spec{
		SmallFiles:    n,
		SmallFileSize: 48,
		Seed:          5,
	}); err != nil {
		t.Fatal(err)
	}

	first := h.backupRun(t)
	if !first.Succeeded() {
		t.Fatalf("first backup reported failures: %d\n%s", len(first.Failures), whyFailed(first))
	}
	if first.Uploaded != n {
		t.Fatalf("Uploaded = %d, want %d", first.Uploaded, n)
	}

	edited, deleted, added, err := fixtures.Mutate(h.root, 50, 30, 20, 5)
	if err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(time.Hour)
	for _, rel := range edited {
		if err := touchRel(h.root, rel, future); err != nil {
			t.Fatal(err)
		}
	}

	second := h.backupRun(t)
	if !second.Succeeded() {
		t.Fatalf("second backup reported failures: %d\n%s", len(second.Failures), whyFailed(second))
	}
	wantMin := len(edited) + len(added)
	if second.Uploaded < wantMin {
		t.Fatalf("Uploaded = %d, want at least %d (%d edited + %d added)",
			second.Uploaded, wantMin, len(edited), len(added))
	}
	if second.Deleted != len(deleted) {
		t.Fatalf("Deleted = %d, want %d", second.Deleted, len(deleted))
	}

	target := t.TempDir()
	restoreRep := h.restoreInto(t, target)
	if !restoreRep.Succeeded() {
		t.Fatalf("restore reported failures: %d", len(restoreRep.Failures))
	}

	diffs, err := fixtures.Compare(h.root, target, 2*time.Second)
	if err != nil {
		t.Fatalf("fixtures.Compare: %v", err)
	}
	if len(diffs) != 0 {
		for i, d := range diffs {
			if i >= 20 {
				t.Logf("... and %d more differences", len(diffs)-20)
				break
			}
			t.Logf("DIFF: %s", d)
		}
		t.Fatalf("restore did not round-trip byte for byte: %d differences of %d files (see log)", len(diffs), n)
	}
}

// whyFailed names what actually went wrong.
//
// This test asserts zero failures across 20,000 uploads, and when it broke on
// a Windows runner it said "failures: 62" and nothing else -- which is not
// enough to tell a genuine defect from the local MinIO refusing connections
// under load on an overloaded machine. A count is not a diagnosis.
func whyFailed(r *backup.Report) string {
	var b strings.Builder
	seen := map[string]int{}
	for _, f := range r.Failures {
		seen[f.Err.Error()]++
	}
	b.WriteString("distinct errors:\n")
	for msg, n := range seen {
		fmt.Fprintf(&b, "  %4d x %s\n", n, msg)
	}
	for i, f := range r.Failures {
		if i == 3 {
			break
		}
		fmt.Fprintf(&b, "  e.g. %s: %v\n", f.Key, f.Err)
	}
	return b.String()
}
