package cli

import (
	"context"
	"testing"
	"time"

	"github.com/saurabhhbansal/r2backup/internal/config"
	"github.com/saurabhhbansal/r2backup/internal/runstate"
	"github.com/saurabhhbansal/r2backup/internal/schedule"
	"github.com/saurabhhbansal/r2backup/internal/sets"
	"github.com/saurabhhbansal/r2backup/internal/tray"
	"github.com/saurabhhbansal/r2backup/internal/ui"
)

// Removing the only folder while its backup was cut off used to leave the
// panel saying "<folder> was interrupted" with a Carry on button that could
// not work, and the schedule firing at nothing. Now the panel goes back to
// how it looks before any folder was added.
func TestRemovingTheLastFolderLeavesNothingInterruptedOrScheduled(t *testing.T) {
	t.Setenv("R2BACKUP_DATA_DIR", t.TempDir())
	origCurrent, origRemove := scheduleCurrent, scheduleRemove
	t.Cleanup(func() { scheduleCurrent, scheduleRemove = origCurrent, origRemove })
	registered := true
	scheduleCurrent = func(string) (schedule.Status, error) {
		return schedule.Status{Registered: registered, Interval: 30 * time.Minute}, nil
	}
	removed := 0
	scheduleRemove = func(string) error { removed++; registered = false; return nil }

	seedSetForRemoveBusyTest(t, sets.Set{Name: "Code Projects", Root: t.TempDir(), Machine: "testpc",
		Prefix: "machines/testpc/Code Projects", RetentionDays: 30})
	p, err := config.ProgressPath()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := runstate.WriteLive(p, runstate.Live{Set: "Code Projects", PID: 1 << 30,
		StartedAt: now.Add(-time.Hour), UpdatedAt: now.Add(-10 * time.Minute)}); err != nil {
		t.Fatal(err)
	}

	d, err := openDashboard(&Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer d.close()
	if err := d.Remove(context.Background(), "Code Projects", false); err != nil {
		t.Fatal(err)
	}

	if _, ok := runstate.ReadInterrupted(p, time.Now()); ok {
		t.Error("the removed folder's cut-off run is still there to be reported")
	}
	if schedule.Supported() && removed != 1 {
		t.Errorf("schedule removed %d times, want once: nothing is left to back up", removed)
	}
	views, ov, err := d.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st := tray.Summarize(views, ov, nil, time.Now()); st.Status.Kind == tray.KindInterrupted {
		t.Errorf("panel still says %q", st.Status.Title)
	}
}

// Another folder left: the schedule stays, it still has work to do.
func TestRemovingOneOfTwoFoldersKeepsTheSchedule(t *testing.T) {
	t.Setenv("R2BACKUP_DATA_DIR", t.TempDir())
	origCurrent, origRemove := scheduleCurrent, scheduleRemove
	t.Cleanup(func() { scheduleCurrent, scheduleRemove = origCurrent, origRemove })
	scheduleCurrent = func(string) (schedule.Status, error) {
		return schedule.Status{Registered: true, Interval: 30 * time.Minute}, nil
	}
	removed := 0
	scheduleRemove = func(string) error { removed++; return nil }

	for _, n := range []string{"Photos", "Documents"} {
		seedSetForRemoveBusyTest(t, sets.Set{Name: n, Root: t.TempDir(), Machine: "testpc",
			Prefix: "machines/testpc/" + n, RetentionDays: 30})
	}
	d, err := openDashboard(&Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer d.close()
	if err := d.Remove(context.Background(), "Photos", false); err != nil {
		t.Fatal(err)
	}
	if removed != 0 {
		t.Error("the schedule was removed while a folder was still being backed up")
	}
}

// Someone who only copies Drime folders to the second copy has no folders
// here at all, and the schedule is what does that copying.
func TestRemovingTheLastFolderKeepsTheScheduleForDrimeCopies(t *testing.T) {
	t.Setenv("R2BACKUP_DATA_DIR", t.TempDir())
	origCurrent, origRemove := scheduleCurrent, scheduleRemove
	t.Cleanup(func() { scheduleCurrent, scheduleRemove = origCurrent, origRemove })
	scheduleCurrent = func(string) (schedule.Status, error) {
		return schedule.Status{Registered: true, Interval: 30 * time.Minute}, nil
	}
	removed := 0
	scheduleRemove = func(string) error { removed++; return nil }

	st, err := config.LoadSettings()
	if err != nil {
		t.Fatal(err)
	}
	st.MirrorSources = []config.MirrorSource{{Name: "Photos"}}
	if err := config.SaveSettings(st); err != nil {
		t.Fatal(err)
	}
	seedSetForRemoveBusyTest(t, sets.Set{Name: "Documents", Root: t.TempDir(), Machine: "testpc",
		Prefix: "machines/testpc/Documents", RetentionDays: 30})
	d, err := openDashboard(&Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer d.close()
	if err := d.Remove(context.Background(), "Documents", false); err != nil {
		t.Fatal(err)
	}
	if removed != 0 {
		t.Error("the schedule was removed though it still copies Drime folders")
	}
}

// A cut-off run of a folder removed some other way (another version, by
// hand) is not offered for carrying on either.
func TestAnInterruptedRunOfAGoneFolderIsNotShown(t *testing.T) {
	ov := ui.Overview{Configured: true, Interrupted: "Gone"}
	st := tray.Summarize(nil, ov, nil, time.Now())
	if st.Status.Kind == tray.KindInterrupted || st.Status.Action == "backup" {
		t.Errorf("status = %+v, want the no-folders state", st.Status)
	}
}
