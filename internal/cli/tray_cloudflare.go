package cli

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/saurabhhbansal/r2backup/internal/cfapi"
	"github.com/saurabhhbansal/r2backup/internal/oauth"
)

// The panel's Cloudflare sign-in: the browser half of `r2b setup`, which finds
// the account and the bucket (and makes a bucket) so nobody has to look
// either up. It cannot produce the R2 keys -- see discoverViaCloudflare --
// so the page still asks for those, with the right dashboard page one click
// away.

// cfSessionTTL is how long a finished sign-in is kept for picking a bucket.
// The token is revoked when the page says it is done, or after this.
const cfSessionTTL = 15 * time.Minute

// cfSession is one Cloudflare sign-in: waiting on the browser, or signed in
// and kept just long enough to choose an account and a bucket.
type cfSession struct {
	cfg    oauth.Config
	cancel context.CancelFunc
	token  string
	api    *cfapi.Client
	expire *time.Timer
}

// cfError says what went wrong with a sign-in in terms of what to do next,
// which is always the same: type the details in.
func cfError(err error) error {
	switch {
	case errors.Is(err, oauth.ErrDenied):
		return errors.New("the sign-in was cancelled. You can type the details in instead")
	case errors.Is(err, oauth.ErrNoBrowser):
		return errors.New("there is no browser to open on this computer. Type the details in instead")
	case errors.Is(err, oauth.ErrPortsBusy):
		return errors.New("the sign-in could not start because the ports it needs are in use. Type the details in instead")
	case errors.Is(err, context.DeadlineExceeded):
		return errors.New("the sign-in timed out. Try again, or type the details in")
	case errors.Is(err, context.Canceled):
		return errors.New("the sign-in was stopped")
	case errors.Is(err, cfapi.ErrNoAccounts):
		return errors.New("that Cloudflare login has no accounts")
	case errors.Is(err, cfapi.ErrUnauthorized):
		return errors.New("Cloudflare did not accept the sign-in. Try again, or type the details in")
	}
	return fmt.Errorf("could not finish the Cloudflare sign-in: %w", err)
}

// endCF revokes the sign-in, if there is one, and forgets it.
func (t *trayBackend) endCF() {
	t.mu.Lock()
	s := t.cf
	t.cf = nil
	t.mu.Unlock()
	if s == nil {
		return
	}
	if s.cancel != nil {
		s.cancel()
	}
	if s.expire != nil {
		s.expire.Stop()
	}
	if s.token != "" {
		// The token is scaffolding: revoked now rather than left usable on
		// Cloudflare's side for its whole lifetime.
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = s.cfg.Revoke(ctx, s.token)
	}
}

func (t *trayBackend) cfAPI() (*cfapi.Client, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.cf == nil || t.cf.api == nil {
		return nil, errors.New("sign in with Cloudflare again")
	}
	return t.cf.api, nil
}

type cfAccountOut struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// cloudflareCall serves the panel's Cloudflare sign-in requests.
func (t *trayBackend) cloudflareCall(ctx context.Context, method string, raw []byte) (out any, ok bool, err error) {
	switch method {
	case "cfSignIn":
		if !oauth.BrowserAvailable() {
			return nil, true, cfError(oauth.ErrNoBrowser)
		}
		t.endCF() // a second press starts again
		cfg := oauth.Config{ClientID: cloudflareClientID, Scopes: cfapi.Scopes}
		authCtx, cancel := context.WithTimeout(ctx, browserSignInTimeout)
		defer cancel()
		s := &cfSession{cfg: cfg, cancel: cancel}
		t.mu.Lock()
		t.cf = s
		t.mu.Unlock()

		tok, err := cfg.Authorize(authCtx)
		if err != nil {
			t.endCF()
			return nil, true, cfError(err)
		}
		api := cfapi.New(tok.AccessToken)
		accounts, err := api.Accounts(ctx)
		if err == nil && len(accounts) == 0 {
			err = cfapi.ErrNoAccounts
		}
		t.mu.Lock()
		owned := t.cf == s
		if owned {
			s.cancel, s.token, s.api = nil, tok.AccessToken, api
			s.expire = time.AfterFunc(cfSessionTTL, t.endCF)
		}
		t.mu.Unlock()
		if !owned {
			// Stopped, or started again, while the browser was finishing:
			// this token is nobody's, so it goes now.
			rctx, rcancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer rcancel()
			_ = cfg.Revoke(rctx, tok.AccessToken)
			return nil, true, cfError(context.Canceled)
		}
		if err != nil {
			t.endCF()
			return nil, true, cfError(err)
		}
		res := make([]cfAccountOut, 0, len(accounts))
		for _, a := range accounts {
			res = append(res, cfAccountOut{ID: a.ID, Name: a.Name})
		}
		return map[string]any{"accounts": res}, true, nil

	case "cfBuckets":
		var a struct {
			AccountID string `json:"accountId"`
		}
		if err := decodeArgs(raw, &a); err != nil {
			return nil, true, err
		}
		api, err := t.cfAPI()
		if err != nil {
			return nil, true, err
		}
		buckets, err := api.Buckets(ctx, a.AccountID)
		if err != nil {
			return nil, true, cfError(err)
		}
		names := make([]string, 0, len(buckets))
		for _, b := range buckets {
			names = append(names, b.Name)
		}
		sort.Strings(names)
		return names, true, nil

	case "cfCreateBucket":
		var a struct {
			AccountID string `json:"accountId"`
			Name      string `json:"name"`
		}
		if err := decodeArgs(raw, &a); err != nil {
			return nil, true, err
		}
		a.Name = strings.TrimSpace(a.Name)
		if err := cfapi.ValidateBucketName(a.Name); err != nil {
			return nil, true, err
		}
		api, err := t.cfAPI()
		if err != nil {
			return nil, true, err
		}
		// One that is already there is the one meant, almost always: someone
		// setting up again.
		if err := api.CreateBucket(ctx, a.AccountID, a.Name); err != nil && !errors.Is(err, cfapi.ErrBucketExists) {
			return nil, true, cfError(err)
		}
		return a.Name, true, nil

	case "cfDone", "cfCancel":
		t.endCF()
		return nil, true, nil
	}
	return nil, false, nil
}
