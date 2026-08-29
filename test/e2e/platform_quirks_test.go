package e2e

import (
	"bytes"
	"context"
	"os"
	"path"
	"path/filepath"
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
	if err := tryInjectObject(h, relPath, content); err != nil {
		t.Fatalf("inject %s: %v", relPath, err)
	}
}

// tryInjectObject is injectObject for keys the test server itself may refuse.
// MinIO lays object keys out on its own filesystem, so a key ending in a dot
// or a space is unstorable when MinIO is running on Windows -- it comes back
// as an S3 IncompleteBody, because the path it wrote to is not the path it
// was asked for. That is the emulator's limit and not r2backup's: R2 is a
// real object store and takes the key verbatim, which is what test/r2 is for.
// Callers decide what to do about a refusal instead of dying on it.
func tryInjectObject(h *harness, relPath string, content []byte) error {
	meta := remote.Metadata{
		ModTime: time.Now(),
		Mode:    0o644,
		Size:    int64(len(content)),
		Kind:    remote.KindFile,
	}
	return h.client.Put(context.Background(), remote.PutInput{
		Key:      h.currentPrefix() + relPath,
		Body:     bytes.NewReader(content),
		Size:     int64(len(content)),
		Metadata: meta,
	})
}

// behavesAsAFile reports whether this machine turns writing to name inside
// dir into an ordinary file holding those bytes.
//
// It exists because "can Windows create this name" has no answer that holds
// for every Windows machine. A reserved name is not refused, it is redirected
// to a device: CON.txt reaches the console, NUL.txt swallows what it is
// given, and COM1.txt or LPT1.txt fail only on a machine with no such port.
// The create can therefore succeed while leaving nothing behind, so the only
// question worth asking is what is at the path afterwards. Note it never
// reads the path back -- reading CON on a machine with a console waits for
// somebody to type something, which in CI is forever.
func behavesAsAFile(t *testing.T, dir, name string) bool {
	t.Helper()
	const probe = "probe"
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(probe), 0o644); err != nil {
		return false
	}
	info, err := os.Lstat(p)
	return err == nil && info.Mode().IsRegular() && info.Size() == int64(len(probe))
}

// TestWindowsReservedNamesRestoreAsFailuresNotCrashes covers the case a
// predecessor tool got wrong. A backup made on Linux or macOS can absolutely
// contain CON.txt, NUL.txt or COM1.txt -- nothing on those platforms stops it
// -- so a Windows machine restoring someone else's backup, or its own after
// reinstalling Windows onto what was a dual-boot box, must not let those few
// objects crash or halt the run. Every other object still restores.
//
// What Windows does with such a name is not a constant, which is why this
// probes rather than branching on runtime.GOOS. The names are redirected to
// devices, not refused: writing CON.txt can succeed with the bytes going to
// the console, NUL.txt discards them, and COM1.txt or LPT1.txt fail only
// because that machine has no such port. Which of them come back as failures
// is therefore a property of the machine, and asserting a fixed answer is
// what made this test fail the first time it ever ran on Windows.
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

	// Whatever this machine makes of these names, a failure must never be
	// reported against anything else: an object the platform cannot hold is
	// not allowed to take an ordinary file down with it.
	failed := map[string]bool{}
	for _, f := range restoreRep.Failures {
		if _, wasReserved := reservedContent[f.Key]; !wasReserved {
			t.Errorf("unexpected failure for %q, which is not one of the reserved names", f.Key)
			continue
		}
		failed[f.Key] = true
	}

	probe := t.TempDir()
	for rel, want := range reservedContent {
		if !behavesAsAFile(t, probe, path.Base(rel)) {
			// This machine will not hand anybody a file by that name, so
			// there is nothing on disk to compare it against. That restore
			// neither crashed nor stopped is what matters, and the manifest
			// check below is what proves it.
			continue
		}
		if failed[rel] {
			t.Errorf("%s was reported as a failure, but this machine writes that name as an ordinary file", rel)
			continue
		}
		got, err := os.ReadFile(filepath.Join(target, filepath.FromSlash(rel)))
		if err != nil {
			t.Errorf("read back %s: %v", rel, err)
			continue
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s: content did not round-trip", rel)
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
	injectObject(t, h, "trailing/keep.txt", keepContent)

	// The odd names go in one at a time, because the test server may not be
	// able to hold one: MinIO stores an object key as a path on its own
	// filesystem, so running on Windows it cannot store a key whose last
	// component ends in a dot or a space -- the very names this test is
	// about. Whichever it does accept still exercises the restore side,
	// which is what is under test here.
	odd := []struct {
		rel      string
		stripped string
		content  []byte
	}{
		{"trailing/file.", "trailing/file", []byte("trailing dot")},
		{"trailing/name ", "trailing/name", []byte("trailing space")},
	}
	var stored []int
	for i, o := range odd {
		if err := tryInjectObject(h, o.rel, o.content); err != nil {
			t.Logf("the test server would not store %q, so restoring it cannot be covered here: %v", o.rel, err)
			continue
		}
		stored = append(stored, i)
	}
	if len(stored) == 0 {
		t.Skip("the test server could not store either trailing-dot or trailing-space key, so there is no such object for restore to be tested against")
	}

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

	for _, i := range stored {
		assertStrippedOrExact(t, target, odd[i].rel, odd[i].stripped, odd[i].content)
	}
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
