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
	"github.com/saurabhhbansal/r2backup/internal/ui"
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
	var provider, purpose, second string
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
			f, err := parseSetupFlags(provider, purpose, second)
			if err != nil {
				return err
			}
			f.keysOnly = keysOnly
			return runSetup(cmd.Context(), a, opts, f)
		},
	}
	cmd.Flags().BoolVar(&keysOnly, "keys", false,
		"enter storage keys directly, even if a saved copy exists (use after rotating them)")
	cmd.Flags().StringVar(&provider, "provider", "",
		"where backups go: drime, r2, b2, s3 or s3-custom (asked if not given)")
	cmd.Flags().StringVar(&purpose, "purpose", "",
		"what r2backup is for here: both (Drime plus a second copy on S3 storage) or s3 (S3 storage only); asked if not given")
	cmd.Flags().StringVar(&second, "second", "",
		"with --purpose both, where the second copy goes: r2, b2, s3, s3-custom, or none to add it later (asked if not given)")
	cmd.AddCommand(newPurposeCmd(opts))
	return cmd
}

// setupFlags are what `setup` was told on its command line.
type setupFlags struct {
	keysOnly bool
	provider string // where backups go, "" to ask
	purpose  string // purposeBoth, purposeS3, or "" to ask
	second   string // S3 kind for the second copy, "" to ask
}

// parseSetupFlags checks the flags agree with each other before anything is
// asked: a script that says --purpose s3 --provider drime is a mistake to
// point out, not a question to answer.
func parseSetupFlags(provider, purpose, second string) (setupFlags, error) {
	f := setupFlags{provider: provider, second: second}
	if provider != "" {
		if _, ok := remote.ParseProvider(provider); !ok {
			return f, fmt.Errorf("unknown provider %q: use drime, r2, b2, s3 or s3-custom", provider)
		}
	}
	if second != "" && second != secondNone {
		pv, ok := remote.ParseProvider(second)
		if !ok || pv == remote.ProviderDrime {
			return f, fmt.Errorf("unknown --second %q: use r2, b2, s3, s3-custom or none", second)
		}
	}
	if purpose != "" {
		pp, ok := parsePurpose(purpose)
		if !ok {
			return f, fmt.Errorf("unknown --purpose %q: use both or s3", purpose)
		}
		f.purpose = pp
	}
	switch {
	case f.purpose == purposeS3 && provider == creds.ProviderDrime:
		return f, errors.New("--purpose s3 backs up to S3 storage, not Drime: leave out --provider drime, or use --purpose both")
	case f.purpose == purposeBoth && provider != "" && provider != creds.ProviderDrime:
		return f, errors.New("--purpose both backs up to Drime first: use --provider drime, or leave --provider out")
	case second != "" && f.purpose != purposeBoth:
		return f, errors.New("--second goes with --purpose both: S3 storage only has no second copy")
	}
	return f, nil
}

// Seams for tests, which have no Drime to reach: proving the main
// destination works, the second copy, and looking for backups at the end.
var (
	setupCheck  = checkConnection
	secondCheck = reachable
	setupFinish = finish
)

func runSetup(ctx context.Context, a *app, opts *Options, f setupFlags) error {
	keysOnly, provider := f.keysOnly, f.provider
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
	//
	// A computer that already has keys keeps them, as the panel's sign-in
	// does: its folders are backed up there. What it lacks comes from the
	// account, and an account with nothing saved takes this computer's.
	if sess != nil && !keysOnly && provider == "" && a.creds.Exists() {
		if err := reportKeysSync(ctx, backendFor(opts, a), p.out); err != nil {
			return err
		}
		if err := setupCheck(ctx, a, p); err != nil {
			return err
		}
		if pp := currentPurpose(a); pp != "" {
			fmt.Fprintf(p.out, "Using r2backup for: %s.\n", purposeTitle(pp))
		}
		return setupFinish(ctx, a, opts, p)
	}
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
			if err := setupCheck(ctx, a, p); err != nil {
				return err
			}
			if pp := currentPurpose(a); pp != "" {
				fmt.Fprintf(p.out, "Using r2backup for: %s.\n", purposeTitle(pp))
			}
			return setupFinish(ctx, a, opts, p)
		}
	}

	// Moving where backups go asks first, as the panel's Change… does: what
	// is already there stays there.
	if old, err := a.creds.Load(); err == nil && !keysOnly && provider == "" {
		move, err := decide(opts, p, "\nBack up somewhere else? New backups go to the new place. What’s in "+
			destLabel(old)+" stays.\nContinue? [y/N]", false)
		if err != nil {
			return err
		}
		if !move {
			fmt.Fprintf(p.out, "Backups still go to %s.\n", destLabel(old))
			if err := setupCheck(ctx, a, p); err != nil {
				return err
			}
			return setupFinish(ctx, a, opts, p)
		}
	}

	// What this computer is for decides what is asked next: Drime and then
	// the second copy, or S3 storage alone. --provider on its own keeps the
	// one question it always answered, and setup --keys keeps the purpose
	// already chosen.
	purpose := f.purpose
	if purpose == "" && provider == "" {
		if keysOnly {
			purpose = storedPurpose()
		}
		if purpose == "" {
			var err error
			if purpose, err = askPurpose(p); err != nil {
				return err
			}
		}
	}
	if purpose != "" {
		if err := savePurpose(purpose); err != nil {
			return err
		}
	}

	var c creds.Credentials
	switch purpose {
	case purposeBoth:
		if keysOnly {
			// The panel's Change…: any kind of storage.
			c, err = askForDestination(ctx, p, provider)
			break
		}
		fmt.Fprintln(p.out, "\nFirst Drime, where backups go.")
		c, err = askForKind(ctx, p, creds.ProviderDrime)
	case purposeS3:
		c, err = askForS3Destination(ctx, p, provider, "Where should backups go?")
	default:
		c, err = askForDestination(ctx, p, provider)
	}
	if err != nil {
		return err
	}
	// New keys for other storage move where backups go. That asks first,
	// as every other way of changing it does; new keys for the same storage
	// do not.
	if old, err := a.creds.Load(); err == nil && keysOnly && old.Place() != c.Place() {
		move, err := decide(opts, p, "\nBack up somewhere else? New backups go to "+destLabel(c)+". What’s in "+
			destLabel(old)+" stays.\nContinue? [y/N]", false)
		if err != nil {
			return err
		}
		if !move {
			fmt.Fprintf(p.out, "Backups still go to %s.\n", destLabel(old))
			return setupFinish(ctx, a, opts, p)
		}
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
	if err := setupCheck(ctx, a, p); err != nil {
		return err
	}

	switch purpose {
	case purposeBoth:
		if err := setUpSecondCopy(ctx, a, p, f.second); err != nil {
			return err
		}
	case purposeS3:
		// S3 storage only, on a computer set up before: a Drime second copy
		// left from then stops, as switching in the panel does.
		if mc, err := a.mirror.Load(); err == nil && mc.IsDrime() {
			if err := forgetSecond(a); err != nil {
				return err
			}
			fmt.Fprintf(p.out, "No more copies to %s. What is there stays.\n", destLabel(mc))
		}
	}

	if sess != nil {
		if err := offerToStore(ctx, a, p, client, sess, c); err != nil {
			return err
		}
	}
	return setupFinish(ctx, a, opts, p)
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

	ch, err := askCode(ctx, p, client, email)
	if err != nil {
		return nil, err
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
			pin, err := p.secret("PIN (or press enter if you forgot it)")
			if err != nil {
				return nil, err
			}
			if pin == "" {
				// Forgot it: a reset, after a fresh email code.
				reset, err := forgotPIN(ctx, p, client, email)
				if err != nil {
					return nil, err
				}
				if reset == nil {
					continue // kept the PIN after all
				}
				return reset, nil
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

// askCode asks for the email code, forgiving a typo or two.
func askCode(ctx context.Context, p *prompter, client *account.Client, email string) (*account.PINChallenge, error) {
	fmt.Fprintln(p.out, "No email? Type new for a new code.")
	for attempt := 1; ; attempt++ {
		code, err := p.askRequired("Code")
		if err != nil {
			return nil, err
		}
		// The panel's "Send a new code".
		if strings.EqualFold(strings.TrimSpace(code), "new") {
			if err := client.RequestCode(ctx, email); err != nil {
				return nil, friendlyAccountErr(err)
			}
			fmt.Fprintf(p.out, "  A new code is on its way to %s.\n", email)
			attempt = 0
			continue
		}
		ch, err := client.Verify(ctx, email, strings.ReplaceAll(code, " ", ""))
		if err == nil {
			return ch, nil
		}
		if !errors.Is(err, account.ErrInvalidCode) {
			return nil, friendlyAccountErr(err)
		}
		if attempt >= passwordAttempts {
			return nil, errors.New("that code is not right, or it has expired. Run setup again for a new one")
		}
		fmt.Fprintln(p.out, "  That code is not right. Check the email and try again.")
	}
}

// forgotPINWarning is what the panel's "Reset your PIN?" screen says.
const forgotPINWarning = "This deletes the keys saved to your account. They were locked with the\n" +
	"old PIN, so nobody can open them now. Your other computers are signed out,\n" +
	"too. Backups in your storage are not touched: connect again with your\n" +
	"storage keys or a key file."

// forgotPIN resets a forgotten PIN after a fresh email code, as the panel's
// "Forgot PIN?" does. It returns nil, nil when the person changes their mind.
// The session it returns is saved and this computer registered, but nothing
// is saved to the account: the caller does that.
func forgotPIN(ctx context.Context, p *prompter, client *account.Client, email string) (*account.Session, error) {
	fmt.Fprintln(p.out, "\nReset your PIN?")
	fmt.Fprintln(p.out, forgotPINWarning)
	ok, err := confirm(p, "Email me a code? [y/N]", false)
	if err != nil || !ok {
		return nil, err
	}
	if err := client.RequestCode(ctx, email); err != nil {
		return nil, friendlyAccountErr(err)
	}
	fmt.Fprintf(p.out, "Sent to %s. Then you choose a new PIN.\n", email)
	ch, err := askCode(ctx, p, client, email)
	if err != nil {
		return nil, err
	}
	fmt.Fprintln(p.out, "\nChoose a new PIN. It locks the keys your account keeps.")
	pin, err := askNewPIN(p)
	if err != nil {
		return nil, err
	}
	var s *account.Session
	if !ch.PINSet {
		// Nothing to reset: the account never had one.
		if s, err = client.SetupPIN(ctx, ch.PINToken, pin); err != nil {
			return nil, pinStepErr(err)
		}
		if err := finishSignIn(ctx, s); err != nil {
			return nil, err
		}
	} else if s, err = resetPIN(ctx, nil, ch.PINToken, pin); err != nil {
		return nil, err
	}
	_ = setAccountMode(accountModeAccount)
	fmt.Fprintln(p.out, "New PIN set. Your other computers are signed out.")
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
	adoptPurpose(k.Purpose)
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
	return askForKind(ctx, p, provider)
}

// askForKind asks for what one kind of storage needs, then whether to pack
// small files there.
func askForKind(ctx context.Context, p *prompter, provider string) (creds.Credentials, error) {
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
		label = "Key ID"
	}
	if c.AccessKeyID, err = p.askRequired(label); err != nil {
		return c, err
	}
	secretLabel := "Secret access key"
	if kind == creds.ProviderB2 {
		secretLabel = "Application key"
	}
	if c.SecretAccessKey, err = p.secret(secretLabel); err != nil {
		return c, err
	}
	if kind == creds.ProviderCustom {
		// Path-style unless the service wants the bucket in the hostname,
		// as the panel's "Bucket in the hostname" switch.
		if c.VirtualHost, err = confirm(p, "Bucket in the hostname, for addresses like bucket.s3.example.com? [y/N]", false); err != nil {
			return c, err
		}
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
	fmt.Fprintln(p.out, "Backups go in a folder in your Drime, \"r2backup\" unless you name another, which you can browse there.")
	fmt.Fprintln(p.out)
	token, err := p.secret("Drime API token")
	if err != nil {
		return creds.Credentials{}, err
	}
	token = strings.TrimSpace(token)
	if token == "" {
		return creds.Credentials{}, errors.New("no token entered")
	}
	folder, err := p.ask("Folder in Drime (press enter for r2backup)")
	if err != nil {
		return creds.Credentials{}, err
	}
	// The same tidying the panel's form gets (credsFromKeys).
	return credsFromKeys(ui.Keys{Provider: creds.ProviderDrime, DrimeToken: token, DrimeFolder: folder}), nil
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

// askPurpose asks what this computer is using r2backup for, in the panel's
// words.
func askPurpose(p *prompter) (string, error) {
	fmt.Fprintln(p.out)
	fmt.Fprintln(p.out, purposeQuestion)
	fmt.Fprintf(p.out, "  1. %s\n     %s\n", purposeBothTitle, purposeBothText)
	fmt.Fprintf(p.out, "  2. %s\n     %s\n", purposeS3Title, purposeS3Text)
	i, err := askIndex(p, "Choose 1 or 2", 2)
	if err != nil {
		return "", err
	}
	return []string{purposeBoth, purposeS3}[i], nil
}

// askForS3Destination asks which S3-compatible storage (unless kind says),
// then for its keys and packing.
func askForS3Destination(ctx context.Context, p *prompter, kind, question string) (creds.Credentials, error) {
	if kind == "" {
		fmt.Fprintln(p.out)
		fmt.Fprintln(p.out, question)
		fmt.Fprintln(p.out, "  1. Cloudflare R2          -- an account, a bucket and a key pair")
		fmt.Fprintln(p.out, "  2. Backblaze B2           -- a bucket, its region and an application key")
		fmt.Fprintln(p.out, "  3. Amazon S3              -- a bucket, its region and an access key")
		fmt.Fprintln(p.out, "  4. Other S3-compatible    -- an endpoint URL, a bucket and a key pair")
		i, err := askIndex(p, "Choose 1 to 4", 4)
		if err != nil {
			return creds.Credentials{}, err
		}
		kind = []string{"r2", "b2", "s3", "s3-custom"}[i]
	}
	return askForKind(ctx, p, kind)
}

// secondNone is --second none: no second copy for now.
const secondNone = "none"

// secondSkipped is what setup says when the second copy is left for later.
const secondSkipped = "No second copy for now. Add one with: r2b mirror setup"

// askSecondKind asks where the second copy goes, or "Not now".
func askSecondKind(p *prompter) (string, error) {
	fmt.Fprintln(p.out)
	fmt.Fprintln(p.out, "Where should the second copy go?")
	fmt.Fprintln(p.out, "  1. Cloudflare R2          -- an account, a bucket and a key pair")
	fmt.Fprintln(p.out, "  2. Backblaze B2           -- a bucket, its region and an application key")
	fmt.Fprintln(p.out, "  3. Amazon S3              -- a bucket, its region and an access key")
	fmt.Fprintln(p.out, "  4. Other S3-compatible    -- an endpoint URL, a bucket and a key pair")
	fmt.Fprintln(p.out, "  5. Not now                -- back up to Drime only, add a second copy later")
	i, err := askIndex(p, "Choose 1 to 5", 5)
	if err != nil {
		return "", err
	}
	return []string{"r2", "b2", "s3", "s3-custom", secondNone}[i], nil
}

// setUpSecondCopy asks for the S3 storage the second copy goes to, checks
// it, and copies each backup there after it finishes. A second copy already
// set up (setup --keys after rotating the main keys) is kept.
func setUpSecondCopy(ctx context.Context, a *app, p *prompter, kind string) error {
	if mc, err := a.mirror.Load(); err == nil && kind == "" {
		fmt.Fprintf(p.out, "\nThe second copy stays on %s. To change it: r2b mirror setup\n", destLabel(mc))
		return nil
	}
	main, err := a.creds.Load()
	if err != nil {
		return err
	}
	if kind == "" {
		// The panel's "Not now": Drime alone for the moment.
		if kind, err = askSecondKind(p); err != nil {
			return err
		}
	}
	if kind == secondNone {
		fmt.Fprintln(p.out, secondSkipped)
		return nil
	}
	for attempt := 1; ; attempt++ {
		c, err := askForS3Destination(ctx, p, kind, "Where should the second copy go?")
		if err != nil {
			return err
		}
		if samePlace(main, c) {
			return errors.New("that is where your backups already go. A second copy has to be somewhere else")
		}
		fmt.Fprintln(p.out, "Checking the second copy...")
		err = secondCheck(ctx, c)
		if err == nil {
			if err := a.mirror.Save(c); err != nil {
				return err
			}
			if err := setMirrorAuto(true); err != nil {
				return err
			}
			fmt.Fprintf(p.out, "Second copy: %s. Each backup is copied there once it finishes.\n", destLabel(c))
			return nil
		}
		if attempt >= passwordAttempts {
			// Backups to Drime work without it, so setup still finishes:
			// the Drime keys are stored and the next step is shown.
			fmt.Fprintf(p.out, "The second copy could not be reached: %s.\n%s\n", err.Error(), secondSkipped)
			return nil
		}
		fmt.Fprintf(p.out, "  %s. Try again.\n", err.Error())
	}
}
