package cli

import (
	"context"
	"errors"
	"os"
	"time"

	"github.com/saurabhhbansal/r2backup/internal/config"
	"github.com/saurabhhbansal/r2backup/internal/runstate"
	"github.com/saurabhhbansal/r2backup/internal/schedule"
	"github.com/saurabhhbansal/r2backup/internal/ui"
)

// carryOnAfter is how long the panel waits, once it starts, before carrying
// on a backup the computer's shutting down cut off. It matches the delay on
// the scheduled task's sign-in trigger, so the two read as one behaviour, and
// it leaves the first minute after signing in to everything else starting.
var carryOnAfter = time.Minute

// carryOn starts an interrupted backup again by itself, a little after the
// panel starts -- which is normally signing in. A backup cut off by a
// shutdown, a restart or the lid closing used to sit there with a "Carry on"
// button until someone pressed it, unless automatic backups were on.
//
// It does nothing when automatic backups are on: the scheduled task has a
// sign-in trigger of its own, and two runs would only fight over the index.
// Nor when a backup is already running (here, in the scheduler, in a
// terminal), nor when the person stopped that backup themselves.
func (t *trayBackend) carryOn(ctx context.Context, wait time.Duration) {
	if !t.shouldCarryOn() {
		return
	}
	select {
	case <-ctx.Done():
		return
	case <-time.After(wait):
	}
	// Asked again: a minute is long enough for the person to have pressed
	// "Carry on", a terminal run to have started, or the schedule to have
	// been turned on.
	if !t.shouldCarryOn() {
		return
	}
	if err := carryOnStart(t); err != nil {
		t.say(err.Error())
	}
}

// carryOnStart is what carrying on does: the same as the "Carry on" button,
// which backs up every folder. A seam for tests, which must not start r2b.
var carryOnStart = func(t *trayBackend) error { return t.BackUp("") }

// shouldCarryOn reports whether there is an interrupted backup the panel
// should start again by itself.
func (t *trayBackend) shouldCarryOn() bool {
	p, err := config.ProgressPath()
	if err != nil {
		return false
	}
	now := time.Now()
	stopped, ok := runstate.ReadInterrupted(p, now)
	if !ok || stoppedByHand(p, stopped) {
		return false
	}
	// Its folder was removed since: nothing to carry on.
	_ = t.d.app.sets.Reload()
	if _, err := t.d.app.sets.Get(stopped.Set); err != nil {
		return false
	}
	var ov ui.Overview
	if applyLiveRun(&ov, now) || t.Busy() != "" {
		return false
	}
	// Automatic backups on: the scheduled task's sign-in trigger carries on
	// by itself. When the scheduler cannot be read, it is left to that too,
	// rather than risk two runs.
	st, err := t.d.currentSchedule()
	if err != nil && !errors.Is(err, schedule.ErrUnsupported) {
		return false
	}
	if st.Registered {
		return false
	}
	// Another r2b holds the index -- a backup that has not written progress
	// yet, a restore, the terminal interface. Its turn first.
	_, release, err := t.d.app.checkoutIndex()
	if err != nil {
		return false
	}
	release()
	return true
}

// stoppedMarker is the file the panel touches when someone stops a backup
// from it. A stop kills the run, which leaves its progress behind just as a
// shutdown would; this is how the two are told apart.
func stoppedMarker(progressPath string) string { return progressPath + ".stopped" }

// markStoppedByHand records that the person stopped the running backup.
func markStoppedByHand() {
	p, err := config.ProgressPath()
	if err != nil {
		return
	}
	_ = os.WriteFile(stoppedMarker(p), nil, 0o600)
	now := time.Now()
	_ = os.Chtimes(stoppedMarker(p), now, now)
}

// stoppedByHand reports whether the interrupted run is one the person
// stopped: the marker was touched after the run last wrote its progress.
func stoppedByHand(progressPath string, l *runstate.Live) bool {
	fi, err := os.Stat(stoppedMarker(progressPath))
	if err != nil {
		return false
	}
	return !fi.ModTime().Before(l.UpdatedAt)
}
