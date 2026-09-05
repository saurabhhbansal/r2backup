package cli

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/saurabhhbansal/r2backup/internal/config"
	"github.com/saurabhhbansal/r2backup/internal/selfupdate"
)

// updateCheckEvery is how long to leave between asking GitHub whether there is
// a newer release.
//
// Once a day. The check is a network round trip on a command someone is
// waiting on, and a release does not appear often enough to be worth paying
// for more than that. It is also the difference between a nudge and a nag:
// nobody wants to decline the same offer six times in an afternoon.
const updateCheckEvery = 24 * time.Hour

// updateCheckTimeout bounds the check.
//
// Short on purpose. This runs after the real work is finished, so every second
// spent here is a second someone waits at a prompt that has already given them
// what they came for. A slow or unreachable GitHub must cost a moment and then
// be forgotten, never hold the command open.
const updateCheckTimeout = 4 * time.Second

// commandsThatSkipTheUpdateCheck are the ones this must never run after.
var commandsThatSkipTheUpdateCheck = map[string]bool{
	// It just did this, and better.
	"update": true,
	// Long-lived and often piped somewhere.
	"status": true,
}

// offerUpdateAfterCommand asks about a newer release once a command has
// finished its actual work.
//
// After, not before, and the ordering is the whole design. Checking first
// would put a network round trip in front of every backup, and swapping the
// binary out from under a run that is about to start is a worse idea than it
// sounds. So the work happens, succeeds, and only then does this ask.
//
// Everything about it is conditional on somebody being there to answer. A
// scheduled 3am run has no terminal, so it never reaches the question; a
// script with --yes never reaches it either, because "answer yes to
// everything" must not silently mean "replace this binary mid-pipeline".
// Failures are swallowed on purpose: nothing here is worth turning a
// successful backup into a non-zero exit.
func offerUpdateAfterCommand(cmd *cobra.Command, opts *Options) {
	if !shouldCheckForUpdate(cmd, opts) {
		return
	}

	// The timestamp is written before the network call, not after. A GitHub
	// that is down or rate-limiting would otherwise leave the check due on
	// every single command, turning one failed request into one per
	// invocation for as long as the outage lasts.
	stamp, err := config.LoadSettings()
	if err != nil {
		return
	}
	stamp.LastUpdateCheck = time.Now().Unix()
	if err := config.SaveSettings(stamp); err != nil {
		return
	}

	ctx, cancel := context.WithTimeout(cmd.Context(), updateCheckTimeout)
	defer cancel()

	rel, err := selfupdate.Latest(ctx, Repo)
	if err != nil || rel == nil || !selfupdate.Newer(Version, rel.Version) {
		return
	}

	fmt.Fprintf(opts.Out, "\nr2backup %s is available (you have %s).\n", rel.Version, Version)
	in := opts.In
	if in == nil {
		in = os.Stdin
	}
	if !askYesNo(opts.Out, in, "Update now?", true) {
		fmt.Fprintln(opts.Out, "Left alone. To do it later: r2b update")
		return
	}

	fmt.Fprintln(opts.Out, "Downloading and verifying...")
	// A fresh context: the download is bigger than the check that found it,
	// and the four seconds that bounds "is there one" would abandon it
	// halfway. Someone who has just said yes is waiting on purpose.
	downloadCtx, downloadCancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer downloadCancel()

	bin, err := selfupdate.Fetch(downloadCtx, rel)
	if err != nil {
		fmt.Fprintf(opts.Err, "Could not download the update: %v\n", err)
		fmt.Fprintln(opts.Out, "Nothing was changed. To try again: r2b update")
		return
	}
	if err := selfupdate.Apply(bin, ""); err != nil {
		fmt.Fprintf(opts.Err, "Could not install the update: %v\n", err)
		fmt.Fprintln(opts.Out, "Nothing was changed. To try again: r2b update")
		return
	}
	fmt.Fprintf(opts.Out, "Updated to %s. It takes effect next time you run r2b.\n", rel.Version)
}

// shouldCheckForUpdate holds every reason not to ask.
func shouldCheckForUpdate(cmd *cobra.Command, opts *Options) bool {
	if cmd == nil || opts == nil {
		return false
	}
	// The root command with no subcommand is the dashboard: a full-screen
	// program that owns the terminal and the keyboard. A prompt printed as
	// it tears down would land in a screen being restored and read from a
	// stdin the TUI was itself draining. The dashboard offers updates from
	// inside instead -- see the Account tab.
	if cmd.Parent() == nil {
		return false
	}
	if commandsThatSkipTheUpdateCheck[cmd.Name()] {
		return false
	}
	// No terminal means nobody is there: a scheduled run, a cron job, a
	// command in a pipeline. Asking would either hang or scribble on
	// somebody's output.
	if !interactive() {
		return false
	}
	// --yes and --no are for unattended use. Neither should be read as
	// consent to replace the binary.
	if opts.Decision() != Ask {
		return false
	}
	// A test driving a conversation through Options.In is not a person, and
	// an unexpected prompt would eat the input meant for something else.
	if opts.In != nil {
		return false
	}
	if os.Getenv(EnvNoUpdateCheck) != "" {
		return false
	}
	return updateCheckDue()
}

// updateCheckDue is the throttle on its own.
//
// Separate from shouldCheckForUpdate because every other condition there
// depends on there being a terminal, which a test process does not have. This
// is the one rule that can be checked directly.
func updateCheckDue() bool {
	s, err := config.LoadSettings()
	if err != nil {
		return false
	}
	if s.LastUpdateCheck == 0 {
		return true
	}
	return time.Since(time.Unix(s.LastUpdateCheck, 0)) >= updateCheckEvery
}

// EnvNoUpdateCheck turns the post-command check off entirely.
//
// For the people who package this, run it in CI, or simply do not want a
// program phoning home on their behalf -- all of whom are entitled to an
// answer better than "edit a settings file".
const EnvNoUpdateCheck = "R2BACKUP_NO_UPDATE_CHECK"
