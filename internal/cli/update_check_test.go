package cli

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/saurabhhbansal/r2backup/internal/config"
)

// child is a stand-in for a real subcommand: the update check keys off having
// a parent, because the root command with no subcommand is the dashboard.
func child(name string) *cobra.Command {
	root := &cobra.Command{Use: "r2b"}
	c := &cobra.Command{Use: name}
	root.AddCommand(c)
	return c
}

// The check must never run where nobody can answer it. A scheduled 3am backup
// has no terminal, and a prompt there would either hang the run or scribble
// into a log.
func TestUpdateCheckSkipsWhenNobodyCanAnswer(t *testing.T) {
	t.Setenv(config.EnvDataDir, t.TempDir())

	// Options.In set means a driven conversation, not a person -- and an
	// unexpected prompt would eat input meant for something else.
	if shouldCheckForUpdate(child("backup"), &Options{Out: os.Stderr, In: strings.NewReader("")}) {
		t.Error("checked for updates on a scripted run")
	}
	// --yes is for unattended use and must not read as consent to replace
	// the binary mid-pipeline.
	if shouldCheckForUpdate(child("backup"), &Options{Out: os.Stderr, Yes: true}) {
		t.Error("checked for updates under --yes")
	}
	if shouldCheckForUpdate(child("backup"), &Options{Out: os.Stderr, No: true}) {
		t.Error("checked for updates under --no")
	}
}

// The root command with no subcommand is the dashboard: a full-screen program
// that owns the terminal. A prompt printed as it tears down would land in a
// screen being restored.
func TestUpdateCheckSkipsTheDashboard(t *testing.T) {
	t.Setenv(config.EnvDataDir, t.TempDir())
	root := &cobra.Command{Use: "r2b"}
	if shouldCheckForUpdate(root, &Options{Out: os.Stderr}) {
		t.Error("checked for updates after the dashboard")
	}
}

func TestUpdateCheckSkipsUpdateItself(t *testing.T) {
	t.Setenv(config.EnvDataDir, t.TempDir())
	if shouldCheckForUpdate(child("update"), &Options{Out: os.Stderr}) {
		t.Error("checked for updates right after `r2b update`")
	}
}

// Someone who does not want a program phoning home on their behalf is
// entitled to an answer better than "edit a settings file".
func TestUpdateCheckHonoursTheOptOut(t *testing.T) {
	t.Setenv(config.EnvDataDir, t.TempDir())
	t.Setenv(EnvNoUpdateCheck, "1")
	if shouldCheckForUpdate(child("backup"), &Options{Out: os.Stderr}) {
		t.Error("checked for updates despite " + EnvNoUpdateCheck)
	}
}

// Nil guards, because PersistentPostRun is called by cobra and this must not
// be able to panic after a successful backup.
func TestUpdateCheckSurvivesNils(t *testing.T) {
	if shouldCheckForUpdate(nil, &Options{}) {
		t.Error("a nil command asked to check")
	}
	if shouldCheckForUpdate(child("backup"), nil) {
		t.Error("nil options asked to check")
	}
	// And the whole entry point must be safe to call with nothing.
	offerUpdateAfterCommand(nil, nil)
}

// The throttle is what keeps a nudge from becoming a nag.
func TestUpdateCheckThrottle(t *testing.T) {
	t.Setenv(config.EnvDataDir, t.TempDir())

	// A check an hour ago is too recent.
	if err := config.SaveSettings(config.Settings{
		LastUpdateCheck: time.Now().Add(-time.Hour).Unix(),
	}); err != nil {
		t.Fatal(err)
	}
	if due := updateCheckDue(); due {
		t.Error("a check an hour old counted as due")
	}

	// One from two days ago is not.
	if err := config.SaveSettings(config.Settings{
		LastUpdateCheck: time.Now().Add(-48 * time.Hour).Unix(),
	}); err != nil {
		t.Fatal(err)
	}
	if due := updateCheckDue(); !due {
		t.Error("a two-day-old check did not come due")
	}

	// A fresh install has never asked, and should.
	if err := config.SaveSettings(config.Settings{}); err != nil {
		t.Fatal(err)
	}
	if due := updateCheckDue(); !due {
		t.Error("a fresh install did not check")
	}
}

// Recording the check must not disturb the other settings in the same file.
func TestRecordingTheCheckKeepsOtherSettings(t *testing.T) {
	t.Setenv(config.EnvDataDir, t.TempDir())
	if err := config.SaveSettings(config.Settings{
		MachineName: "laptop",
		MirrorAuto:  true,
	}); err != nil {
		t.Fatal(err)
	}

	s, err := config.LoadSettings()
	if err != nil {
		t.Fatal(err)
	}
	s.LastUpdateCheck = time.Now().Unix()
	if err := config.SaveSettings(s); err != nil {
		t.Fatal(err)
	}

	got, err := config.LoadSettings()
	if err != nil {
		t.Fatal(err)
	}
	if got.MachineName != "laptop" || !got.MirrorAuto {
		t.Errorf("the update check lost other settings: %+v", got)
	}
	if got.LastUpdateCheck == 0 {
		t.Error("the check was not recorded")
	}
}
