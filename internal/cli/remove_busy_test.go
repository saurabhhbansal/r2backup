package cli

import (
	"strings"
	"testing"

	"github.com/saurabhhbansal/r2backup/internal/config"
	"github.com/saurabhhbansal/r2backup/internal/runstate"
	"github.com/saurabhhbansal/r2backup/internal/sets"
)

// r2b remove must refuse a set that a live progress file says is being
// backed up right now, and it must say so before doing anything -- no
// bucket connection is needed, no --purge flag is passed, so nothing here
// exercises the checkoutIndex lock at all. It exists because the interface
// already has this guard (see internal/ui's busy guard on x and X) and the
// command line had nothing: a scheduled backup and a `remove` typed by hand
// could both be touching the same index bucket at once with no cross-check
// between them.
func TestRemoveRefusesTheSetItsOwnLiveProgressFileNamesAsRunning(t *testing.T) {
	t.Setenv("R2BACKUP_DATA_DIR", t.TempDir())

	set := sets.Set{
		Name: "Photos", Root: t.TempDir(), Machine: "testpc",
		Prefix: "machines/testpc/Photos", RetentionDays: 30,
	}
	seedSetForRemoveBusyTest(t, set)

	progressPath, err := config.ProgressPath()
	if err != nil {
		t.Fatal(err)
	}
	// PID and UpdatedAt are stamped by WriteLive itself, to this test
	// process and to now -- exactly what a run still going would look
	// like: Stale checks both, and this process is certainly alive.
	if err := runstate.WriteLive(progressPath, runstate.Live{Set: set.Name}); err != nil {
		t.Fatal(err)
	}

	out, err := runRemove(t, set.Name)
	if err == nil {
		t.Fatalf("remove of a set its own progress file says is running should have failed; output:\n%s", out)
	}
	if got := err.Error(); !strings.Contains(got, "Photos") || !strings.Contains(got, "being backed up") {
		t.Errorf("error should name the set and say it is being backed up, got %q", got)
	}

	// Nothing below must have run: the set is still there to remove once
	// the backup finishes.
	a, err := openApp()
	if err != nil {
		t.Fatal(err)
	}
	defer a.close()
	if _, err := a.sets.Get(set.Name); err != nil {
		t.Errorf("the set was removed even though its own backup was running: %v", err)
	}
}

// Removing a set is fine while a different set is the one being backed up --
// the live progress file names exactly one set, and only that one may be in
// the middle of a run that this check needs to protect.
func TestRemoveOfADifferentSetIsNotRefusedWhileOneRuns(t *testing.T) {
	t.Setenv("R2BACKUP_DATA_DIR", t.TempDir())

	running := sets.Set{
		Name: "Photos", Root: t.TempDir(), Machine: "testpc",
		Prefix: "machines/testpc/Photos", RetentionDays: 30,
	}
	other := sets.Set{
		Name: "Documents", Root: t.TempDir(), Machine: "testpc",
		Prefix: "machines/testpc/Documents", RetentionDays: 30,
	}
	seedSetForRemoveBusyTest(t, running)
	seedSetForRemoveBusyTest(t, other)

	progressPath, err := config.ProgressPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := runstate.WriteLive(progressPath, runstate.Live{Set: running.Name}); err != nil {
		t.Fatal(err)
	}

	out, err := runRemove(t, other.Name)
	if err != nil {
		t.Fatalf("remove of a set that is not the one running should have succeeded: %v\noutput:\n%s", err, out)
	}

	a, err := openApp()
	if err != nil {
		t.Fatal(err)
	}
	defer a.close()
	if _, err := a.sets.Get(other.Name); err == nil {
		t.Error("the other set should have been removed")
	}
	if _, err := a.sets.Get(running.Name); err != nil {
		t.Errorf("the running set should have been left alone: %v", err)
	}
}

// seedSetForRemoveBusyTest opens a fresh app under the test's data dir and
// records one set, the same way seedAppForRemoveTest does for the upload
// tests -- but with no bucket, since neither test here reaches --purge or
// checkoutIndex at all.
func seedSetForRemoveBusyTest(t *testing.T, set sets.Set) {
	t.Helper()
	a, err := openApp()
	if err != nil {
		t.Fatalf("openApp: %v", err)
	}
	defer a.close()
	if err := a.sets.Add(set); err != nil {
		t.Fatalf("add set: %v", err)
	}
}
