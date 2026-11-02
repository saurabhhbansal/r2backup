// Package ultrafast is Ultra fast mode: copying Drime files to S3 storage
// cloud to cloud, through a small Worker in the user's own Cloudflare
// account, instead of down to this computer and up again.
//
// Setting it up uploads the relay (see /relay) with the user's Cloudflare API
// token, turns on its workers.dev address and binds a random password only
// this computer knows. Turning it off deletes the Worker and forgets the
// token. Nothing goes through the developer's servers: the Worker, the
// token, the password and every byte copied are the user's own.
//
// r2backup still decides what to copy. For each file it asks Drime for a
// temporary download link and the S3 storage for presigned upload links,
// and has the Worker carry the bytes between them, a part at a time for big
// files, several parts at once.
package ultrafast

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/saurabhhbansal/r2backup/internal/creds"
	"github.com/saurabhhbansal/r2backup/relay"
)

// Note is what someone reads before turning Ultra fast mode on. The panel
// and the terminal show the same words.
const Note = "Ultra fast mode is experimental. It copies your Drime files to your S3 storage cloud to cloud: " +
	"each file is downloaded from Drime by a small program (a Cloudflare Worker) in your own Cloudflare account " +
	"and uploaded from there to your S3 storage, so nothing goes through this PC.\n\n" +
	"To set that Worker up, r2backup needs a Cloudflare API token with one permission: Account > Workers Scripts > Edit. " +
	"The token and the Worker's password stay on this computer.\n\n" +
	"It uses your Cloudflare account's free Worker allowance: 100,000 requests a day, one for each file it copies " +
	"and one for each 64 MB of a big file. If you use Backblaze B2 or Amazon S3, you need a free Cloudflare account for this too.\n\n" +
	"With packing on, files of 4 MB or less still come to this PC to be packed, so your S3 storage gets as few requests as before; " +
	"only bigger files go cloud to cloud. With packing off, every file goes cloud to cloud, one S3 request each.\n\n" +
	"If the Worker fails or the day's allowance runs out, the rest is copied through this PC as now, and you are told. " +
	"Turning it off deletes the Worker from your Cloudflare account and forgets the token."

// TokenHelp says where to make the token.
const TokenHelp = "Make a token at dash.cloudflare.com/profile/api-tokens: Create Token > Custom token, " +
	"permission Account > Workers Scripts > Edit, for your account."

// State is what this computer keeps about its relay.
type State struct {
	Token       string `json:"token"`
	AccountID   string `json:"account_id"`
	AccountName string `json:"account_name,omitempty"`
	Script      string `json:"script"`
	URL         string `json:"url"`
	Secret      string `json:"secret"`
	Version     string `json:"version"`
	Since       int64  `json:"since"`
}

// Store keeps State sealed on disk, as the storage keys are.
type Store struct{ s *creds.Secret }

// Open is the Store at path.
func Open(path string) *Store { return &Store{s: creds.OpenSecret(path)} }

// Load is the saved State; ok is false when Ultra fast mode is off.
func (s *Store) Load() (st State, ok bool, err error) {
	b, err := s.s.Load()
	if errors.Is(err, creds.ErrNotFound) {
		return State{}, false, nil
	}
	if err != nil {
		return State{}, false, err
	}
	if err := json.Unmarshal(b, &st); err != nil {
		return State{}, false, fmt.Errorf("ultra fast mode settings are damaged: %w", err)
	}
	return st, st.URL != "" && st.Secret != "", nil
}

// On reports whether Ultra fast mode is set up.
func (s *Store) On() bool {
	_, ok, _ := s.Load()
	return ok
}

func (s *Store) save(st State) error {
	b, err := json.Marshal(st)
	if err != nil {
		return err
	}
	return s.s.Save(b)
}

func (s *Store) forget() error { return s.s.Delete() }

// Options are for tests: where Cloudflare's API is, and how to reach the
// relay.
type Options struct {
	APIBase string
	// RelayURL, if set, replaces the workers.dev address worked out from
	// the account.
	RelayURL string
	// RelayHTTP is the client the relay is called with.
	RelayHTTP *http.Client
}

// NeedAccount is the answer when the token reaches more than one account and
// none was named.
type NeedAccount struct{ Accounts []Account }

func (e *NeedAccount) Error() string {
	names := make([]string, len(e.Accounts))
	for i, a := range e.Accounts {
		names[i] = a.Name + " (" + a.ID + ")"
	}
	return "this token reaches more than one Cloudflare account; choose one: " + strings.Join(names, ", ")
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err) // the system's random source failing is not recoverable
	}
	return hex.EncodeToString(b)
}

// Enable sets the relay up in the account the token reaches (accountID when
// it reaches several) and saves what this computer needs to use it. Called
// again, it replaces this computer's relay with a fresh one.
func Enable(ctx context.Context, s *Store, token, accountID string, o Options) (State, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return State{}, errors.New("paste the Cloudflare API token. " + TokenHelp)
	}
	cf := newCloudflare(token, o.APIBase)
	acct := Account{ID: strings.TrimSpace(accountID)}
	all, err := cf.accounts(ctx)
	switch {
	case err != nil && acct.ID == "":
		return State{}, err
	case err == nil && acct.ID == "":
		switch len(all) {
		case 0:
			return State{}, errors.New("this token does not reach any Cloudflare account")
		case 1:
			acct = all[0]
		default:
			return State{}, &NeedAccount{Accounts: all}
		}
	case err == nil:
		for _, a := range all {
			if a.ID == acct.ID {
				acct = a
			}
		}
	}

	sub, err := cf.subdomain(ctx, acct.ID)
	if err != nil {
		return State{}, fmt.Errorf("could not read your workers.dev address: %w", err)
	}
	if sub == "" {
		if sub, err = cf.createSubdomain(ctx, acct.ID, "r2b-"+randomHex(4)); err != nil {
			return State{}, fmt.Errorf("could not give your account a workers.dev address: %w", err)
		}
	}

	// One relay per computer, so turning it off on one leaves another
	// computer's alone. A computer setting up again replaces its own.
	old, had, _ := s.Load()
	name := "r2backup-relay-" + randomHex(4)
	if had && old.AccountID == acct.ID && old.Script != "" {
		name = old.Script
	}
	st := State{
		Token: token, AccountID: acct.ID, AccountName: acct.Name, Script: name,
		URL:    "https://" + name + "." + sub + ".workers.dev",
		Secret: randomHex(32), Version: relay.Version, Since: time.Now().Unix(),
	}
	if o.RelayURL != "" {
		st.URL = o.RelayURL
	}
	if err := cf.uploadScript(ctx, acct.ID, name, relay.Script, st.Secret); err != nil {
		return State{}, fmt.Errorf("could not upload the Worker: %w", err)
	}
	if err := cf.enableWorkersDev(ctx, acct.ID, name); err != nil {
		_ = cf.deleteScript(context.WithoutCancel(ctx), acct.ID, name)
		return State{}, fmt.Errorf("could not turn on the Worker's address: %w", err)
	}
	if had && old.AccountID != acct.ID && old.Script != "" {
		// The old relay is in another account; take it away too.
		_ = newCloudflare(old.Token, o.APIBase).deleteScript(ctx, old.AccountID, old.Script)
	}
	if err := s.save(st); err != nil {
		_ = cf.deleteScript(context.WithoutCancel(ctx), acct.ID, name)
		return State{}, err
	}
	return st, nil
}

// Ready waits up to wait for a just-set-up relay to answer. A new workers.dev
// address can take a minute or two to start working; until it does, copies
// go through this computer.
func Ready(ctx context.Context, st State, wait time.Duration, o Options) error {
	rc := newRelayClient(st.URL, st.Secret, o.RelayHTTP)
	deadline := time.Now().Add(wait)
	for {
		hctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		_, err := rc.health(hctx)
		cancel()
		if err == nil || time.Now().After(deadline) || ctx.Err() != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
}

// Disable deletes the relay from the account and forgets the token and the
// password, whatever happens. An error says the Worker could not be deleted
// and is still there to remove by hand; it cannot be used by anyone, as only
// this computer had its password.
func Disable(ctx context.Context, s *Store, o Options) (State, error) {
	st, ok, err := s.Load()
	if err != nil || !ok {
		_ = s.forget()
		return st, err
	}
	delErr := newCloudflare(st.Token, o.APIBase).deleteScript(ctx, st.AccountID, st.Script)
	if err := s.forget(); err != nil {
		return st, err
	}
	if delErr != nil {
		return st, fmt.Errorf("the Worker %s could not be deleted from your Cloudflare account (%v). "+
			"Delete it in the Cloudflare dashboard under Workers & Pages; without its password nobody can use it", st.Script, delErr)
	}
	return st, nil
}

// upgrade uploads the relay this binary carries over an older one, keeping
// its name and password.
func upgrade(ctx context.Context, s *Store, st State, o Options) (State, error) {
	cf := newCloudflare(st.Token, o.APIBase)
	if err := cf.uploadScript(ctx, st.AccountID, st.Script, relay.Script, st.Secret); err != nil {
		return st, err
	}
	st.Version = relay.Version
	if s != nil {
		if err := s.save(st); err != nil {
			return st, err
		}
	}
	return st, nil
}
