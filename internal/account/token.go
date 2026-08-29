package account

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/saurabhhbansal/r2backup/internal/config"
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

// ClearToken removes the cached token, e.g. on sign-out. Removing an
// already-absent file is not an error, for the same reason a missing file
// isn't one in LoadToken.
func ClearToken() error {
	path, err := TokenPath()
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("account: clear token: %w", err)
	}
	return nil
}
