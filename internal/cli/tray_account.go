package cli

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"time"

	"github.com/saurabhhbansal/r2backup/internal/account"
	"github.com/saurabhhbansal/r2backup/internal/creds"
)

// The panel's account: signing in with an email code is required, and the
// account keeps this computer's storage keys -- where backups go and the
// second copy -- so another computer signed in with the same email picks
// them up without anyone typing them again.

// tokenClaims is what the panel reads from the saved session without asking
// the server: who is signed in, and until when.
func tokenClaims(token string) (email string, exp time.Time) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return "", time.Time{}
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", time.Time{}
	}
	var c struct {
		Sub string `json:"sub"`
		Exp int64  `json:"exp"`
	}
	if json.Unmarshal(raw, &c) != nil {
		return "", time.Time{}
	}
	return c.Sub, time.Unix(c.Exp, 0)
}

// signedInHere reports who is signed in on this computer, from the saved
// session alone. It never touches the network, so the panel can ask on every
// refresh, and an account server that is down does not sign anybody out.
func signedInHere(now time.Time) (string, bool) {
	token, err := account.LoadToken()
	if err != nil || token == "" {
		return "", false
	}
	email, exp := tokenClaims(token)
	if email == "" || (!exp.IsZero() && !now.Before(exp)) {
		return "", false
	}
	return email, true
}

// localKeys is what this computer would save to the account.
func (t *trayBackend) localKeys() account.StoredKeys { return localStoredKeys(t.d.app) }

// pushKeys saves this computer's keys to the account. Not signed in -- or
// using r2backup without an account -- is not an error: there is nowhere to
// save them.
func (t *trayBackend) pushKeys(ctx context.Context) error {
	token, key, err := accountSession()
	if err != nil {
		return nil
	}
	k := t.localKeys()
	if k.Primary == nil {
		return nil
	}
	return friendlyAccountErr(accountClient().PutKeys(ctx, token, key, k))
}

// reachable opens c and lists it once: keys that do not work are caught
// before anything is built on them.
func reachable(ctx context.Context, c creds.Credentials) error {
	if err := c.Valid(); err != nil {
		return err
	}
	store, err := openStore(ctx, c, nil)
	if err != nil {
		return err
	}
	if _, err := store.List(ctx, "r2backup/"); err != nil {
		return errors.New("could not reach it with the saved keys: " + err.Error())
	}
	return nil
}

type keysSyncOut struct {
	Pulled  bool   `json:"pulled"`  // keys came from the account
	Pushed  bool   `json:"pushed"`  // this computer's keys went to the account
	Primary string `json:"primary"` // where backups go now, "" for nowhere yet
	Second  string `json:"second"`
	Err     string `json:"err,omitempty"`
}

// syncKeys runs right after signing in. A computer with no storage takes the
// account's; an account with nothing saved takes this computer's. When both
// have keys, this computer's stay as they are -- its folders are backed up
// there -- and the account screen offers to save them.
func (t *trayBackend) syncKeys(ctx context.Context) (keysSyncOut, error) {
	var out keysSyncOut
	token, key, err := accountSession()
	if err != nil {
		return out, err
	}
	a := t.d.app
	local := t.localKeys()
	remote, err := accountClient().GetKeys(ctx, token, key)
	switch {
	case errors.Is(err, account.ErrNotFound), errors.Is(err, account.ErrDecryptFailed):
		// Nothing saved, or saved under a PIN since reset: either way this
		// computer's keys are what the account should keep.
		remote = nil
	case err != nil:
		return out, friendlyAccountErr(err)
	}

	if remote != nil && remote.Primary != nil && local.Primary == nil {
		if err := reachable(ctx, *remote.Primary); err != nil {
			out.Err = "Your account has keys saved, but " + err.Error()
		} else if err := a.creds.Save(*remote.Primary); err != nil {
			return out, err
		} else {
			t.d.forgetClient()
			out.Pulled = true
			local.Primary = remote.Primary
		}
	}
	if remote != nil && remote.Source != nil && local.Source == nil {
		if err := a.source.Save(*remote.Source); err == nil {
			local.Source = remote.Source
		}
	}
	if remote != nil && remote.Second != nil && local.Second == nil && local.Primary != nil &&
		!samePlace(*local.Primary, *remote.Second) {
		if reachable(ctx, *remote.Second) == nil {
			if err := a.mirror.Save(*remote.Second); err == nil {
				_ = setMirrorAuto(true)
				out.Pulled = true
				local.Second = remote.Second
			}
		}
	}
	if (remote == nil || remote.Primary == nil) && local.Primary != nil {
		if err := accountClient().PutKeys(ctx, token, key, local); err == nil {
			out.Pushed = true
		}
	}
	if local.Primary != nil {
		out.Primary = destLabel(*local.Primary)
	}
	if local.Second != nil {
		out.Second = destLabel(*local.Second)
	}
	t.invalidate()
	return out, nil
}

// storeOut describes saved keys without giving them away: enough to see
// they are there and which ones they are.
type storeOut struct {
	Provider string `json:"provider"` // drime, r2, b2, s3 or s3-custom
	Label    string `json:"label"`    // "Drime · r2backup", "Backblaze B2 · my-bucket"
	Hint     string `json:"hint"`     // "Token ••••3f2a"
	// Bundle is whether small files are packed into archives here.
	Bundle bool `json:"bundle"`
}

func describeStore(c creds.Credentials) *storeOut {
	o := &storeOut{Provider: c.Kind(), Label: destLabel(c), Bundle: c.Bundle}
	if c.IsDrime() {
		root := c.DrimeRoot
		if root == "" {
			root = "r2backup"
		}
		o.Hint = "Folder " + root + " · token " + masked(c.DrimeToken)
	} else {
		o.Hint = "Bucket " + c.Bucket + " · key " + masked(c.AccessKeyID)
		if ep := c.S3(); c.Kind() == creds.ProviderCustom {
			o.Hint = ep.URL + " · " + o.Hint
		} else if ep.Region != "" && ep.Region != "auto" {
			o.Hint = o.Hint + " · " + ep.Region
		}
	}
	return o
}

// masked keeps the last four characters, which is what anyone comparing a key
// with the one in their dashboard looks at.
func masked(s string) string {
	s = strings.TrimSpace(s)
	if len(s) <= 4 {
		return "••••"
	}
	return "••••" + s[len(s)-4:]
}

type accountConfigOut struct {
	SignedIn bool      `json:"signedIn"`
	Email    string    `json:"email,omitempty"`
	Machine  string    `json:"machine"`
	Primary  *storeOut `json:"primary"`
	Second   *storeOut `json:"second"`
	Auto     bool      `json:"auto"` // copy to the second after every backup
	Saved    bool      `json:"saved"`
	// NeedsPIN is a sign-in from before PINs: the panel asks for one
	// (setPin) before anything else.
	NeedsPIN bool `json:"needsPin"`
	// Mode is "account" when signed in, "local" when using r2backup without
	// an account, "" when neither has been chosen yet.
	Mode    string       `json:"mode"`
	Err     string       `json:"err,omitempty"`
	Devices []deviceJSON `json:"devices"`
}

// accountConfig is everything the account screen shows.
func (t *trayBackend) accountConfig(ctx context.Context) (accountConfigOut, error) {
	out := accountConfigOut{Machine: machineName(), Devices: []deviceJSON{}, Mode: accountMode()}
	local := t.localKeys()
	if local.Primary != nil {
		out.Primary = describeStore(*local.Primary)
	}
	if local.Second != nil {
		out.Second = describeStore(*local.Second)
		if mi, err := t.mirrorInfo(); err == nil {
			out.Auto = mi.Auto
		}
	}

	v, err := t.d.Account(ctx)
	if err != nil {
		return out, err
	}
	out.SignedIn, out.Email, out.Err = v.SignedIn, v.Email, v.Err
	for _, d := range v.Devices {
		out.Devices = append(out.Devices, deviceJSON{Name: d.Name, OS: d.OS, This: d.This, LastSeen: d.LastSeen.Format(time.RFC3339)})
	}
	if !v.SignedIn {
		// Signed out by the server, or never signed in: the panel asks again.
		if email, ok := signedInHere(time.Now()); ok && v.Err != "" {
			out.SignedIn, out.Email = true, email // offline, not signed out
		}
		return out, nil
	}
	if v.NeedsPIN {
		out.NeedsPIN = true
		return out, nil
	}
	token, key, err := accountSession()
	if err != nil {
		return out, nil
	}
	remote, err := accountClient().GetKeys(ctx, token, key)
	switch {
	case errors.Is(err, account.ErrNotFound), errors.Is(err, account.ErrDecryptFailed):
		out.Saved = local.Primary == nil
	case err != nil:
		if out.Err == "" {
			out.Err = "Could not check the keys saved to your account: " + friendlyAccountErr(err).Error()
		}
	default:
		out.Saved = sameKeys(local, *remote)
	}
	return out, nil
}

func sameKeys(a, b account.StoredKeys) bool {
	return reflect.DeepEqual(a.Primary, b.Primary) && reflect.DeepEqual(a.Second, b.Second) &&
		reflect.DeepEqual(a.Source, b.Source)
}

// accountCall serves the account screen.
func (t *trayBackend) accountCall(ctx context.Context, method string, raw []byte) (out any, ok bool, err error) {
	switch method {
	case "accountConfig":
		res, err := t.accountConfig(ctx)
		return res, true, err

	case "keysSync":
		res, err := t.syncKeys(ctx)
		return res, true, err

	case "keysPush":
		return nil, true, t.pushKeys(ctx)

	// Pack small files into archives at a destination, or stop. Backups
	// already there stay readable either way (see internal/bundle).
	case "setBundle":
		var a struct {
			Which string `json:"which"` // "primary" or "second"
			On    bool   `json:"on"`
		}
		if err := decodeArgs(raw, &a); err != nil {
			return nil, true, err
		}
		store := t.d.app.creds
		if a.Which == "second" {
			store = t.d.app.mirror
		}
		c, err := store.Load()
		if err != nil {
			return nil, true, err
		}
		c.Bundle = a.On
		if err := store.Save(c); err != nil {
			return nil, true, err
		}
		t.d.forgetClient()
		t.invalidate()
		_ = t.pushKeys(ctx)
		return describeStore(c), true, nil

	// The PIN step after signInVerify: the account's PIN, or its first one.
	case "signInPin", "pinReset", "setPin":
		var a struct {
			PIN string `json:"pin"`
		}
		if err := decodeArgs(raw, &a); err != nil {
			return nil, true, err
		}
		var err error
		switch method {
		case "signInPin":
			err = t.d.SignInPIN(ctx, a.PIN)
		case "pinReset": // forgot it: after a fresh email code
			err = t.d.ResetPIN(ctx, a.PIN)
		default: // a sign-in from before PINs
			err = t.d.SetPIN(ctx, a.PIN)
		}
		t.invalidate()
		if err != nil {
			return nil, true, err
		}
		_ = setAccountMode(accountModeAccount)
		res, err := t.syncKeys(ctx)
		return res, true, err

	// Use r2backup on this computer alone: keys stay here.
	case "useLocal":
		t.invalidate()
		return nil, true, setAccountMode(accountModeLocal)

	case "keysExport":
		var a struct {
			Path       string `json:"path"`
			Passphrase string `json:"passphrase"`
		}
		if err := decodeArgs(raw, &a); err != nil {
			return nil, true, err
		}
		return nil, true, exportKeys(t.d.app, a.Path, a.Passphrase)

	case "keysImport":
		var a struct {
			Path       string `json:"path"`
			Passphrase string `json:"passphrase"`
		}
		if err := decodeArgs(raw, &a); err != nil {
			return nil, true, err
		}
		got, err := importKeys(ctx, t.d.app, a.Path, a.Passphrase)
		t.d.forgetClient()
		t.invalidate()
		if err == nil {
			_ = t.pushKeys(ctx)
		}
		return got, true, err

	case "renameMachine":
		var a struct {
			Name string `json:"name"`
		}
		if err := decodeArgs(raw, &a); err != nil {
			return nil, true, err
		}
		if err := t.renameMachine(ctx, a.Name); err != nil {
			return nil, true, err
		}
		return map[string]string{"machine": machineName()}, true, nil
	}
	return nil, false, nil
}
