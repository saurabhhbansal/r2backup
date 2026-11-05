package cli

import (
	"os"
	"testing"

	"github.com/saurabhhbansal/r2backup/internal/config"
	"github.com/saurabhhbansal/r2backup/internal/pause"
	"github.com/saurabhhbansal/r2backup/internal/runstate"
)

// The panel's Pause and Resume reach whichever run is publishing progress --
// here a run in this very process, standing in for a scheduled one.
func TestPanelPausesAndResumesTheRunningBackup(t *testing.T) {
	tb := trayForTest(t)
	if _, err := call(t, tb, "backupPause", nil); err == nil {
		t.Error("pausing with nothing running should say so")
	}

	progressPath, err := config.ProgressPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := runstate.WriteLive(progressPath, runstate.Live{Set: "Photos", Phase: "uploading", PID: os.Getpid()}); err != nil {
		t.Fatal(err)
	}
	marker := pause.Path(progressPath)

	if _, err := call(t, tb, "backupPause", nil); err != nil {
		t.Fatalf("backupPause: %v", err)
	}
	if !pause.For(marker, os.Getpid()) {
		t.Fatal("backupPause did not pause the running backup")
	}
	if _, err := call(t, tb, "backupResume", nil); err != nil {
		t.Fatalf("backupResume: %v", err)
	}
	if pause.For(marker, os.Getpid()) {
		t.Fatal("backupResume left the backup paused")
	}
}

// A new run never inherits a pause it was not given.
func TestARunStartsUnpaused(t *testing.T) {
	dir := t.TempDir()
	progressPath := dir + string(os.PathSeparator) + "progress.json"
	marker := pause.Path(progressPath)
	if err := pause.Set(marker, os.Getpid()+1); err != nil { // another, older run's
		t.Fatal(err)
	}
	ctx, gate, stop := watchPause(t.Context(), progressPath)
	defer stop()
	if gate.Paused() {
		t.Fatal("a new run started paused")
	}
	if pause.FromContext(ctx) == nil {
		t.Fatal("the run's context carries no gate")
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Error("a stale marker was left behind")
	}
}
