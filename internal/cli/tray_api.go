package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/saurabhhbansal/r2backup/internal/ui"
)

// Call is the panel's request line: everything the terminal interface does
// that a person needs, as named methods the page can ask for and draw. Each
// one calls the same dashboard method the terminal's key does, so the two
// cannot disagree about what, say, signing in means.
func (t *trayBackend) Call(ctx context.Context, method string, raw json.RawMessage) (any, error) {
	out, err := t.call(ctx, method, raw)
	if err == nil && keysChange[method] {
		// The account keeps the keys for this person's other computers. A
		// failure here is not this request's failure: the change is made on
		// this computer, and the account screen says it is not saved yet.
		_ = t.pushKeys(ctx)
	}
	return out, err
}

// keysChange are the requests after which the keys the account keeps are
// out of date.
var keysChange = map[string]bool{"connect": true, "mirrorSetup": true, "mirrorForget": true}

func (t *trayBackend) call(ctx context.Context, method string, raw json.RawMessage) (any, error) {
	switch method {
	case "connect":
		var a struct {
			Provider    string `json:"provider"`
			Token       string `json:"token"`
			AccountID   string `json:"accountId"`
			AccessKeyID string `json:"accessKeyId"`
			Secret      string `json:"secret"`
			Bucket      string `json:"bucket"`
		}
		if err := decodeArgs(raw, &a); err != nil {
			return nil, err
		}
		k := ui.Keys{AccountID: a.AccountID, AccessKeyID: a.AccessKeyID, Secret: a.Secret, Bucket: a.Bucket}
		if a.Provider == "drime" {
			k = ui.Keys{DrimeToken: a.Token}
		}
		err := t.d.SaveKeys(ctx, k)
		t.invalidate()
		if err == nil {
			t.endCF() // the account and bucket it found are in the keys now
		}
		return nil, err

	case "tokenPage":
		var a struct {
			AccountID string `json:"accountId"`
		}
		if err := decodeArgs(raw, &a); err != nil {
			return nil, err
		}
		return tokenPageURL(a.AccountID), nil

	case "account":
		v, err := t.d.Account(ctx)
		if err != nil {
			return nil, err
		}
		return accountJSON(v), nil

	case "signInStart":
		var a struct {
			Email string `json:"email"`
		}
		if err := decodeArgs(raw, &a); err != nil {
			return nil, err
		}
		return nil, t.d.SignInStart(ctx, a.Email)

	case "signInVerify":
		var a struct {
			Email string `json:"email"`
			Code  string `json:"code"`
		}
		if err := decodeArgs(raw, &a); err != nil {
			return nil, err
		}
		return nil, t.d.SignInVerify(ctx, a.Email, a.Code)

	case "unlock":
		var a struct {
			Password string `json:"password"`
		}
		if err := decodeArgs(raw, &a); err != nil {
			return nil, err
		}
		err := t.d.UnlockVault(ctx, a.Password)
		t.invalidate()
		if err != nil {
			// The terminal's wording points at one of its own keys; the
			// panel has no such key, and says what the person can do here.
			if i := strings.Index(err.Error(), " Press k"); i >= 0 {
				err = errors.New(err.Error()[:i] + " Go back and enter your keys instead.")
			}
		}
		return nil, err

	case "signOut":
		err := t.d.SignOut(ctx)
		t.invalidate()
		return nil, err
	}
	if out, ok, err := t.accountCall(ctx, method, raw); ok {
		return out, err
	}
	if out, ok, err := t.deleteCall(ctx, method, raw); ok {
		return out, err
	}
	if out, ok, err := t.restoreCall(ctx, method, raw); ok {
		return out, err
	}
	if out, ok, err := t.trashCall(ctx, method, raw); ok {
		return out, err
	}
	if out, ok, err := t.folderCall(ctx, method, raw); ok {
		return out, err
	}
	if out, ok, err := t.addCall(ctx, method, raw); ok {
		return out, err
	}
	if out, ok, err := t.mirrorCall(ctx, method, raw); ok {
		return out, err
	}
	if out, ok, err := t.cloudflareCall(ctx, method, raw); ok {
		return out, err
	}
	if out, ok, err := t.updateCall(ctx, method, raw); ok {
		return out, err
	}
	return nil, fmt.Errorf("unknown request %q", method)
}

func decodeArgs(raw json.RawMessage, into any) error {
	if len(raw) == 0 {
		return nil
	}
	return json.Unmarshal(raw, into)
}

// invalidate makes the next State read everything again: something the panel
// did has changed what there is to show.
func (t *trayBackend) invalidate() {
	t.mu.Lock()
	t.last.ok = false
	t.mu.Unlock()
}

type deviceJSON struct {
	Name     string `json:"name"`
	OS       string `json:"os"`
	LastSeen string `json:"lastSeen"`
	This     bool   `json:"this"`
}

type accountOut struct {
	Machine     string       `json:"machine"`
	SignedIn    bool         `json:"signedIn"`
	Email       string       `json:"email,omitempty"`
	VaultStored bool         `json:"vaultStored"`
	Reachable   bool         `json:"reachable"`
	Err         string       `json:"err,omitempty"`
	Devices     []deviceJSON `json:"devices"`
}

func accountJSON(v ui.AccountView) accountOut {
	out := accountOut{Machine: machineName(), SignedIn: v.SignedIn, Email: v.Email, VaultStored: v.VaultStored,
		Reachable: v.Reachable, Err: v.Err, Devices: []deviceJSON{}}
	for _, d := range v.Devices {
		out.Devices = append(out.Devices, deviceJSON{
			Name: d.Name, OS: d.OS, This: d.This,
			LastSeen: d.LastSeen.Format(time.RFC3339),
		})
	}
	return out
}
