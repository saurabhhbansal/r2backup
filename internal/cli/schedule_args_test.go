package cli

import (
	"bytes"
	"slices"
	"strings"
	"testing"

	"github.com/saurabhhbansal/r2backup/internal/winconsole"
)

// The Windows scheduled command line has to carry --hidden. Without it the
// task runs a console-subsystem binary in the interactive session and leaves a
// console window on the desktop for the whole length of the backup -- which is
// what happened on a real desktop, while every scheduler test in the project
// passed. Nothing in CI can see a desktop, so the command line is asserted
// instead.
func TestWindowsScheduledRunsAskToHideTheirConsole(t *testing.T) {
	win := scheduledRunArgs("windows")
	if len(win) == 0 || win[0] != "backup" {
		t.Fatalf("scheduledRunArgs(windows) = %q, want it to run backup", win)
	}
	if !slices.Contains(win, winconsole.HiddenFlag) {
		t.Errorf("scheduledRunArgs(windows) = %q, missing %s: the task would put a console window on the desktop",
			win, winconsole.HiddenFlag)
	}
	// And the flag has to be one the argument scan actually recognizes.
	if !winconsole.WantsHidden(win) {
		t.Errorf("winconsole.WantsHidden(%q) = false: the flag written and the flag read disagree", win)
	}
}

// Off Windows there is no console to hide -- systemd and launchd start a
// process with no terminal at all -- so the flag would be noise in a unit file
// a user can read.
func TestOtherPlatformsScheduleAPlainBackup(t *testing.T) {
	for _, goos := range []string{"linux", "darwin"} {
		got := scheduledRunArgs(goos)
		if !slices.Equal(got, []string{"backup"}) {
			t.Errorf("scheduledRunArgs(%s) = %q, want [backup]", goos, got)
		}
	}
}

// The flag the scheduler writes must be one cobra will parse. If it is not
// registered, every scheduled run on Windows dies with "unknown flag" -- on a
// machine with no one watching, reporting only through the task's exit code.
func TestTheHiddenFlagIsAcceptedNotJustWritten(t *testing.T) {
	root := NewRoot(&Options{})
	f := root.PersistentFlags().Lookup(winconsole.HiddenFlagName)
	if f == nil {
		t.Fatalf("--%s is not registered on the root command", winconsole.HiddenFlagName)
	}
	if !f.Hidden {
		t.Errorf("--%s should not appear in --help: schedule is the only thing that passes it",
			winconsole.HiddenFlagName)
	}

	// Executing it still fails -- there are no credentials in a test
	// environment -- but it must not fail because of the flag.
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(scheduledRunArgs("windows"))
	err := root.Execute()
	if err != nil && strings.Contains(err.Error(), "unknown flag") {
		t.Fatalf("the scheduled command line is not parseable: %v", err)
	}
}
