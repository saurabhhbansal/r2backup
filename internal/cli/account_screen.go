package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/saurabhhbansal/r2backup/internal/account"
)

// The panel's account screen, for the terminal: how this computer keeps its
// keys, where backups go and the second copy, the keys saved to the account,
// and the computers. Every command here calls the same trayBackend or
// dashboard method the panel's request does, so the two cannot disagree.

// backendFor is the panel's backend over an app a command already opened.
// Nothing in it starts a child process or a window; it is the request
// handlers, without the panel.
func backendFor(opts *Options, a *app) *trayBackend {
	return &trayBackend{d: &dashboard{opts: opts, app: a, now: time.Now}}
}

// withBackend opens the app for one command and hands it the panel's backend.
func withBackend(opts *Options, run func(t *trayBackend) error) error {
	a, err := openApp()
	if err != nil {
		return err
	}
	defer a.close()
	return run(backendFor(opts, a))
}

// errNeedsYes is a decision with nobody to ask.
var errNeedsYes = errors.New("this needs an answer: run it in a terminal, or add --yes to go ahead")

// decide resolves one of the panel's confirmations. --yes and --no answer it
// for scripts; otherwise the person is asked, and with nobody there the
// command stops rather than guessing.
func decide(opts *Options, p *prompter, question string, def bool) (bool, error) {
	switch opts.Decision() {
	case AlwaysYes:
		return true, nil
	case AlwaysNo:
		return false, nil
	}
	if opts.In == nil && !interactive() {
		return false, errNeedsYes
	}
	return confirm(p, question, def)
}

// reportKeysSync runs the panel's step after signing in -- pull what this
// computer lacks, push when the account has nothing -- and says what
// happened in the panel's words.
func reportKeysSync(ctx context.Context, t *trayBackend, out io.Writer) error {
	r, err := t.syncKeys(ctx)
	if err != nil {
		return err
	}
	switch {
	case r.Err != "":
		fmt.Fprintln(out, r.Err)
	case r.Pulled && r.Primary != "":
		fmt.Fprintf(out, "Signed in. Backups go to %s.\n", r.Primary)
	case r.Pushed:
		fmt.Fprintln(out, "Signed in. This computer's keys are saved to your account, locked with your PIN.")
	default:
		fmt.Fprintln(out, "Signed in.")
	}
	if r.Pulled && r.Second != "" {
		fmt.Fprintf(out, "Second copy: %s, after every backup.\n", r.Second)
	}
	if !r.Pulled && !r.Pushed && r.Primary != "" && r.Err == "" {
		fmt.Fprintf(out, "This computer's keys stay as they are: backups go to %s.\n", r.Primary)
	}
	if r.Primary == "" {
		fmt.Fprintln(out, "Next, choose where backups go: r2b setup")
	}
	return nil
}

// printAccount is the account screen.
func printAccount(ctx context.Context, t *trayBackend, out io.Writer) error {
	c, err := t.accountConfig(ctx)
	if err != nil {
		return err
	}
	hosted := c.Mode == accountModeAccount || c.SignedIn
	if hosted {
		who := c.Email
		if who == "" {
			who = "Signed in"
		}
		fmt.Fprintf(out, "%s\n  Hosted account, locked with your PIN\n", who)
	} else {
		fmt.Fprintln(out, "No account\n  Keys stay on this computer. Move them with: r2b keys export <file>")
	}
	if c.NeedsPIN {
		fmt.Fprintln(out, "\nYour account now needs a 6-digit PIN. Set one: r2b account login")
	}
	if c.Err != "" {
		fmt.Fprintf(out, "\n%s\n", c.Err)
	}

	fmt.Fprintln(out, "\nUsing r2backup for")
	if c.Purpose == "" {
		fmt.Fprintln(out, "  Not chosen yet. Choose: r2b setup purpose both|s3")
	} else {
		fmt.Fprintf(out, "  %s\n  %s\n  Change: r2b setup purpose %s\n", purposeTitle(c.Purpose), purposeText(c.Purpose),
			map[string]string{purposeBoth: purposeS3, purposeS3: purposeBoth}[c.Purpose])
	}

	fmt.Fprintln(out, "\nWhere backups go")
	if c.Primary == nil {
		fmt.Fprintln(out, "  Not connected. Connect: r2b setup")
	} else {
		printDest(out, c.Primary, "")
	}
	// S3 storage only has no second copy to offer.
	showSecond := c.Purpose != purposeS3 || c.Second != nil
	if showSecond {
		fmt.Fprintln(out, "\nSecond copy")
	}
	switch {
	case !showSecond:
	case c.Second != nil:
		printDest(out, c.Second, " --second-copy")
		fmt.Fprintf(out, "  Copy after every backup   %s\n", onOff(c.Auto))
		fmt.Fprintln(out, "  Copy now: r2b mirror · stop keeping it: r2b mirror off")
	case c.Primary != nil:
		fmt.Fprintln(out, "  None. Keep your backups with a second company too: r2b mirror setup")
	default:
		fmt.Fprintln(out, "  None. Connect where backups go first.")
	}

	if c.Primary != nil && hosted && !c.NeedsPIN {
		if c.Saved {
			fmt.Fprintln(out, "\nKeys saved to your account")
		} else {
			fmt.Fprintln(out, "\nKeys not saved to your account. Save them: r2b account save-keys")
		}
	}

	if hosted {
		fmt.Fprintln(out, "\nComputers")
	} else {
		fmt.Fprintln(out, "\nThis computer")
	}
	devices := c.Devices
	if len(devices) == 0 {
		devices = []deviceJSON{{Name: c.Machine, This: true}}
	}
	for _, d := range devices {
		when := ""
		switch {
		case d.This && hosted:
			when = "This one"
		case !d.This:
			if at, err := time.Parse(time.RFC3339, d.LastSeen); err == nil && at.Year() >= 2000 {
				when = "Seen " + at.Local().Format("2 Jan")
			}
		}
		fmt.Fprintf(out, "  %-28s %s\n", d.Name, when)
	}
	fmt.Fprintln(out, "  Rename this computer: r2b account rename <name>")
	return nil
}

func printDest(out io.Writer, s *storeOut, flag string) {
	fmt.Fprintf(out, "  %s\n  %s\n", s.Label, s.Hint)
	next := "on"
	if s.Bundle {
		next = "off"
	}
	fmt.Fprintf(out, "  Pack small files          %s (r2b pack %s%s)\n", onOff(s.Bundle), next, flag)
}

func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

// accountSubcommands are the account screen's actions: signing in, the PIN,
// saving keys and renaming this computer.
func accountSubcommands(opts *Options) []*cobra.Command {
	login := &cobra.Command{
		Use:   "login",
		Short: "Sign in with your email, the code sent to it and your PIN",
		Long: "Signs this computer in to the hosted account. Keys already on this\n" +
			"computer stay; what it lacks comes from the account, and an account\n" +
			"with nothing saved keeps this computer's, locked with your PIN.\n" +
			"Forgot your PIN? Press enter at the PIN prompt.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if !interactive() && opts.In == nil {
				return errors.New("signing in needs a terminal")
			}
			return withBackend(opts, func(t *trayBackend) error {
				p := newPrompter(opts)
				s, err := signIn(cmd.Context(), p, accountClient())
				if err != nil {
					return err
				}
				if s == nil {
					return nil // chose to use r2backup without an account
				}
				return reportKeysSync(cmd.Context(), t, opts.Out)
			})
		},
	}

	reset := &cobra.Command{
		Use:   "reset-pin",
		Short: "Forgot your PIN? Choose a new one after an email code",
		Long:  "Reset your PIN?\n\n" + forgotPINWarning,
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if !interactive() && opts.In == nil {
				return errors.New("resetting the PIN needs a terminal")
			}
			return withBackend(opts, func(t *trayBackend) error {
				p := newPrompter(opts)
				email, _ := signedInHere(time.Now())
				if email == "" {
					if tok, _ := account.LoadToken(); tok != "" {
						email = account.EmailFromToken(tok)
					}
				}
				if email == "" {
					v, err := p.askRequired("Email address")
					if err != nil {
						return err
					}
					email = strings.ToLower(v)
				}
				s, err := forgotPIN(cmd.Context(), p, accountClient(), email)
				if err != nil || s == nil {
					return err
				}
				// The account has nothing now: this computer's keys, if it
				// has any, go there.
				if localStoredKeys(t.d.app).Primary != nil {
					if err := t.d.PushKeys(cmd.Context()); err != nil {
						fmt.Fprintf(opts.Out, "Could not save this computer's keys to your account: %v\n", err)
						fmt.Fprintln(opts.Out, "Try again with: r2b account save-keys")
						return nil
					}
					fmt.Fprintln(opts.Out, "This computer's keys are saved to your account, locked with the new PIN.")
				}
				return nil
			})
		},
	}

	save := &cobra.Command{
		Use:   "save-keys",
		Short: "Save this computer's storage keys to your account",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return withBackend(opts, func(t *trayBackend) error {
				if err := t.d.PushKeys(cmd.Context()); err != nil {
					if errors.Is(err, account.ErrPINRequired) {
						return errors.New("your account now needs a PIN. Set one: r2b account login")
					}
					return err
				}
				fmt.Fprintln(opts.Out, "Saved to your account.")
				return nil
			})
		},
	}

	rename := &cobra.Command{
		Use:   "rename <new-name>",
		Short: "Rename this computer",
		Long: "Gives this computer a new name. Folders already backed up keep where\n" +
			"they are stored; new folders are filed under the new name. A name\n" +
			"another computer on your account or in your storage uses is refused.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withBackend(opts, func(t *trayBackend) error {
				if err := t.renameMachine(cmd.Context(), args[0]); err != nil {
					return err
				}
				fmt.Fprintf(opts.Out, "Renamed to %s.\n", machineName())
				return nil
			})
		},
	}
	return []*cobra.Command{login, reset, save, rename}
}
