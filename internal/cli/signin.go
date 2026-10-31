package cli

import (
	"context"
	"errors"
	"fmt"
	"runtime"

	"github.com/saurabhhbansal/r2backup/internal/account"
)

// Signing in to the hosted account is email, then the code sent to it, then a
// six-digit PIN. The PIN locks the storage keys the account keeps (see
// internal/account/pin.go). Everything here is shared by the terminal setup,
// the terminal interface and the taskbar panel, so they cannot disagree about
// what signing in means.

// errNoPendingSignIn is a PIN with no email code before it.
var errNoPendingSignIn = errors.New("enter the email code first")

// accountSession is this computer's sign-in: the token and the key that opens
// the keys the account keeps. A computer signed in before PINs has a token and
// no key, which reads as account.ErrPINRequired.
func accountSession() (token string, key []byte, err error) {
	token, err = requireAccountToken()
	if err != nil {
		return "", nil, err
	}
	key, err = account.LoadSealKey()
	if err != nil {
		return "", nil, err
	}
	return token, key, nil
}

// finishSignIn saves a session from the PIN step and registers this computer.
// Keys an account kept from before PINs come back once with the session; they
// are saved sealed straight away, which is what makes the server delete the
// copy it could read.
func finishSignIn(ctx context.Context, s *account.Session) error {
	if err := account.SaveSession(s); err != nil {
		return err
	}
	c := accountClient()
	if s.LegacyKeys != nil && (s.LegacyKeys.Primary != nil || s.LegacyKeys.Second != nil) {
		if err := c.PutKeys(ctx, s.Token, s.SealKey, *s.LegacyKeys); err != nil {
			return fmt.Errorf("signed in, but the keys your account kept could not be locked with the PIN: %w",
				friendlyAccountErr(err))
		}
	}
	_ = c.RegisterDevice(ctx, s.Token, machineName(), runtime.GOOS)
	return nil
}

// pinStepErr words the PIN step's failures for a person.
func pinStepErr(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, account.ErrUnauthorized):
		return errors.New("that sign-in took too long. Ask for a new email code")
	case errors.Is(err, account.ErrPINAlreadySet):
		return errors.New("this account already has a PIN. Enter it instead")
	}
	return friendlyAccountErr(err)
}

// SignInVerify checks the email code and holds the PIN step open. It reports
// whether the account already has a PIN to enter, or needs one chosen.
func (d *dashboard) SignInVerify(ctx context.Context, email, code string) (bool, error) {
	ch, err := accountClient().Verify(ctx, email, code)
	if err != nil {
		if errors.Is(err, account.ErrInvalidCode) {
			return false, errors.New("that code is not right, or it has expired")
		}
		return false, friendlyAccountErr(err)
	}
	d.pinMu.Lock()
	d.pending = ch
	d.pinMu.Unlock()
	return ch.PINSet, nil
}

func (d *dashboard) pendingSignIn() (*account.PINChallenge, error) {
	d.pinMu.Lock()
	defer d.pinMu.Unlock()
	if d.pending == nil {
		return nil, errNoPendingSignIn
	}
	return d.pending, nil
}

func (d *dashboard) endSignIn() {
	d.pinMu.Lock()
	d.pending = nil
	d.pinMu.Unlock()
}

// SignInPIN finishes signing in: the account's PIN, or its first one. A wrong
// PIN leaves the step open, so the next try does not need a new code.
func (d *dashboard) SignInPIN(ctx context.Context, pin string) error {
	ch, err := d.pendingSignIn()
	if err != nil {
		return err
	}
	c := accountClient()
	var s *account.Session
	if ch.PINSet {
		s, err = c.VerifyPIN(ctx, ch, pin)
	} else {
		s, err = c.SetupPIN(ctx, ch.PINToken, pin)
	}
	if err != nil {
		return pinStepErr(err)
	}
	d.endSignIn()
	return finishSignIn(ctx, s)
}

// ResetPIN replaces a forgotten PIN, after a fresh email code. The keys the
// account kept are deleted -- they were locked with the old PIN -- and every
// other computer is signed out.
func (d *dashboard) ResetPIN(ctx context.Context, pin string) error {
	ch, err := d.pendingSignIn()
	if err != nil {
		return err
	}
	s, err := accountClient().ResetPIN(ctx, ch.PINToken, pin)
	if err != nil {
		return pinStepErr(err)
	}
	d.endSignIn()
	if err := finishSignIn(ctx, s); err != nil {
		return err
	}
	// The account has nothing now. This computer's keys, if it has any, are
	// the obvious thing to keep there. Not saving them is not a failed reset:
	// the PIN is changed and this computer signed in, and the account screen
	// offers to save them again.
	if localStoredKeys(d.app).Primary != nil {
		_ = d.PushKeys(ctx)
	}
	return nil
}

// SetPIN gives a computer signed in before PINs its PIN, without a new email
// code. Its session is good for nothing else until it has one.
func (d *dashboard) SetPIN(ctx context.Context, pin string) error {
	token, err := requireAccountToken()
	if err != nil {
		return err
	}
	s, err := accountClient().SetupPIN(ctx, token, pin)
	if err != nil {
		return pinStepErr(err)
	}
	return finishSignIn(ctx, s)
}

// SignOut forgets the session and the seal key on this computer. Storage
// keys and backups are untouched.
func (d *dashboard) SignOut(ctx context.Context) error {
	d.endSignIn()
	return account.ClearToken()
}

// localStoredKeys is what this computer would save to the account.
func localStoredKeys(a *app) account.StoredKeys {
	var k account.StoredKeys
	if c, err := a.creds.Load(); err == nil {
		k.Primary = &c
	}
	if c, err := a.mirror.Load(); err == nil {
		k.Second = &c
	}
	if c, err := a.source.Load(); err == nil {
		k.Source = &c
	}
	return k
}

// PushKeys saves this computer's storage keys to the account, sealed with the
// PIN. They are checked first: keys that do not work would hand every later
// computer the same broken setup.
func (d *dashboard) PushKeys(ctx context.Context) error {
	token, key, err := accountSession()
	if err != nil {
		return err
	}
	k := localStoredKeys(d.app)
	if k.Primary == nil {
		return errors.New("there are no storage keys on this computer to save yet")
	}
	if err := reachable(ctx, *k.Primary); err != nil {
		return err
	}
	return friendlyAccountErr(accountClient().PutKeys(ctx, token, key, k))
}

// PullKeys fetches the keys the account keeps and saves the ones this
// computer does not have yet. Keys already here stay: this computer's folders
// are backed up with them.
func (d *dashboard) PullKeys(ctx context.Context) error {
	token, key, err := accountSession()
	if err != nil {
		return err
	}
	remote, err := accountClient().GetKeys(ctx, token, key)
	switch {
	case errors.Is(err, account.ErrNotFound):
		return errors.New("no keys are saved to this account yet")
	case errors.Is(err, account.ErrDecryptFailed):
		return errors.New("the keys saved to this account were locked with a different PIN. Enter your keys again")
	case err != nil:
		return friendlyAccountErr(err)
	}
	a := d.app
	if remote.Primary != nil && !a.creds.Exists() {
		if err := reachable(ctx, *remote.Primary); err != nil {
			return fmt.Errorf("your account has keys saved, but %w", err)
		}
		if err := a.creds.Save(*remote.Primary); err != nil {
			return err
		}
		d.forgetClient()
	}
	if remote.Second != nil && !a.mirror.Exists() {
		_ = a.mirror.Save(*remote.Second)
	}
	if remote.Source != nil && !a.source.Exists() {
		_ = a.source.Save(*remote.Source)
	}
	return nil
}
