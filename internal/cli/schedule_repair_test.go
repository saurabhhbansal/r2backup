package cli

import (
	"bytes"
	"fmt"
	"testing"
	"time"

	"github.com/saurabhhbansal/r2backup/internal/schedule"
)

// fakeScheduleInstall stands in for schedule.Install. It records the Entry it
// was given, but first rejects a non-positive Interval exactly the way
// Entry.validate (internal/schedule/schedule.go) does -- the real error this
// finding is about -- so a regression here fails the same way it would
// against the real scheduler, not just with a wrong number recorded.
func fakeScheduleInstall(installed *schedule.Entry) func(schedule.Entry) error {
	return func(e schedule.Entry) error {
		if e.Interval <= 0 {
			return fmt.Errorf("schedule: Entry.Interval must be positive, got %s", e.Interval)
		}
		*installed = e
		return nil
	}
}

// This is the finding: `r2b schedule --repair` reads back whatever the
// scheduler reports and hands its Interval straight to schedule.Install. On
// the crontab fallback (internal/schedule/schedule_linux.go's Current, past
// the point where no systemd unit file exists) that report is Registered
// with a zero Interval -- cron has no notion of "every N minutes" for this
// package to read back, only the marker line proving the entry exists.
// Install then rejects the zero with "Entry.Interval must be positive", and
// the installers run --repair right after replacing the files, so this is
// the automated path failing, not a rare hand-typed one.
//
// scheduleCurrent and scheduleInstall (commands.go) are the seam that lets
// this be driven through the real cobra command without touching whatever
// scheduler this test happens to run under.
func TestRepairFallsBackToDefaultIntervalWhenTheSchedulerCannotSayOne(t *testing.T) {
	origCurrent, origInstall := scheduleCurrent, scheduleInstall
	t.Cleanup(func() { scheduleCurrent, scheduleInstall = origCurrent, origInstall })

	var installed schedule.Entry
	scheduleCurrent = func(string) (schedule.Status, error) {
		return schedule.Status{Registered: true, Interval: 0}, nil
	}
	scheduleInstall = fakeScheduleInstall(&installed)

	var out bytes.Buffer
	root := NewRoot(&Options{Out: &out, Err: &out})
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"schedule", "--repair"})
	if err := root.Execute(); err != nil {
		t.Fatalf("schedule --repair with a zero-interval report returned an error: %v", err)
	}
	want := time.Duration(schedule.DefaultIntervalMinutes) * time.Minute
	if installed.Interval != want {
		t.Errorf("installed with Interval = %s, want the default %s", installed.Interval, want)
	}
}

// A scheduler that can say how often the entry fires -- the ordinary case --
// must keep that interval exactly, not be quietly overridden to the default.
func TestRepairKeepsAKnownInterval(t *testing.T) {
	origCurrent, origInstall := scheduleCurrent, scheduleInstall
	t.Cleanup(func() { scheduleCurrent, scheduleInstall = origCurrent, origInstall })

	var installed schedule.Entry
	scheduleCurrent = func(string) (schedule.Status, error) {
		return schedule.Status{Registered: true, Interval: 15 * time.Minute}, nil
	}
	scheduleInstall = fakeScheduleInstall(&installed)

	var out bytes.Buffer
	root := NewRoot(&Options{Out: &out, Err: &out})
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"schedule", "--repair"})
	if err := root.Execute(); err != nil {
		t.Fatalf("schedule --repair returned an error: %v", err)
	}
	if installed.Interval != 15*time.Minute {
		t.Errorf("installed with Interval = %s, want the 15m the scheduler reported", installed.Interval)
	}
}

// Nothing registered means nothing to repair, and schedule.Install -- which
// would register something new rather than repair anything -- must never
// run. This is the guard the long comment in the repair block explains: a
// machine with no schedule must be left with no schedule.
func TestRepairWithNothingRegisteredInstallsNothing(t *testing.T) {
	origCurrent, origInstall := scheduleCurrent, scheduleInstall
	t.Cleanup(func() { scheduleCurrent, scheduleInstall = origCurrent, origInstall })

	scheduleCurrent = func(string) (schedule.Status, error) {
		return schedule.Status{}, nil
	}
	scheduleInstall = func(schedule.Entry) error {
		t.Fatal("schedule.Install was called with nothing registered")
		return nil
	}

	var out bytes.Buffer
	root := NewRoot(&Options{Out: &out, Err: &out})
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"schedule", "--repair"})
	if err := root.Execute(); err != nil {
		t.Fatalf("schedule --repair returned an error: %v", err)
	}
}

// repairMinutes is the exact logic the finding was about, unit tested
// directly so a future change to either call site -- this command, or the
// dashboard's RepairSchedule in dashboard.go -- that stops calling it shows
// up here even if the end-to-end command tests above were ever weakened.
func TestRepairMinutes(t *testing.T) {
	cases := []struct {
		name string
		st   schedule.Status
		want int
	}{
		{"zero interval falls back to the default", schedule.Status{Registered: true, Interval: 0}, schedule.DefaultIntervalMinutes},
		{"a known interval is kept", schedule.Status{Registered: true, Interval: 45 * time.Minute}, 45},
		{"rounds to the nearest minute", schedule.Status{Registered: true, Interval: 44*time.Minute + 40*time.Second}, 45},
	}
	for _, c := range cases {
		if got := repairMinutes(c.st); got != c.want {
			t.Errorf("%s: repairMinutes(%+v) = %d, want %d", c.name, c.st, got, c.want)
		}
	}
}
