package account

import (
	"encoding/base64"
	"errors"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/saurabhhbansal/r2backup/internal/config"
	"github.com/saurabhhbansal/r2backup/internal/creds"
)

// tokenFileName lives directly under config.DataDir(), alongside index.db
// and the other per-machine state -- a session token is exactly that kind
// of state, not a credential worth the extra protection internal/creds
// gives the R2 keys.
const tokenFileName = "account-token"

// TokenPath returns where the cached session token lives.
func TokenPath() (string, error) {
	dir, err := config.EnsureDataDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, tokenFileName), nil
}

// SaveToken persists token to disk, mode 0600. It is what a caller runs
// after a successful Client.Verify, so the next command on this machine
// doesn't need the user to sign in again.
func SaveToken(token string) error {
	path, err := TokenPath()
	if err != nil {
		return err
	}
	// 0600: this token is a 30-day bearer credential for the account's
	// vault -- anyone who reads it can fetch someone else's R2 keys.
	if err := os.WriteFile(path, []byte(strings.TrimSpace(token)), 0o600); err != nil {
		return fmt.Errorf("account: save token: %w", err)
	}
	return nil
}

// LoadToken reads back a token saved by SaveToken. A missing file is not
// an error -- it just means no one has signed in on this machine yet --
// and is reported as ("", nil) so callers can tell "not signed in" apart
// from "couldn't read the file" without parsing the error.
func LoadToken() (string, error) {
	path, err := TokenPath()
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", fmt.Errorf("account: load token: %w", err)
	}
	return strings.TrimSpace(string(data)), nil
}

// ClearToken removes the cached token and the seal key with it, e.g. on
// sign-out. Removing an already-absent file is not an error, for the same
// reason a missing file isn't one in LoadToken.
func ClearToken() error {
	path, err := TokenPath()
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("account: clear token: %w", err)
	}
	if s, err := sealKeyFile(); err == nil {
		return s.Delete()
	}
	return nil
}

// sealKeyFileName holds the key that opens the keys the account keeps. Unlike
// the token it is a real secret -- it is half made of the PIN -- so it is kept
// under the same OS protection as the storage keys themselves.
const sealKeyFileName = "account-key"

func sealKeyFile() (*creds.Secret, error) {
	dir, err := config.EnsureDataDir()
	if err != nil {
		return nil, err
	}
	return creds.OpenSecret(filepath.Join(dir, sealKeyFileName)), nil
}

// SaveSession stores a session from the PIN step: the token and the seal key.
func SaveSession(s *Session) error {
	f, err := sealKeyFile()
	if err != nil {
		return err
	}
	if err := f.Save(s.SealKey); err != nil {
		return err
	}
	return SaveToken(s.Token)
}

// LoadSealKey reads the seal key saved by SaveSession. A computer signed in
// before PINs has a token and no key; that is ErrPINRequired, because the
// answer is the same as the server's -- set a PIN.
func LoadSealKey() ([]byte, error) {
	f, err := sealKeyFile()
	if err != nil {
		return nil, err
	}
	k, err := f.Load()
	if errors.Is(err, creds.ErrNotFound) {
		return nil, ErrPINRequired
	}
	if err == nil && len(k) != SealKeySize {
		return nil, ErrPINRequired
	}
	return k, err
}

// EmailFromToken reads the subject out of a cached session token.
//
// The token is a JWT whose payload the server signs and this client only
// reads; the signature is the server's business, checked on every request it
// is sent with. This is for display -- the Account screen saying which
// address you are signed in as -- so a payload that does not parse yields ""
// rather than an error, and nothing is trusted on the strength of it.
func EmailFromToken(token string) string {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return ""
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var claims struct {
		Sub string `json:"sub"`
	}
	if err := json.Unmarshal(raw, &claims); err != nil {
		return ""
	}
	return claims.Sub
}
