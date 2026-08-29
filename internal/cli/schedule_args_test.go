package cli

import (
	"bytes"
	"path/filepath"
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

// What the scheduler is pointed at is the whole fix for the console window.
// r2backup cannot hide its own console reliably -- the loader creates it
// before any of its code runs, and a real desktop showed the window both
// before and after that was attempted -- so the task has to run the
// GUI-subsystem launcher instead.
func TestWindowsSchedulesTheWindowlessLauncher(t *testing.T) {
	const self = `C:\Users\me\AppData\Local\Programs\r2backup\r2backup.exe`
	launcher := filepath.Join(filepath.Dir(self), LauncherName)

	got, windowless := scheduledBinary("windows", self, func(p string) bool { return p == launcher })
	if got != launcher {
		t.Errorf("scheduled %q, want the launcher %q: pointing the task at the console binary is the bug", got, launcher)
	}
	if !windowless {
		t.Error("reported that a window will appear when the launcher is present")
	}
}

// A build straight from `go build`, or an install where only the one file was
// copied, must still get a working schedule -- and must be told the truth
// about it rather than promising something it cannot deliver.
func TestAMissingLauncherFallsBackAndSaysSo(t *testing.T) {
	const self = `C:\Users\me\r2backup.exe`
	got, windowless := scheduledBinary("windows", self, func(string) bool { return false })
	if got != self {
		t.Errorf("scheduled %q, want a fallback to %q", got, self)
	}
	if windowless {
		t.Error("claimed nothing appears on screen while pointing the task at a console binary")
	}
}

// Off Windows there is no launcher and never was a window: systemd and
// launchd start a process with no terminal at all.
func TestOtherPlatformsScheduleTheBinaryItself(t *testing.T) {
	for _, goos := range []string{"linux", "darwin"} {
		got, windowless := scheduledBinary(goos, "/usr/local/bin/r2backup", func(string) bool {
			t.Errorf("%s: looked for a launcher, which only exists on Windows", goos)
			return true
		})
		if got != "/usr/local/bin/r2backup" || !windowless {
			t.Errorf("scheduledBinary(%s) = %q, %v; want the binary itself and no window", goos, got, windowless)
		}
	}
}
