package e2e

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/saurabhhbansal/r2backup/internal/backup"
	"github.com/saurabhhbansal/r2backup/internal/progress"
	"github.com/saurabhhbansal/r2backup/internal/remote"
	"github.com/saurabhhbansal/r2backup/test/fixtures"
)

// TestLockedFileIsReportedAndRunCompletes proves that a file this process
// cannot read -- locked by another application, or simply permission-denied
// -- does not stop the other files in the same run from being backed up. It
// must show up somewhere in the report (as a Failure, since the open itself
// is what fails here, past the point scan already succeeded in listing it)
// and every other file must still land.
func TestLockedFileIsReportedAndRunCompletes(t *testing.T) {
	h := newHarness(t, "Locked")
	manifest, err := fixtures.Build(h.root, fixtures.Spec{
		SmallFiles:    8,
		SmallFileSize: 256,
		Seed:          11,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(manifest.Files) == 0 {
		t.Fatal("fixture built no files to lock")
	}
	victim := manifest.Files[0]

	unlock, ineffective, err := lockFile(filepath.Join(h.root, filepath.FromSlash(victim)))
	if err != nil {
		t.Fatalf("lockFile: %v", err)
	}
	if ineffective {
		t.Skip("this process can read a file with its permissions stripped (likely running as root); a locked-file failure cannot be simulated here")
	}
	defer unlock()

	rep := h.backupRun(t)
	if rep.Succeeded() {
		t.Fatal("backup reported success, but the locked file should have failed")
	}

	var found bool
	for _, f := range rep.Failures {
		if f.Key == victim {
			found = true
		}
	}
	if !found {
		t.Fatalf("locked file %q was not reported among the failures: %+v", victim, rep.Failures)
	}
	wantUploaded := len(manifest.Files) - 1
	if rep.Uploaded != wantUploaded {
		t.Fatalf("Uploaded = %d, want %d -- every file except the locked one should have landed", rep.Uploaded, wantUploaded)
	}
}

// TestFileVanishesBetweenScanAndUpload proves the specific race a backup
// tool has to survive: a file exists when the tree is scanned and planned,
// but is gone by the time the transfer phase actually opens it (deleted by
// the user, a build tool, an editor's own temp-file cleanup). This uses
// Options.Observer's PhaseUploading callback -- which backup.Run calls
// synchronously, and only once, strictly after planning and strictly before
// the engine opens a single file -- to delete the file at exactly that
// boundary, deterministically rather than racing a background goroutine
// against real disk and network I/O.
func TestFileVanishesBetweenScanAndUpload(t *testing.T) {
	h := newHarness(t, "Vanishes")
	manifest, err := fixtures.Build(h.root, fixtures.Spec{
		SmallFiles:    8,
		SmallFileSize: 256,
		Seed:          12,
	})
	if err != nil {
		t.Fatal(err)
	}
	victim := manifest.Files[0]
	victimAbs := filepath.Join(h.root, filepath.FromSlash(victim))

	obs := &deleteAtUploadObserver{path: victimAbs}
	rep := h.backupRun(t, func(o *backup.Options) { o.Observer = obs })

	if !obs.deleted {
		t.Fatal("test bug: the observer never fired, so the file was never removed")
	}
	if rep.Succeeded() {
		t.Fatal("backup reported success, but the vanished file should have failed")
	}
	var found bool
	for _, f := range rep.Failures {
		if f.Key == victim {
			found = true
		}
	}
	if !found {
		t.Fatalf("vanished file %q was not reported among the failures: %+v", victim, rep.Failures)
	}
	wantUploaded := len(manifest.Files) - 1
	if rep.Uploaded != wantUploaded {
		t.Fatalf("Uploaded = %d, want %d -- every other file should still have landed", rep.Uploaded, wantUploaded)
	}
}

type deleteAtUploadObserver struct {
	path    string
	once    sync.Once
	deleted bool
}

func (o *deleteAtUploadObserver) Phase(p backup.Phase, r *backup.Report) {
	if p == backup.PhaseUploading {
		o.once.Do(func() {
			if err := os.Remove(o.path); err == nil {
				o.deleted = true
			}
		})
	}
}
func (o *deleteAtUploadObserver) Progress(progress.Snapshot) {}

// TestFileModifiedOnceDuringUploadIsRetriedAndSucceeds proves the one-retry
// rule: a file that changes exactly once while it is being read -- a save
// that lands mid-transfer -- is caught by comparing size/mtime before and
// after the read, retried exactly once, and (since nothing disturbs the
// second attempt) stored as whatever the file actually contained by the
// time the retry ran, never a torn mix of old and new bytes.
func TestFileModifiedOnceDuringUploadIsRetriedAndSucceeds(t *testing.T) {
	const relPath = "racy.bin"
	const size = 4 << 20 // 4MiB: big enough to be a real multi-chunk HTTP body

	newContent := bytes.Repeat([]byte{0xB2}, size)
	farFuture := time.Now().Add(2 * time.Hour)

	// h is assigned below; the hook only runs once backupRun calls it,
	// well after this closure is done being constructed, so capturing the
	// not-yet-assigned variable by reference is safe.
	var h *harness
	h = newHarnessWithClient(t, "RacyOnce", withPutHook(relPath, func(attempt int) {
		if attempt != 1 {
			return // leave the retry alone; it must succeed cleanly
		}
		if err := os.WriteFile(filepath.Join(h.root, relPath), newContent, 0o644); err != nil {
			t.Errorf("mutate during attempt 1: %v", err)
			return
		}
		if err := os.Chtimes(filepath.Join(h.root, relPath), farFuture, farFuture); err != nil {
			t.Errorf("chtimes during attempt 1: %v", err)
		}
	}))

	original := bytes.Repeat([]byte{0xA1}, size)
	if err := os.WriteFile(filepath.Join(h.root, relPath), original, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeRelFile(h.root, "companion.txt", []byte("unaffected sibling")); err != nil {
		t.Fatal(err)
	}

	rep := h.backupRun(t)
	if !rep.Succeeded() {
		t.Fatalf("backup reported failures: %+v", rep.Failures)
	}
	if rep.Uploaded != 2 {
		t.Fatalf("Uploaded = %d, want 2", rep.Uploaded)
	}

	// The object in the bucket must match what is on disk right now (the
	// mutated content), never the original bytes and never some mix.
	obj, err := h.client.Get(context.Background(), h.currentPrefix()+relPath)
	if err != nil {
		t.Fatalf("get %s: %v", relPath, err)
	}
	defer obj.Body.Close()
	got := make([]byte, size)
	if _, err := io.ReadFull(obj.Body, got); err != nil {
		t.Fatalf("read stored object: %v", err)
	}
	if !bytes.Equal(got, newContent) {
		t.Fatal("stored object does not match the file's content after the retry; it may have stored a torn mix of old and new bytes")
	}

	target := t.TempDir()
	restoreRep := h.restoreInto(t, target)
	if !restoreRep.Succeeded() {
		t.Fatalf("restore reported failures: %+v", restoreRep.Failures)
	}
	assertCleanRoundTrip(t, h.root, target)
}

// TestFileModifiedTwiceDuringUploadFailsWithoutStoringTornData proves the
// other half: a file under constant, unrelated churn -- changing again
// during the one retry it was given -- must be reported as a failure rather
// than looped on forever, and critically must never leave a torn object
// behind at that key. Since this is the file's first-ever backup, "never
// torn" here means the key simply does not exist afterwards.
func TestFileModifiedTwiceDuringUploadFailsWithoutStoringTornData(t *testing.T) {
	const relPath = "racy.bin"
	const size = 4 << 20

	var h *harness
	h = newHarnessWithClient(t, "RacyTwice", withPutHook(relPath, func(attempt int) {
		// Every attempt gets a fresh rewrite plus a fresh timestamp, so the
		// post-upload stat never matches the pre-upload one -- not on the
		// first attempt, and not on the one retry either.
		newContent := bytes.Repeat([]byte{byte(0xC0 + attempt)}, size)
		path := filepath.Join(h.root, relPath)
		if err := os.WriteFile(path, newContent, 0o644); err != nil {
			t.Errorf("mutate during attempt %d: %v", attempt, err)
			return
		}
		stamp := time.Now().Add(time.Duration(attempt) * time.Hour)
		if err := os.Chtimes(path, stamp, stamp); err != nil {
			t.Errorf("chtimes during attempt %d: %v", attempt, err)
		}
	}))

	original := bytes.Repeat([]byte{0xA1}, size)
	if err := os.WriteFile(filepath.Join(h.root, relPath), original, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeRelFile(h.root, "companion.txt", []byte("unaffected sibling")); err != nil {
		t.Fatal(err)
	}

	rep := h.backupRun(t)
	if rep.Succeeded() {
		t.Fatal("backup reported success, but the file changed on both the original attempt and the retry")
	}
	if len(rep.Failures) != 1 || rep.Failures[0].Key != relPath {
		t.Fatalf("Failures = %+v, want exactly one failure for %q", rep.Failures, relPath)
	}
	if rep.Uploaded != 1 {
		t.Fatalf("Uploaded = %d, want 1 (the unaffected sibling)", rep.Uploaded)
	}

	_, err := h.client.Get(context.Background(), h.currentPrefix()+relPath)
	if !errors.Is(err, remote.ErrNotFound) {
		t.Fatalf("get %s: got err=%v, want a not-found error -- a failed upload must never leave a torn object behind", relPath, err)
	}
}
