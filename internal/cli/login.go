package cli

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/saurabhhbansal/r2backup/internal/account"
	"github.com/saurabhhbansal/r2backup/internal/creds"
	"github.com/spf13/cobra"
	"golang.org/x/term"
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

func readLine(prompt string, out *Options) (string, error) {
	fmt.Fprint(out.Out, prompt)
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	return strings.TrimSpace(line), err
}

func readPassphrase(prompt string, out *Options) (string, error) {
	fmt.Fprint(out.Out, prompt)
	b, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(out.Out)
	return strings.TrimSpace(string(b)), err
}

func newLoginCmd(opts *Options) *cobra.Command {
	return &cobra.Command{
		Use:   "login <email>",
		Short: "Get this machine your R2 credentials",
		Long: "Sends a six-digit code to your email address, then pulls the R2\n" +
			"credentials you saved from another computer. The server stores them\n" +
			"encrypted and cannot read them.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !interactive() {
				return errors.New("login needs a terminal")
			}
			email := strings.ToLower(strings.TrimSpace(args[0]))
			c := accountClient()

			if err := c.RequestCode(cmd.Context(), email); err != nil {
				return err
			}
			fmt.Fprintf(opts.Out, "A six-digit code is on its way to %s. It expires in 10 minutes.\n", email)

			code, err := readLine("Code: ", opts)
			if err != nil {
				return err
			}
			token, err := c.Verify(cmd.Context(), email, code)
			if err != nil {
				if errors.Is(err, account.ErrInvalidCode) {
					return errors.New("that code is not right, or it has expired. Run login again for a new one")
				}
				return err
			}
			if err := account.SaveToken(token); err != nil {
				return err
			}

			vault, err := c.GetVault(cmd.Context(), token)
			if err != nil {
				if errors.Is(err, account.ErrNotFound) {
					fmt.Fprintln(opts.Out, "Signed in, but no credentials are stored for this account yet.")
					fmt.Fprintln(opts.Out, "On the computer that already works, run: r2backup account push")
					return nil
				}
				return err
			}

			pass, err := readPassphrase("Passphrase: ", opts)
			if err != nil {
				return err
			}
			plain, err := account.Decrypt(pass, vault)
			if err != nil {
				return errors.New("that passphrase does not open the vault.\n" +
					"  Nothing is lost if you have forgotten it: it protects the stored\n" +
					"  credentials, not your files. Run `r2backup setup` with your R2 keys instead")
			}
			var c2 creds.Credentials
			if err := json.Unmarshal(plain, &c2); err != nil {
				return fmt.Errorf("the vault did not contain credentials: %w", err)
			}

			a, err := openApp()
			if err != nil {
				return err
			}
			defer a.close()
			if err := a.creds.Save(c2); err != nil {
				return err
			}
			_ = c.RegisterDevice(cmd.Context(), token, machineName(), runtime.GOOS)

			fmt.Fprintf(opts.Out, "\nDone. This machine can now reach bucket %q.\n", c2.Bucket)
			fmt.Fprintln(opts.Out, "Next: r2backup add <folder>, or r2backup restore <set> --to <directory>")
			return nil
		},
	}
}

func newAccountCmd(opts *Options) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "account",
		Short: "Sign in so other computers can pick up your credentials",
	}

	cmd.AddCommand(&cobra.Command{
		Use:   "push",
		Short: "Encrypt this machine's R2 credentials and store them for other machines",
		Long: "The credentials are encrypted here, with a key derived from a passphrase\n" +
			"you choose. The server keeps a blob it cannot read.\n\n" +
			"Forgetting the passphrase is not a data-loss event: it guards the stored\n" +
			"credentials, not your files, which are never client-side encrypted. The\n" +
			"worst case is re-entering your R2 keys.",
		RunE: func(cmd *cobra.Command, args []string) error {
			if !interactive() {
				return errors.New("this needs a terminal")
			}
			a, err := openApp()
			if err != nil {
				return err
			}
			defer a.close()
			c2, err := a.creds.Load()
			if err != nil {
				return err
			}
			token, err := account.LoadToken()
			if err != nil {
				return errors.New("not signed in on this machine. Run: r2backup login <email>")
			}

			pass, err := readPassphrase("Choose a passphrase: ", opts)
			if err != nil {
				return err
			}
			again, err := readPassphrase("Again: ", opts)
			if err != nil {
				return err
			}
			if pass != again {
				return errors.New("those did not match")
			}
			if len(pass) < 8 {
				return errors.New("use at least 8 characters")
			}

			plain, err := json.Marshal(c2)
			if err != nil {
				return err
			}
			vault, err := account.Encrypt(pass, plain)
			if err != nil {
				return err
			}
			if err := accountClient().PutVault(cmd.Context(), token, vault); err != nil {
				return err
			}
			fmt.Fprintln(opts.Out, "Stored. On another computer: r2backup login <your email>")
			return nil
		},
	})

	cmd.AddCommand(&cobra.Command{
		Use:   "devices",
		Short: "Which computers have signed in",
		RunE: func(cmd *cobra.Command, args []string) error {
			token, err := account.LoadToken()
			if err != nil {
				return errors.New("not signed in on this machine. Run: r2backup login <email>")
			}
			devices, err := accountClient().ListDevices(cmd.Context(), token)
			if err != nil {
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
		Short: "Forget the sign-in on this machine",
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
