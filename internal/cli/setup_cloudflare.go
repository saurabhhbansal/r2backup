package cli

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/saurabhhbansal/r2backup/internal/cfapi"
	"github.com/saurabhhbansal/r2backup/internal/oauth"
)

// cloudflareClientID is r2backup's registered OAuth client.
//
// It is in the source on purpose. A public client ships no secret -- there is
// nowhere in a binary anyone can download to keep one -- so PKCE carries the
// proof instead and this identifier is not sensitive. See internal/oauth.
const cloudflareClientID = "5a327ccc3567346da55b31b65322a456"

// browserSignInTimeout is how long the sign-in waits for someone to finish in
// their browser.
//
// Generous on purpose. What actually happens in this window is finding a
// password manager, a two-factor prompt on a phone that is in another room,
// and picking the right account out of several. Being impatient here costs
// someone the whole flow and makes them start again, which is a far worse
// outcome than waiting.
const browserSignInTimeout = 5 * time.Minute

// discovered is what a browser sign-in works out on someone's behalf.
type discovered struct {
	AccountID string
	Bucket    string
}

// discoverViaCloudflare offers the browser sign-in and, if it is taken up,
// returns the account and bucket so nobody has to find either by hand.
//
// It cannot return the R2 keys themselves, and that is not an oversight.
// Cloudflare deliberately offers no OAuth scope that mints API tokens -- an
// app that could would be able to grant itself permanent access far beyond
// what was consented to -- so the access key and secret still have to be
// created in the dashboard and pasted. What this removes is everything
// around them: the account ID, the bucket name, and having to know which
// dashboard page to look on.
//
// Every failure here is soft. Declining, no browser, a rejected token, a
// network that is not there -- all of them fall through to typing the details
// in by hand, which is the path that existed before any of this and still
// works. Sign-in is a convenience, and a convenience that can block setup is
// a liability.
func discoverViaCloudflare(ctx context.Context, p *prompter) (discovered, bool) {
	// Nothing is offered where nothing can be opened. Asking first and
	// failing afterwards would waste a question on every SSH install, and on
	// a scripted setup the prompt would swallow the answer meant for the
	// account ID.
	if !oauth.BrowserAvailable() {
		return discovered{}, false
	}

	fmt.Fprintln(p.out)
	fmt.Fprintln(p.out, "You can sign in to Cloudflare in your browser and r2backup will find")
	fmt.Fprintln(p.out, "your account and bucket for you. It asks only to manage R2 storage,")
	fmt.Fprintln(p.out, "never to read your files, and the sign-in is thrown away when setup")
	fmt.Fprintln(p.out, "is done.")
	fmt.Fprintln(p.out)

	yes, err := confirm(p, "Sign in with Cloudflare? [Y/n]", true)
	if err != nil || !yes {
		return discovered{}, false
	}

	cfg := oauth.Config{ClientID: cloudflareClientID, Scopes: cfapi.Scopes}

	authCtx, cancel := context.WithTimeout(ctx, browserSignInTimeout)
	defer cancel()

	fmt.Fprintln(p.out, "\nOpening your browser...")
	tok, err := cfg.Authorize(authCtx)
	if err != nil {
		explainSignInFailure(p, err)
		return discovered{}, false
	}
	// The token is scaffolding. Revoking it rather than letting the variable
	// go out of scope is the difference between a credential that is dead now
	// and one that stays usable on Cloudflare's side for its full lifetime.
	defer func() {
		revokeCtx, revokeCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer revokeCancel()
		_ = cfg.Revoke(revokeCtx, tok.AccessToken)
	}()

	api := cfapi.New(tok.AccessToken)

	account, err := chooseAccount(ctx, p, api)
	if err != nil {
		explainSignInFailure(p, err)
		return discovered{}, false
	}
	bucket, err := chooseBucket(ctx, p, api, account.ID)
	if err != nil {
		explainSignInFailure(p, err)
		return discovered{}, false
	}
	return discovered{AccountID: account.ID, Bucket: bucket}, true
}

// chooseAccount picks the account, asking only when there is a choice to make.
func chooseAccount(ctx context.Context, p *prompter, api *cfapi.Client) (cfapi.Account, error) {
	accounts, err := api.Accounts(ctx)
	if err != nil {
		return cfapi.Account{}, err
	}
	if len(accounts) == 1 {
		fmt.Fprintf(p.out, "Account: %s\n", accounts[0].Name)
		return accounts[0], nil
	}
	fmt.Fprintln(p.out, "\nWhich Cloudflare account?")
	for i, a := range accounts {
		fmt.Fprintf(p.out, "  %d) %s\n", i+1, a.Name)
	}
	choice, err := askIndex(p, "Number", len(accounts))
	if err != nil {
		return cfapi.Account{}, err
	}
	return accounts[choice], nil
}

// chooseBucket picks or creates the bucket the backups go in.
func chooseBucket(ctx context.Context, p *prompter, api *cfapi.Client, accountID string) (string, error) {
	buckets, err := api.Buckets(ctx, accountID)
	if err != nil {
		return "", err
	}
	if len(buckets) == 0 {
		fmt.Fprintln(p.out, "\nThis account has no R2 buckets yet.")
		return createBucket(ctx, p, api, accountID)
	}

	fmt.Fprintln(p.out, "\nWhich bucket should the backups go in?")
	for i, b := range buckets {
		fmt.Fprintf(p.out, "  %d) %s\n", i+1, b.Name)
	}
	fmt.Fprintf(p.out, "  %d) Make a new one\n", len(buckets)+1)

	choice, err := askIndex(p, "Number", len(buckets)+1)
	if err != nil {
		return "", err
	}
	if choice == len(buckets) {
		return createBucket(ctx, p, api, accountID)
	}
	return buckets[choice].Name, nil
}

// createBucket makes a bucket, re-asking on a name that is taken or invalid
// rather than dropping the whole sign-in over a fixable answer.
func createBucket(ctx context.Context, p *prompter, api *cfapi.Client, accountID string) (string, error) {
	fmt.Fprintln(p.out, "Lowercase letters, numbers and hyphens.")
	for attempt := 0; attempt < 5; attempt++ {
		name, err := p.askRequired("New bucket name")
		if err != nil {
			return "", err
		}
		name = strings.TrimSpace(name)
		err = api.CreateBucket(ctx, accountID, name)
		switch {
		case err == nil:
			fmt.Fprintf(p.out, "Made %s.\n", name)
			return name, nil
		case errors.Is(err, cfapi.ErrBucketExists):
			// Already there is not a failure. It is almost always someone
			// re-running setup, and the bucket they meant is the one that
			// exists.
			fmt.Fprintf(p.out, "%s already exists -- using it.\n", name)
			return name, nil
		case errors.Is(err, cfapi.ErrUnauthorized):
			return "", err
		default:
			fmt.Fprintf(p.out, "%v\n", err)
		}
	}
	return "", errors.New("too many attempts at a bucket name")
}

// explainSignInFailure says what went wrong in terms of what happens next,
// which is always the same thing: type the details in instead.
func explainSignInFailure(p *prompter, err error) {
	switch {
	case errors.Is(err, oauth.ErrDenied):
		fmt.Fprintln(p.out, "\nSign-in cancelled.")
	case errors.Is(err, oauth.ErrNoBrowser):
		fmt.Fprintln(p.out, "\nThere is no browser to open on this computer.")
	case errors.Is(err, oauth.ErrPortsBusy):
		fmt.Fprintln(p.out, "\nThe ports the sign-in needs are all in use.")
	case errors.Is(err, context.DeadlineExceeded):
		fmt.Fprintln(p.out, "\nThe sign-in timed out.")
	case errors.Is(err, cfapi.ErrNoAccounts):
		fmt.Fprintln(p.out, "\nThat Cloudflare login has no accounts.")
	default:
		fmt.Fprintf(p.out, "\nCould not finish the Cloudflare sign-in: %v\n", err)
	}
	fmt.Fprintln(p.out, "Carrying on with the details typed in by hand.")
}

// confirm asks a yes-or-no question through the prompter's own reader.
//
// The package already has an askYesNo, and this is deliberately not it: that
// one wraps the io.Reader it is handed in a fresh bufio.Reader on every call,
// which would swallow whatever the prompter has already buffered and leave
// the next question reading someone else's answer. Anything on the setup path
// has to go through p.
func confirm(p *prompter, label string, def bool) (bool, error) {
	answer, err := p.ask(label)
	if err != nil {
		return false, err
	}
	switch strings.ToLower(strings.TrimSpace(answer)) {
	case "":
		return def, nil
	case "y", "yes":
		return true, nil
	case "n", "no":
		return false, nil
	default:
		return def, nil
	}
}

// askIndex asks for a number from 1 to n and returns it zero-based.
func askIndex(p *prompter, label string, n int) (int, error) {
	for attempt := 0; attempt < 5; attempt++ {
		answer, err := p.askRequired(label)
		if err != nil {
			return 0, err
		}
		i, convErr := strconv.Atoi(strings.TrimSpace(answer))
		if convErr != nil || i < 1 || i > n {
			fmt.Fprintf(p.out, "Enter a number between 1 and %d.\n", n)
			continue
		}
		return i - 1, nil
	}
	return 0, errors.New("too many attempts")
}

// tokenPageURL is the dashboard page where R2 API tokens are made. Linking
// straight to it, with the account already known, is what turns "go and find
// the right page" into a click.
func tokenPageURL(accountID string) string {
	if accountID == "" {
		return "https://dash.cloudflare.com/?to=/:account/r2/api-tokens"
	}
	return "https://dash.cloudflare.com/" + accountID + "/r2/api-tokens"
}
