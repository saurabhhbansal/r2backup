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

// `add` used to end by printing a command the user had to notice, remember
// and run. Anyone who did not was left with a folder backed up exactly once,
// which is the opposite of what they asked for. The offer is the fix, and it
// follows the same rule as every other prompt here: only when a person is
// there to answer.
func TestSchedulingIsOfferedOnlyWhenSomeoneCanAnswer(t *testing.T) {
	for _, o := range []*Options{{Yes: true}, {No: true}} {
		if o.Decision() == Ask {
			t.Fatalf("Options{Yes:%v No:%v} reports Ask; an unattended add would block on a prompt", o.Yes, o.No)
		}
	}
}

func TestAskYesNo(t *testing.T) {
	cases := []struct {
		name string
		in   string
		def  bool
		want bool
	}{
		{"enter takes the default", "\n", true, true},
		{"enter takes the default, the other way", "\n", false, false},
		{"y", "y\n", false, true},
		{"yes", "YES\n", false, true},
		{"n", "n\n", true, false},
		{"no", "No\n", true, false},
		{"anything else takes the default", "maybe\n", true, true},
		// Nothing to read at all: take the default rather than hang.
		{"EOF", "", true, true},
		{"EOF, safe default", "", false, false},
	}
	for _, c := range cases {
		var out bytes.Buffer
		if got := askYesNo(&out, strings.NewReader(c.in), "Do it?", c.def); got != c.want {
			t.Errorf("%s: askYesNo(%q, def=%v) = %v, want %v", c.name, c.in, c.def, got, c.want)
		}
		if !strings.Contains(out.String(), "Do it?") {
			t.Errorf("%s: the question was not asked", c.name)
		}
	}
	// The default is the one shown capitalised.
	var out bytes.Buffer
	askYesNo(&out, strings.NewReader("\n"), "Q", true)
	if !strings.Contains(out.String(), "[Y/n]") {
		t.Errorf("a default of yes should render [Y/n], got %q", out.String())
	}
	out.Reset()
	askYesNo(&out, strings.NewReader("\n"), "Q", false)
	if !strings.Contains(out.String(), "[y/N]") {
		t.Errorf("a default of no should render [y/N], got %q", out.String())
	}
}

// The excludes a set carries are editable after the fact. They were not: the
// picker had exactly one caller, so the choice was made once at creation and
// the only way to change it afterwards was to hand-edit sets.json.
func TestEditExistsAndTakesASet(t *testing.T) {
	root := NewRoot(&Options{})
	for _, c := range root.Commands() {
		if c.Name() != "edit" {
			continue
		}
		if c.Short == "" {
			t.Error("edit has no short description")
		}
		if err := c.Args(c, []string{}); err == nil {
			t.Error("edit accepted no arguments; it needs a set name")
		}
		if err := c.Args(c, []string{"one"}); err != nil {
			t.Errorf("edit rejected a single set name: %v", err)
		}
		return
	}
	t.Fatal("edit command not found")
}

func TestDiffExcludesNamesWhatChanged(t *testing.T) {
	added, removed := diffExcludes(
		[]string{"code/node_modules", "docs/old"},
		[]string{"code/node_modules", "photos/raw"},
	)
	if !slices.Equal(added, []string{"photos/raw"}) {
		t.Errorf("added = %v, want [photos/raw]", added)
	}
	if !slices.Equal(removed, []string{"docs/old"}) {
		t.Errorf("removed = %v, want [docs/old]", removed)
	}
	// Unchanged means unchanged, so `edit` can say "No change" rather than
	// running a backup for nothing.
	a, r := diffExcludes([]string{"x"}, []string{"x"})
	if len(a) != 0 || len(r) != 0 {
		t.Errorf("an unchanged list reported added=%v removed=%v", a, r)
	}
}
