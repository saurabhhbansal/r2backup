package e2e

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/saurabhhbansal/r2backup/internal/remote"
	"github.com/saurabhhbansal/r2backup/test/fixtures"
)

// injectObject writes a live object directly through the client, bypassing
// backup.Run entirely, with the same metadata shape backup itself writes
// (see internal/backup/adapters.go's metadataFor). Both tests in this file
// need to reproduce a bucket exactly as a backup made on a *different*
// operating system would have left it -- macOS/Linux both allow "CON.txt"
// and "file." on disk, so a real backup.Run on this suite's own platform
// cannot be trusted to have ever created those object keys if this suite
// happens to run on Windows. Writing the object directly makes the
// scenario -- and therefore the restore-side assertions below -- the same
// on every platform CI runs this on, rather than only on whichever platform
// happened to be available when the test was written.
func injectObject(t *testing.T, h *harness, relPath string, content []byte) {
	t.Helper()
	meta := remote.Metadata{
		ModTime: time.Now(),
		Mode:    0o644,
		Size:    int64(len(content)),
		Kind:    remote.KindFile,
	}
	err := h.client.Put(context.Background(), remote.PutInput{
		Key:      h.currentPrefix() + relPath,
		Body:     bytes.NewReader(content),
		Size:     int64(len(content)),
		Metadata: meta,
	})
	if err != nil {
		t.Fatalf("inject %s: %v", relPath, err)
	}
}

// TestWindowsReservedNamesRestoreAsFailuresNotCrashes covers the case a
// predecessor tool got wrong: CON, PRN, AUX, NUL and COM1/LPT1 cannot be
// created on Windows under any name-parsing rule, full stop. A backup made
// on Linux or macOS can absolutely contain them (nothing on those platforms
// stops it), so a Windows machine restoring someone else's backup -- or its
// own, restored after reinstalling Windows onto what was a dual-boot box --
// must not let those few objects crash or halt the run. They are reported
// as failures, and every other object still restores.
func TestWindowsReservedNamesRestoreAsFailuresNotCrashes(t *testing.T) {
	h := newHarness(t, "Reserved")
	manifest, err := fixtures.Build(h.root, fixtures.Spec{
		SmallFiles:    5,
		SmallFileSize: 64,
		Seed:          21,
	})
	if err != nil {
		t.Fatal(err)
	}
	rep := h.backupRun(t)
	if !rep.Succeeded() {
		t.Fatalf("backup of the ordinary tree reported failures: %+v", rep.Failures)
	}

	reservedContent := map[string][]byte{}
	for _, name := range fixtures.WindowsReservedNames {
		rel := "reserved/" + name + ".txt"
		content := []byte("this is " + name)
		reservedContent[rel] = content
		injectObject(t, h, rel, content)
	}

	target := t.TempDir()
	restoreRep := h.restoreInto(t, target)

	if runtime.GOOS == "windows" {
		if restoreRep.Succeeded() {
			t.Fatal("restore reported success, but Windows cannot create any of the reserved names")
		}
		if len(restoreRep.Failures) != len(reservedContent) {
			t.Fatalf("Failures = %+v, want exactly the %d reserved-name objects", restoreRep.Failures, len(reservedContent))
		}
		for _, f := range restoreRep.Failures {
			if _, wasReserved := reservedContent[f.Key]; !wasReserved {
				t.Errorf("unexpected failure for %q, which is not one of the reserved names", f.Key)
			}
		}
	} else {
		if !restoreRep.Succeeded() {
			t.Fatalf("restore reported failures on a platform with no naming restriction: %+v", restoreRep.Failures)
		}
		for rel, want := range reservedContent {
			got, err := os.ReadFile(filepath.Join(target, filepath.FromSlash(rel)))
			if err != nil {
				t.Errorf("read back %s: %v", rel, err)
				continue
			}
			if !bytes.Equal(got, want) {
				t.Errorf("%s: content did not round-trip", rel)
			}
		}
	}

	// Whichever platform this ran on, the ordinary tree from the first
	// backup must have restored untouched -- a batch of unrestorable
	// objects must never take the rest of the run down with it. Compared
	// file-by-file against the manifest rather than with a whole-tree
	// fixtures.Compare, since h.root deliberately never contains the
	// injected reserved-name objects (see injectObject) and a directory
	// diff would report every one of them as "unexpected".
	assertManifestFilesMatch(t, h.root, target, manifest)
}

// TestTrailingDotsAndSpacesDoNotCorruptRestore covers the other Windows
// naming quirk: CreateFile silently strips trailing dots and spaces from a
// path component before it ever reaches NTFS, rather than refusing the
// name outright. A backup can easily contain "file." or "name " (Linux and
// macOS both keep them verbatim), and restoring one on Windows must not
// corrupt or clobber anything else in the same directory -- at worst, that
// one object lands under a silently different name than the bucket
// recorded, which this test allows for explicitly rather than asserting a
// byte-for-byte match runtime.GOOS can't actually promise.
func TestTrailingDotsAndSpacesDoNotCorruptRestore(t *testing.T) {
	h := newHarness(t, "TrailingChars")
	manifest, err := fixtures.Build(h.root, fixtures.Spec{
		SmallFiles:    5,
		SmallFileSize: 64,
		Seed:          22,
	})
	if err != nil {
		t.Fatal(err)
	}
	rep := h.backupRun(t)
	if !rep.Succeeded() {
		t.Fatalf("backup of the ordinary tree reported failures: %+v", rep.Failures)
	}

	keepContent := []byte("the sibling in the same directory")
	dotContent := []byte("trailing dot")
	spaceContent := []byte("trailing space")
	injectObject(t, h, "trailing/keep.txt", keepContent)
	injectObject(t, h, "trailing/file.", dotContent)
	injectObject(t, h, "trailing/name ", spaceContent)

	target := t.TempDir()
	restoreRep := h.restoreInto(t, target)
	if !restoreRep.Succeeded() {
		t.Fatalf("restore reported failures, but a stripped name should be silently renamed, not fail: %+v", restoreRep.Failures)
	}

	// The rest of the tree, including the sibling sharing the same
	// directory as the two odd names, must be untouched.
	assertManifestFilesMatch(t, h.root, target, manifest)

	got, err := os.ReadFile(filepath.Join(target, "trailing", "keep.txt"))
	if err != nil {
		t.Fatalf("sibling file did not survive restoring its oddly-named neighbours: %v", err)
	}
	if !bytes.Equal(got, keepContent) {
		t.Error("sibling file's content was corrupted by restoring its oddly-named neighbours")
	}

	assertStrippedOrExact(t, target, "trailing/file.", "trailing/file", dotContent)
	assertStrippedOrExact(t, target, "trailing/name ", "trailing/name", spaceContent)
}

// assertManifestFilesMatch checks every file fixtures.Build actually put
// under root exists, byte-identical, under the same relative path in
// target. Unlike fixtures.Compare, it never looks at what else target
// contains, which is exactly the point when target also holds objects that
// were injected straight into the bucket rather than ever having existed
// under root (see injectObject).
func assertManifestFilesMatch(t *testing.T, root, target string, manifest *fixtures.Manifest) {
	t.Helper()
	for _, rel := range manifest.Files {
		want, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatalf("read original %s: %v", rel, err)
		}
		got, err := os.ReadFile(filepath.Join(target, filepath.FromSlash(rel)))
		if err != nil {
			t.Errorf("read restored %s: %v", rel, err)
			continue
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s: content did not round-trip", rel)
		}
	}
}

// assertStrippedOrExact checks that the object landed either under its
// exact recorded name (any platform that preserves trailing dots/spaces) or
// under the name Windows silently strips it down to -- and that whichever
// one exists on disk holds the right bytes. It fails only if neither path
// exists, or if the content is wrong.
func assertStrippedOrExact(t *testing.T, target, exactRel, strippedRel string, want []byte) {
	t.Helper()
	if got, err := os.ReadFile(filepath.Join(target, filepath.FromSlash(exactRel))); err == nil {
		if !bytes.Equal(got, want) {
			t.Errorf("%s: content did not round-trip", exactRel)
		}
		return
	}
	if got, err := os.ReadFile(filepath.Join(target, filepath.FromSlash(strippedRel))); err == nil {
		if !bytes.Equal(got, want) {
			t.Errorf("%s (stripped from %s): content did not round-trip", strippedRel, exactRel)
		}
		return
	}
	t.Errorf("neither %q nor its Windows-stripped form %q exists after restore", exactRel, strippedRel)
}
