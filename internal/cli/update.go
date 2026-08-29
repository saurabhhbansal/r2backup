package cli

import (
	"fmt"

	"github.com/saurabhhbansal/r2backup/internal/selfupdate"
	"github.com/spf13/cobra"
)

// Repo is where releases are published.
const Repo = "saurabhhbansal/r2backup"

func newUpdateCmd(opts *Options) *cobra.Command {
	var check bool
	cmd := &cobra.Command{
		Use:   "update",
		Short: "Replace this binary with the latest release",
		Long: "Downloads the newest release, checks it against the published\n" +
			"checksum, and swaps it in. There is no installer and no background\n" +
			"service, so nothing has to be stopped first.",
		RunE: func(cmd *cobra.Command, args []string) error {
			rel, err := selfupdate.Latest(cmd.Context(), Repo)
			if err != nil {
				return err
			}
			if !selfupdate.Newer(Version, rel.Version) {
				fmt.Fprintf(opts.Out, "r2backup %s is already the latest.\n", Version)
				return nil
			}
			fmt.Fprintf(opts.Out, "%s is available (you have %s).\n", rel.Version, Version)
			if check {
				fmt.Fprintln(opts.Out, "Run `r2b update` to install it.")
				return nil
			}

			fmt.Fprintln(opts.Out, "Downloading and verifying...")
			bin, err := selfupdate.Fetch(cmd.Context(), rel)
			if err != nil {
				return err
			}
			if err := selfupdate.Apply(bin, ""); err != nil {
				return err
			}
			fmt.Fprintf(opts.Out, "Updated to %s.\n", rel.Version)
			return nil
		},
	}
	cmd.Flags().BoolVar(&check, "check", false, "only report whether an update exists")
	return cmd
}
