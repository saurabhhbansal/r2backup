package cli

import (
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/saurabhhbansal/r2backup/internal/account"
)

// AccountAPI is where the account service lives. It is compiled in rather than
// configured, so a user never types it and never has to know it exists.
const AccountAPI = "https://r2backup.flexpod.cc"

// EnvAccountAPI points the client somewhere else. Tests use it; users do not.
const EnvAccountAPI = "R2BACKUP_ACCOUNT_API"

func accountClient() *account.Client {
	base := AccountAPI
	if v := os.Getenv(EnvAccountAPI); v != "" {
		base = v
	}
	return account.NewClient(base, nil)
}

// newAccountCmd builds `account`, which is now only the two things a person
// might want to look at or undo.
//
// `account push` used to live here, and it was the step that made the whole
// feature not work: the credentials only reached the second computer if you
// had known to run it on the first one, before you needed them. `setup` does
// it as part of signing in, so there is nothing left to remember.
func newAccountCmd(opts *Options) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "account",
		Short: "The computers signed in to your account",
	}

	// requireToken reads the cached sign-in. LoadToken reports "nobody has
	// signed in here" as ("", nil), not as an error, so checking only err
	// sent an empty bearer token to the server and turned "you are not
	// signed in" into a 401 from the network.
	requireToken := func() (string, error) {
		tok, err := account.LoadToken()
		if err != nil {
			return "", err
		}
		if tok == "" {
			return "", errors.New("not signed in on this computer. Run: r2b setup")
		}
		return tok, nil
	}

	cmd.AddCommand(&cobra.Command{
		Use:   "devices",
		Short: "Which computers have signed in",
		RunE: func(cmd *cobra.Command, args []string) error {
			token, err := requireToken()
			if err != nil {
				return err
			}
			devices, err := accountClient().ListDevices(cmd.Context(), token)
			if err != nil {
				if errors.Is(err, account.ErrUnauthorized) {
					return errors.New("your sign-in has expired. Run: r2b setup")
				}
				return err
			}
			if len(devices) == 0 {
				fmt.Fprintln(opts.Out, "No computers signed in yet.")
				return nil
			}
			for _, d := range devices {
				fmt.Fprintf(opts.Out, "%-24s %-10s last seen %s\n",
					d.DeviceName, d.OS, time.UnixMilli(d.LastSeen).Format("2 Jan 15:04"))
			}
			return nil
		},
	})

	cmd.AddCommand(&cobra.Command{
		Use:   "logout",
		Short: "Forget the sign-in on this computer",
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := account.ClearToken(); err != nil {
				return err
			}
			fmt.Fprintln(opts.Out, "Signed out. Your R2 credentials and backups are untouched.")
			return nil
		},
	})
	return cmd
}
