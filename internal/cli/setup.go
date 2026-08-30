package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"runtime"
	"strings"

	"github.com/spf13/cobra"

	"github.com/saurabhhbansal/r2backup/internal/account"
	"github.com/saurabhhbansal/r2backup/internal/creds"
)

// passwordAttempts is how many times a wrong vault password is forgiven
// before setup gives up. A typo should cost a keystroke, not a re-run of the
// whole command -- the same reason `relink` asks rather than telling you to
// come back with a different one.
const passwordAttempts = 3

// minPasswordLen is the floor on the vault password. It guards a blob that
// is already behind an emailed sign-in code, so this is a guard against
// "abc", not an attempt at a password policy.
const minPasswordLen = 8

// newSetupCmd builds `setup`: the one command that gets a computer working.
//
// It used to be three. `setup` took R2 keys, `login <email>` signed in and
// pulled a stored copy of them, and `account push` was what put them there in
// the first place -- and nothing said so until you had already run the wrong
// one. Setting up a second computer meant knowing, in advance and in order,
// that you had to run `account push` back on the first one, then `login` here,
// and that `setup` was the thing you specifically must not run. That is a
// choreography, and this is a tool meant to be set up once and forgotten.
//
// So there is one door. Give it your email address; it works out for itself
// whether this is the first computer on the account or the fifth, and asks
// only for what it does not already have. Leaving the email blank keeps the
// old local-only path, so the account is an offer, not a requirement -- and
// nothing about it is needed to back up or restore a single byte.
func newSetupCmd(opts *Options) *cobra.Command {
	var keysOnly bool
	cmd := &cobra.Command{
		Use:   "setup",
		Short: "Get this computer ready to back up",
		Long: "Asks for your email, sends you a six-digit code, and then either\n" +
			"picks up the R2 keys you saved from another computer or takes them\n" +
			"now and saves them for the next one.\n\n" +
			"The saved copy is encrypted here, with a password you choose. The\n" +
			"server keeps a blob it cannot read.",
		RunE: func(cmd *cobra.Command, args []string) error {
			if !interactive() && opts.In == nil {
				return errors.New("setup needs a terminal")
			}
			a, err := openApp()
			if err != nil {
				return err
			}
			defer a.close()
			return runSetup(cmd.Context(), a, opts, keysOnly)
		},
	}
	cmd.Flags().BoolVar(&keysOnly, "keys", false,
		"enter R2 keys directly, even if a saved copy exists (use after rotating them)")
	return cmd
}

func runSetup(ctx context.Context, a *app, opts *Options, keysOnly bool) error {
	p := newPrompter(opts)
	client := accountClient()

	token, err := signIn(ctx, p, client)
	if err != nil {
		return err
	}

	// Second computer, the common case: everything needed is already stored.
	if token != "" && !keysOnly {
		done, err := pullCredentials(ctx, a, p, client, token)
		if err != nil {
			return friendlyAccountErr(err)
		}
		if done {
			// Checked here too, not only on the path that typed them in.
			// Credentials that came out of the vault can still be wrong --
			// they are whatever the first computer had when it stored them,
			// and an R2 token revoked or rotated since then leaves a blob
			// that decrypts perfectly and opens nothing. Without this, setup
			// ended by telling the user this computer "can now reach" a
			// bucket it had never once contacted, and the truth arrived at
			// their first backup instead.
			if err := checkConnection(ctx, a, p); err != nil {
				return err
			}
			_ = client.RegisterDevice(ctx, token, machineName(), runtime.GOOS)
			return finish(ctx, a, opts, p)
		}
	}

	c, err := askForKeys(p)
	if err != nil {
		return err
	}
	if err := a.creds.Save(c); err != nil {
		return err
	}
	name, protected := a.creds.Protection()
	fmt.Fprintf(p.out, "\nSaved. The secret key is guarded by %s.\n", name)
	if !protected {
		fmt.Fprintln(p.out, "Note: no OS keystore is available here, so this is file permissions only.")
	}

	// The connection is checked before the keys are stored for other
	// computers, never after. Pushing a set of keys that do not work would
	// hand every later machine the same broken setup, and the failure would
	// surface there rather than here, where it can still be retyped.
	if err := checkConnection(ctx, a, p); err != nil {
		return err
	}

	if token != "" {
		if err := pushCredentials(ctx, p, client, token, c); err != nil {
			return err
		}
		_ = client.RegisterDevice(ctx, token, machineName(), runtime.GOOS)
	}
	return finish(ctx, a, opts, p)
}

// signIn returns a usable account token, asking for an email address and a
// code if there isn't one cached already. An empty return means the user
// declined an account, which is a supported answer and not an error.
func signIn(ctx context.Context, p *prompter, client *account.Client) (string, error) {
	// A token this machine already holds is worth trying first: it saves the
	// round trip through email entirely on a re-run, which is what `setup
	// --keys` after rotating R2 keys is.
	if tok, err := account.LoadToken(); err == nil && tok != "" {
		if ok, err := tokenWorks(ctx, client, tok); err != nil {
			return "", friendlyAccountErr(err)
		} else if ok {
			fmt.Fprintln(p.out, "Already signed in on this computer.")
			return tok, nil
		}
		// Expired or revoked. Say so and sign in again rather than failing
		// with an authorization error the user cannot act on.
		fmt.Fprintln(p.out, "Your sign-in has expired.")
		_ = account.ClearToken()
	}

	fmt.Fprintln(p.out, "An account lets your other computers pick up these credentials")
	fmt.Fprintln(p.out, "without you typing them again. It is optional.")
	fmt.Fprintln(p.out)
	email, err := p.ask("Email address (or press enter to skip)")
	if err != nil {
		return "", err
	}
	email = strings.ToLower(email)
	if email == "" {
		fmt.Fprintln(p.out, "\nNo account. This computer's credentials stay on this computer.")
		return "", nil
	}

	if err := client.RequestCode(ctx, email); err != nil {
		return "", err
	}
	fmt.Fprintf(p.out, "\nA six-digit code is on its way to %s. It expires in 10 minutes.\n", email)

	for attempt := 1; ; attempt++ {
		code, err := p.askRequired("Code")
		if err != nil {
			return "", err
		}
		token, err := client.Verify(ctx, email, code)
		if err == nil {
			if err := account.SaveToken(token); err != nil {
				return "", err
			}
			return token, nil
		}
		if !errors.Is(err, account.ErrInvalidCode) {
			return "", err
		}
		if attempt >= passwordAttempts {
			return "", errors.New("that code is not right, or it has expired. Run setup again for a new one")
		}
		fmt.Fprintln(p.out, "  That code is not right. Check the email and try again.")
	}
}

// tokenWorks reports whether a cached token is still accepted. A vault that
// does not exist yet is a fine answer -- it means the token is good and the
// account is empty, which is exactly the first-computer case.
func tokenWorks(ctx context.Context, client *account.Client, token string) (bool, error) {
	_, err := client.GetVault(ctx, token)
	switch {
	case err == nil, errors.Is(err, account.ErrNotFound):
		return true, nil
	case errors.Is(err, account.ErrUnauthorized):
		return false, nil
	default:
		return false, err
	}
}

// pullCredentials fetches and unlocks the stored credentials. It reports
// false, nil when there is nothing stored yet -- the first computer on an
// account -- which is a normal path, not a failure.
func pullCredentials(ctx context.Context, a *app, p *prompter, client *account.Client, token string) (bool, error) {
	vault, err := client.GetVault(ctx, token)
	if errors.Is(err, account.ErrNotFound) {
		fmt.Fprintln(p.out, "\nThis is the first computer on this account, so I need your")
		fmt.Fprintln(p.out, "Cloudflare R2 keys once. Every computer after this one picks")
		fmt.Fprintln(p.out, "them up automatically.")
		return false, nil
	}
	if err != nil {
		return false, err
	}

	fmt.Fprintln(p.out, "\nFound the credentials you saved from another computer.")
	for attempt := 1; ; attempt++ {
		pass, err := p.secret("Password (the one you chose there)")
		if err != nil {
			return false, err
		}
		plain, derr := account.Decrypt(pass, vault)
		if derr == nil {
			var c creds.Credentials
			if err := json.Unmarshal(plain, &c); err != nil {
				return false, fmt.Errorf("the saved credentials could not be read: %w", err)
			}
			if err := a.creds.Save(c); err != nil {
				return false, err
			}
			fmt.Fprintf(p.out, "\nUnlocked: bucket %q.\n", c.Bucket)
			return true, nil
		}
		if attempt >= passwordAttempts {
			// Worth spelling out, because "wrong password" on a backup tool
			// reads like lost data and is not: the password guards the stored
			// copy of the keys, not one byte of anyone's files.
			return false, errors.New("that password does not open the saved credentials.\n" +
				"  Nothing is lost if you have forgotten it -- it protects the stored keys,\n" +
				"  not your files. Run `r2b setup --keys` and enter your R2 keys instead")
		}
		fmt.Fprintln(p.out, "  That password does not open it. Try again.")
	}
}

// pushCredentials encrypts the credentials here and stores the result, so the
// next computer needs only the email address.
func pushCredentials(ctx context.Context, p *prompter, client *account.Client, token string, c creds.Credentials) error {
	fmt.Fprintln(p.out, "\nNow choose a password. Your other computers will ask for it, and")
	fmt.Fprintln(p.out, "it is what keeps the stored copy unreadable to the server.")

	var pass string
	for {
		first, err := p.secret("Password")
		if err != nil {
			return err
		}
		if len(first) < minPasswordLen {
			fmt.Fprintf(p.out, "  Use at least %d characters.\n", minPasswordLen)
			continue
		}
		again, err := p.secret("Again")
		if err != nil {
			return err
		}
		if first != again {
			fmt.Fprintln(p.out, "  Those did not match. Try again.")
			continue
		}
		pass = first
		break
	}

	plain, err := json.Marshal(c)
	if err != nil {
		return err
	}
	vault, err := account.Encrypt(pass, plain)
	if err != nil {
		return err
	}
	if err := client.PutVault(ctx, token, vault); err != nil {
		return err
	}
	fmt.Fprintln(p.out, "\nStored. On your next computer, run `r2b setup` and give it the")
	fmt.Fprintln(p.out, "same email address -- it will pick these up by itself.")
	return nil
}

// askForKeys collects the four values that make an R2 connection.
func askForKeys(p *prompter) (creds.Credentials, error) {
	var c creds.Credentials
	fmt.Fprintln(p.out)
	for _, f := range []struct {
		label string
		into  *string
	}{
		{"Cloudflare account ID", &c.AccountID},
		{"R2 access key ID", &c.AccessKeyID},
		{"R2 bucket name", &c.Bucket},
	} {
		v, err := p.askRequired(f.label)
		if err != nil {
			return c, err
		}
		*f.into = v
	}
	secret, err := p.secret("R2 secret access key")
	if err != nil {
		return c, err
	}
	c.SecretAccessKey = secret
	return c, nil
}

// checkConnection proves the credentials work before anything is built on
// them, so a typo is caught while it is still on screen.
func checkConnection(ctx context.Context, a *app, p *prompter) error {
	fmt.Fprintln(p.out, "Checking the connection...")
	if err := a.connect(ctx); err != nil {
		return err
	}
	if _, err := a.client.List(ctx, "r2backup/"); err != nil {
		return fmt.Errorf("credentials saved, but the bucket could not be read: %w", err)
	}
	fmt.Fprintln(p.out, "Connection works.")
	return nil
}

// finish points at whichever step actually comes next. A computer that has
// backups on the account is here to restore them; a fresh one is here to add
// a folder. Guessing wrong turns the last line of setup into a dead end.
func finish(ctx context.Context, a *app, opts *Options, p *prompter) error {
	fmt.Fprintln(p.out)
	if names, err := existingBackups(ctx, a); err == nil && len(names) > 0 {
		fmt.Fprintf(p.out, "There are already backups here: %s\n", strings.Join(names, ", "))
		fmt.Fprintf(p.out, "To bring one back: r2b restore %s --to <folder>\n", names[0])
		fmt.Fprintln(p.out, "To back up a folder on this computer: r2b add <folder>")
		return nil
	}
	fmt.Fprintln(p.out, "Next: r2b add <folder>")
	return nil
}
