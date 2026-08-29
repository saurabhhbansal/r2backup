package cli

import (
	"context"
	"errors"

	"github.com/spf13/cobra"

	"github.com/saurabhhbansal/r2backup/internal/ui"
)

// runDashboard opens the interface.
//
// It no longer gates on having credentials. The first version refused to open
// without them and told the user to go and run `r2b setup` -- which is the
// command line again, for the one thing a new user has to do first. Signing
// in and entering R2 keys are both on the Account tab, so an unconfigured
// machine opens the window and is shown where to go.
func runDashboard(ctx context.Context, opts *Options) error {
	if !interactive() {
		return errors.New("r2b with no arguments opens the interface, which needs a terminal.\n" +
			"  For a script or a scheduled run, name a command: r2b backup, r2b status")
	}
	return ui.Run(ctx, &dashboard{opts: opts})
}

// attachDashboard makes the interface the default when no command is named,
// while leaving `r2b --help` and every subcommand exactly as they were.
//
// Nothing was removed to make room for it. Every flag and every command still
// works and still means what it did: the scheduler runs `r2b backup`, scripts
// run whatever they ran, and this is a second front door rather than a
// replacement for the first.
func attachDashboard(root *cobra.Command, opts *Options) {
	root.Args = cobra.NoArgs
	root.RunE = func(cmd *cobra.Command, args []string) error {
		return runDashboard(cmd.Context(), opts)
	}
}
