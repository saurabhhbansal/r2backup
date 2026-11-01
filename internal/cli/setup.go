package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/saurabhhbansal/r2backup/internal/account"
	"github.com/saurabhhbansal/r2backup/internal/creds"
	"github.com/saurabhhbansal/r2backup/internal/drime"
	"github.com/saurabhhbansal/r2backup/internal/progress"
	"github.com/saurabhhbansal/r2backup/internal/remote"
)

// passwordAttempts is how many times a wrong vault password is forgiven
// before setup gives up. A typo should cost a keystroke, not a re-run of the
// whole command -- the same reason `relink` asks rather than telling you to
// come back with a different one.
const passwordAttempts = 3

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
	var provider string
	cmd := &cobra.Command{
		Use:   "setup",
		Short: "Get this computer ready to back up",
		Long: "Use r2b on this computer alone, or sign in with an email, the\n" +
			"six-digit code sent to it and your PIN. Signed in, it either picks up\n" +
			"the storage keys you saved from another computer or takes them now\n" +
			"and saves them for the next one.\n\n" +
			"The saved copy is encrypted here, with a key made from your PIN. The\n" +
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
			if provider != "" {
				if _, ok := remote.ParseProvider(provider); !ok {
					return fmt.Errorf("unknown provider %q: use drime, r2, b2, s3 or s3-custom", provider)
				}
			}
			return runSetup(cmd.Context(), a, opts, keysOnly, provider)
		},
	}
	cmd.Flags().BoolVar(&keysOnly, "keys", false,
		"enter storage keys directly, even if a saved copy exists (use after rotating them)")
	cmd.Flags().StringVar(&provider, "provider", "",
		"where backups go: drime, r2, b2, s3 or s3-custom (asked if not given)")
	return cmd
}

func runSetup(ctx context.Context, a *app, opts *Options, keysOnly bool, provider string) error {
	p := newPrompter(opts)
	client := accountClient()

	sess, err := signIn(ctx, p, client)
	if err != nil {
		return err
	}

	// Second computer, the common case: everything needed is already stored.
	//
	// Not when a destination was named, though. The account holds one saved
	// set of credentials, and someone saying --provider drime is choosing
	// where this computer goes, not asking to unlock the R2 keys another
	// computer saved -- for which they may not even remember the password.
	if sess != nil && !keysOnly && provider == "" {
		done, err := pullCredentials(ctx, a, p, client, sess)
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
			return finish(ctx, a, opts, p)
		}
	}

	c, err := askForDestination(ctx, p, provider)
	if err != nil {
		return err
	}
	if err := a.creds.Save(c); err != nil {
		return err
	}
	fmt.Fprintln(p.out, "\nSaved.")
	printProtection(p, a)

	// The connection is checked before the keys are stored for other
	// computers, never after. Pushing a set of keys that do not work would
	// hand every later machine the same broken setup, and the failure would
	// surface there rather than here, where it can still be retyped.
	if err := checkConnection(ctx, a, p); err != nil {
		return err
	}

	if sess != nil {
		if err := offerToStore(ctx, a, p, client, sess, c); err != nil {
			return err
		}
	}
	return finish(ctx, a, opts, p)
}

// signIn returns a usable session, asking for an email address, the code
// sent to it and the PIN if this computer is not signed in already. A nil
// session means the person chose to use r2backup without an account -- a
// supported answer, and the first one offered, not an error.
func signIn(ctx context.Context, p *prompter, client *account.Client) (*account.Session, error) {
	// Already signed in here: carry on without asking, which skips the
	// round trip through email entirely on a re-run, which is what `setup
	// --keys` after rotating keys is.
	if tok, err := account.LoadToken(); err == nil && tok != "" {
		ok, err := tokenWorks(ctx, client, tok)
		switch {
		case errors.Is(err, account.ErrPINRequired):
			fmt.Fprintln(p.out, "Your account now has a PIN after the email code. Choose one to carry on.")
			pin, err := askNewPIN(p)
			if err != nil {
				return nil, err
			}
			s, err := client.SetupPIN(ctx, tok, pin)
			if err != nil {
				return nil, pinStepErr(err)
			}
			if err := finishSignIn(ctx, s); err != nil {
				return nil, err
			}
			return s, nil
		case err != nil:
			return nil, friendlyAccountErr(err)
		case ok:
			if key, err := account.LoadSealKey(); err == nil {
				fmt.Fprintln(p.out, "Already signed in on this computer.")
				return &account.Session{Token: tok, SealKey: key}, nil
			}
		}
		// Expired, revoked, or missing its key. Say so and sign in again
		// rather than failing with an error the user cannot act on.
		fmt.Fprintln(p.out, "Your sign-in has expired.")
		_ = account.ClearToken()
	}

	fmt.Fprintln(p.out, "You can use r2backup on this computer alone: your storage keys stay")
	fmt.Fprintln(p.out, "here, and `r2b keys export` moves them to another computer.")
	fmt.Fprintln(p.out, "Or sign in with an email address (hosted, for personal use), and your")
	fmt.Fprintln(p.out, "other computers pick the keys up after an email code and your PIN.")
	fmt.Fprintln(p.out)
	email, err := p.ask("Email address (or press enter to use without an account)")
	if err != nil {
		return nil, err
	}
	email = strings.ToLower(email)
	if email == "" {
		_ = setAccountMode(accountModeLocal)
		fmt.Fprintln(p.out, "\nNo account. This computer's keys stay on this computer.")
		return nil, nil
	}

	if err := client.RequestCode(ctx, email); err != nil {
		return nil, err
	}
	fmt.Fprintf(p.out, "\nA six-digit code is on its way to %s. It expires in 10 minutes.\n", email)

	var ch *account.PINChallenge
	for attempt := 1; ; attempt++ {
		code, err := p.askRequired("Code")
		if err != nil {
			return nil, err
		}
		ch, err = client.Verify(ctx, email, code)
		if err == nil {
			break
		}
		if !errors.Is(err, account.ErrInvalidCode) {
			return nil, friendlyAccountErr(err)
		}
		if attempt >= passwordAttempts {
			return nil, errors.New("that code is not right, or it has expired. Run setup again for a new one")
		}
		fmt.Fprintln(p.out, "  That code is not right. Check the email and try again.")
	}

	var s *account.Session
	if !ch.PINSet {
		fmt.Fprintln(p.out, "\nChoose a six-digit PIN. You enter it after the email code on every")
		fmt.Fprintln(p.out, "computer, and it locks the keys your account keeps: the server cannot")
		fmt.Fprintln(p.out, "read them.")
		pin, err := askNewPIN(p)
		if err != nil {
			return nil, err
		}
		if s, err = client.SetupPIN(ctx, ch.PINToken, pin); err != nil {
			return nil, pinStepErr(err)
		}
	} else {
		for {
			pin, err := p.secret("PIN")
			if err != nil {
				return nil, err
			}
			s, err = client.VerifyPIN(ctx, ch, pin)
			if err == nil {
				break
			}
			var wrong *account.WrongPINError
			if errors.As(err, &wrong) || account.ValidPIN(pin) != nil {
				fmt.Fprintf(p.out, "  %s.\n", err.Error())
				continue
			}
			return nil, pinStepErr(err)
		}
	}
	if err := finishSignIn(ctx, s); err != nil {
		return nil, err
	}
	_ = setAccountMode(accountModeAccount)
	return s, nil
}

// askNewPIN asks for a new PIN twice.
func askNewPIN(p *prompter) (string, error) {
	for {
		first, err := p.secret("PIN (6 digits)")
		if err != nil {
			return "", err
		}
		if err := account.ValidPIN(first); err != nil {
			fmt.Fprintf(p.out, "  %s.\n", err.Error())
			continue
		}
		again, err := p.secret("Again")
		if err != nil {
			return "", err
		}
		if first != again {
			fmt.Fprintln(p.out, "  Those did not match. Try again.")
			continue
		}
		return first, nil
	}
}

// tokenWorks reports whether a cached token is still accepted.
// ErrPINRequired is returned as is: a session from before PINs works once it
// has one.
func tokenWorks(ctx context.Context, client *account.Client, token string) (bool, error) {
	_, err := client.ListDevices(ctx, token)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, account.ErrUnauthorized):
		return false, nil
	default:
		return false, err
	}
}

// pullCredentials fetches the keys the account keeps. It reports false, nil
// when there is nothing stored yet -- the first computer on an account --
// which is a normal path, not a failure.
func pullCredentials(ctx context.Context, a *app, p *prompter, client *account.Client, s *account.Session) (bool, error) {
	k, err := client.GetKeys(ctx, s.Token, s.SealKey)
	switch {
	case errors.Is(err, account.ErrNotFound), err == nil && k.Primary == nil:
		fmt.Fprintln(p.out, "\nThis is the first computer on this account, so I need your storage")
		fmt.Fprintln(p.out, "keys once. Every computer after this one picks them up automatically.")
		return false, nil
	case errors.Is(err, account.ErrDecryptFailed):
		fmt.Fprintln(p.out, "\nThe keys saved to this account were locked with an earlier PIN, so I")
		fmt.Fprintln(p.out, "need them once more.")
		return false, nil
	case err != nil:
		return false, err
	}
	if err := a.creds.Save(*k.Primary); err != nil {
		return false, err
	}
	if k.Second != nil && !a.mirror.Exists() {
		_ = a.mirror.Save(*k.Second)
	}
	if k.Source != nil && !a.source.Exists() {
		_ = a.source.Save(*k.Source)
	}
	fmt.Fprintf(p.out, "\nFound the keys you saved from another computer: %s.\n", destLabel(*k.Primary))
	// Keys pulled from the account land in the same store, under the same
	// OS protection, as keys typed in by hand -- so the README's promise that
	// setup names the protection has to hold here too.
	printProtection(p, a)
	return true, nil
}

// printProtection names whatever is actually guarding the secret that was
// just written to this machine's credential store -- an OS keystore where
// one exists, or, honestly, file permissions where it does not. It runs
// after every write, whichever path produced it, because the promise in the
// README ("setup tells you which of the two you got") does not say which of
// the two ways into setup it applies to.
func printProtection(p *prompter, a *app) {
	name, protected := a.creds.Protection()
	fmt.Fprintf(p.out, "The secret key is guarded by %s.\n", name)
	if !protected {
		fmt.Fprintln(p.out, "Note: no OS keystore is available here, so this is file permissions only.")
	}
}

// offerToStore saves c to the account for other computers -- but asks first
// when the account already holds keys. There is room for one set, and
// replacing what another computer saved would leave every computer set up
// after this one pointing somewhere different from the ones before it.
func offerToStore(ctx context.Context, a *app, p *prompter, client *account.Client, s *account.Session, c creds.Credentials) error {
	existing, err := client.GetKeys(ctx, s.Token, s.SealKey)
	switch {
	case errors.Is(err, account.ErrNotFound), errors.Is(err, account.ErrDecryptFailed),
		err == nil && existing.Primary == nil:
	case err != nil:
		return friendlyAccountErr(err)
	default:
		fmt.Fprintln(p.out, "\nYour account already has keys saved by another computer.")
		fmt.Fprintf(p.out, "Replacing them with these (%s) changes what new computers pick up.\n", destLabel(c))
		replace, err := confirm(p, "Replace them? [y/N]", false)
		if err != nil {
			return err
		}
		if !replace {
			fmt.Fprintln(p.out, "Kept what was saved. This computer uses the new ones.")
			return nil
		}
	}
	k := localStoredKeys(a)
	k.Primary = &c
	if err := client.PutKeys(ctx, s.Token, s.SealKey, k); err != nil {
		return friendlyAccountErr(err)
	}
	fmt.Fprintln(p.out, "\nSaved to your account, locked with your PIN. On your next computer, run")
	fmt.Fprintln(p.out, "`r2b setup` with the same email address and PIN.")
	return nil
}

// askForKeys collects the four values that make an R2 connection.
//
// It offers the browser sign-in first, which answers two of the four -- the
// account ID and the bucket -- and can make the bucket if there is not one
// yet. Whatever that does or does not manage, the remaining questions are
// asked here, so a declined sign-in, a machine with no browser and a
// Cloudflare outage all land in the same place: the four prompts this
// function has always had.
//
// The access key and secret are asked for either way. Cloudflare offers no
// OAuth scope that can mint an API token, by design, so nothing that happens
// in a browser can produce them -- see discoverViaCloudflare.
func askForKeys(ctx context.Context, p *prompter) (creds.Credentials, error) {
	var c creds.Credentials
	if found, ok := discoverViaCloudflare(ctx, p); ok {
		c.AccountID = found.AccountID
		c.Bucket = found.Bucket
	}

	fmt.Fprintln(p.out)
	if c.AccountID == "" {
		v, err := p.askRequired("Cloudflare account ID")
		if err != nil {
			return c, err
		}
		c.AccountID = v
	}

	// The link matters more than it looks. "R2 -> Manage API tokens" is a
	// page people have to go and find, and with the account already known
	// this is the exact page, one click away.
	fmt.Fprintln(p.out, "Now an R2 API token. Make one here:")
	fmt.Fprintf(p.out, "  %s\n", tokenPageURL(c.AccountID))
	fmt.Fprintln(p.out)

	accessKey, err := p.askRequired("R2 access key ID")
	if err != nil {
		return c, err
	}
	c.AccessKeyID = accessKey

	secret, err := p.secret("R2 secret access key")
	if err != nil {
		return c, err
	}
	c.SecretAccessKey = secret

	if c.Bucket == "" {
		v, err := p.askRequired("R2 bucket name")
		if err != nil {
			return c, err
		}
		c.Bucket = v
	}
	return c, nil
}

// askForDestination asks where backups should go, then for what that place
// needs, then whether to bundle small files there.
func askForDestination(ctx context.Context, p *prompter, provider string) (creds.Credentials, error) {
	if provider == "" {
		fmt.Fprintln(p.out)
		fmt.Fprintln(p.out, "Where should backups go?")
		fmt.Fprintln(p.out, "  1. Drime                  -- needs one API token")
		fmt.Fprintln(p.out, "  2. Cloudflare R2          -- an account, a bucket and a key pair")
		fmt.Fprintln(p.out, "  3. Backblaze B2           -- a bucket, its region and an application key")
		fmt.Fprintln(p.out, "  4. Amazon S3              -- a bucket, its region and an access key")
		fmt.Fprintln(p.out, "  5. Other S3-compatible    -- an endpoint URL, a bucket and a key pair")
		i, err := askIndex(p, "Choose 1 to 5", 5)
		if err != nil {
			return creds.Credentials{}, err
		}
		provider = []string{"drime", "r2", "b2", "s3", "s3-custom"}[i]
	}
	pv, _ := remote.ParseProvider(provider)
	var c creds.Credentials
	var err error
	switch pv {
	case remote.ProviderDrime:
		c, err = askForDrime(p)
	case remote.ProviderR2:
		c, err = askForKeys(ctx, p)
	default:
		c, err = askForS3(p, string(pv))
	}
	if err != nil {
		return c, err
	}
	c.Bundle, err = askBundle(p, c)
	return c, err
}

// askBundle asks whether small files are packed into archives at this
// destination. Yes by default where storage charges by the request.
func askBundle(p *prompter, c creds.Credentials) (bool, error) {
	fmt.Fprintln(p.out)
	if c.IsDrime() {
		fmt.Fprintln(p.out, "Pack small files into zip archives before upload? Fewer, bigger uploads;")
		fmt.Fprintln(p.out, "Drime does not charge by the request, so most people leave this off.")
		return confirm(p, "Pack small files? [y/N]", false)
	}
	fmt.Fprintf(p.out, "%s counts every upload against a monthly allowance. Packing small files\n", c.Label())
	fmt.Fprintln(p.out, "into zip archives first turns thousands of uploads into a handful, and")
	fmt.Fprintln(p.out, "restores, trash and the second copy work the same. Without it, the bucket")
	fmt.Fprintln(p.out, "holds one plain object per file.")
	return confirm(p, "Pack small files? [Y/n]", true)
}

// askForS3 takes the keys for Backblaze B2, Amazon S3 or another
// S3-compatible service.
func askForS3(p *prompter, kind string) (creds.Credentials, error) {
	c := creds.Credentials{Provider: kind}
	fmt.Fprintln(p.out)
	switch kind {
	case creds.ProviderB2:
		fmt.Fprintln(p.out, "In Backblaze: B2 Cloud Storage > Buckets for the bucket and its endpoint")
		fmt.Fprintln(p.out, "(s3.<region>.backblazeb2.com), then Application Keys for a key pair.")
	case creds.ProviderS3:
		fmt.Fprintln(p.out, "In the AWS console: the S3 bucket and its region, and an IAM access key")
		fmt.Fprintln(p.out, "allowed to read, write, list and delete in it.")
	default:
		fmt.Fprintln(p.out, "Any service that speaks S3: MinIO, Wasabi, a NAS. You need its endpoint URL.")
		url, err := p.askRequired("Endpoint URL (https://...)")
		if err != nil {
			return c, err
		}
		c.Endpoint = strings.TrimRight(strings.TrimSpace(url), "/")
	}
	bucket, err := p.askRequired("Bucket name")
	if err != nil {
		return c, err
	}
	c.Bucket = bucket
	switch kind {
	case creds.ProviderB2:
		if c.Region, err = p.askRequired("Region (from the endpoint, us-west-004 say)"); err != nil {
			return c, err
		}
	case creds.ProviderS3:
		if c.Region, err = p.askRequired("Region (eu-west-1, say)"); err != nil {
			return c, err
		}
	default:
		if c.Region, err = p.ask("Region (press enter if it has none)"); err != nil {
			return c, err
		}
	}
	label := "Access key ID"
	if kind == creds.ProviderB2 {
		label = "keyID"
	}
	if c.AccessKeyID, err = p.askRequired(label); err != nil {
		return c, err
	}
	secretLabel := "Secret access key"
	if kind == creds.ProviderB2 {
		secretLabel = "applicationKey"
	}
	if c.SecretAccessKey, err = p.secret(secretLabel); err != nil {
		return c, err
	}
	c.AccessKeyID, c.SecretAccessKey = strings.TrimSpace(c.AccessKeyID), strings.TrimSpace(c.SecretAccessKey)
	c.Region = strings.TrimSpace(c.Region)
	return c, c.Valid()
}

// askForDrime takes a Drime API token. That is the whole of Drime's setup:
// no account id, no bucket, no key pair.
func askForDrime(p *prompter) (creds.Credentials, error) {
	fmt.Fprintln(p.out)
	fmt.Fprintln(p.out, "Make a Drime API token: open Drime, then Settings > Developer > Create token.")
	fmt.Fprintln(p.out, "  https://app.drime.cloud/account-settings")
	fmt.Fprintln(p.out, "Backups go in a folder called \"r2backup\" in your Drime, which you can browse there.")
	fmt.Fprintln(p.out)
	token, err := p.secret("Drime API token")
	if err != nil {
		return creds.Credentials{}, err
	}
	token = strings.TrimSpace(token)
	if token == "" {
		return creds.Credentials{}, errors.New("no token entered")
	}
	return creds.Credentials{Provider: creds.ProviderDrime, DrimeToken: token}, nil
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
	if d, ok := baseStore(a.client).(*drime.Client); ok {
		if u, err := d.SpaceUsage(ctx); err == nil && u.Available > 0 {
			fmt.Fprintf(p.out, "Drime space: %s used of %s.\n", progress.FormatBytes(u.Used), progress.FormatBytes(u.Available))
		}
	}
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
