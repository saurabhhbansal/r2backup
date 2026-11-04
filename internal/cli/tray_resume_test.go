package cli

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/saurabhhbansal/r2backup/internal/config"
	"github.com/saurabhhbansal/r2backup/internal/runstate"
	"github.com/saurabhhbansal/r2backup/internal/schedule"
)

// A backup cut off by a shutdown used to wait for someone to press "Carry
// on" unless automatic backups were on. The panel, started at sign-in, now
// carries on by itself -- except where that would be a second run, or undo
// the person's own Stop.
func TestThePanelCarriesOnAnInterruptedBackup(t *testing.T) {
	cases := []struct {
		name      string
		progress  string // "interrupted", "live" or "" for none
		scheduled bool
		stopped   bool
		want      bool
	}{
		{name: "interrupted, no schedule", progress: "interrupted", want: true},
		{name: "the schedule carries on by itself", progress: "interrupted", scheduled: true},
		{name: "nothing was interrupted", progress: ""},
		{name: "a backup is running", progress: "live"},
		{name: "the person stopped it", progress: "interrupted", stopped: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("R2BACKUP_DATA_DIR", t.TempDir())
			origCurrent, origStart := scheduleCurrent, carryOnStart
			t.Cleanup(func() { scheduleCurrent, carryOnStart = origCurrent, origStart })
			scheduleCurrent = func(string) (schedule.Status, error) {
				return schedule.Status{Registered: c.scheduled, Interval: 30 * time.Minute}, nil
			}
			started := 0
			carryOnStart = func(*trayBackend) error { started++; return nil }

			p, err := config.ProgressPath()
			if err != nil {
				t.Fatal(err)
			}
			now := time.Now()
			switch c.progress {
			case "interrupted":
				// Last written ten minutes ago by a process long gone.
				if err := runstate.WriteLive(p, runstate.Live{Set: "Documents", PID: 1 << 30,
					StartedAt: now.Add(-time.Hour), UpdatedAt: now.Add(-10 * time.Minute)}); err != nil {
					t.Fatal(err)
				}
			case "live":
				if err := runstate.WriteLive(p, runstate.Live{Set: "Documents", PID: os.Getpid(),
					StartedAt: now.Add(-time.Minute), UpdatedAt: now}); err != nil {
					t.Fatal(err)
				}
			}
			if c.stopped {
				markStoppedByHand()
			}

			d, err := openDashboard(&Options{})
			if err != nil {
				t.Fatal(err)
			}
			defer d.close()
			tb := &trayBackend{d: d}
			tb.carryOn(context.Background(), time.Millisecond)

			if got := started == 1; got != c.want {
				t.Errorf("started %d backups, want carrying on = %v", started, c.want)
			}
		})
	}
}

// A run the person stopped before is not mistaken for this one: a later
// backup that a shutdown cut off is carried on.
func TestAnEarlierStopDoesNotHoldBackALaterInterruption(t *testing.T) {
	t.Setenv("R2BACKUP_DATA_DIR", t.TempDir())
	p, err := config.ProgressPath()
	if err != nil {
		t.Fatal(err)
	}
	markStoppedByHand()
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(stoppedMarker(p), old, old); err != nil {
		t.Fatal(err)
	}
	l := &runstate.Live{UpdatedAt: time.Now().Add(-10 * time.Minute)}
	if stoppedByHand(p, l) {
		t.Error("a stop an hour ago was read as stopping a run that was still going ten minutes ago")
	}
	l.UpdatedAt = old.Add(-time.Minute)
	if !stoppedByHand(p, l) {
		t.Error("the run the person stopped was not recognised")
	}
}
