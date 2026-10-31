// Package creds stores the R2 credentials this machine uses.
//
// They have to be readable without a person present. A scheduled backup runs
// at 3am with nobody at the keyboard, so anything that needs a passphrase
// typed at run time cannot be the storage for these -- it would mean the
// unattended backups the product exists to provide simply never happen.
//
// So the protection is the operating system's where one is wired up, and
// honest about its absence where it is not. On Windows, DPAPI seals the file
// to the logged-in user and nobody else. Everywhere else -- macOS and Linux
// both, for now -- there is no OS keystore integration yet, so the file is
// written 0600 in a 0700 directory and Protection() reports false, so the
// caller can say so plainly rather than implying a protection that is not
// there. Keychain and libsecret backends would close that gap; neither
// exists today.
package creds

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Provider values. Empty is R2: every credentials file written before Drime
// support existed has no provider field, and must keep meaning what it meant.
const (
	ProviderR2    = "r2"
	ProviderDrime = "drime"
)

// Credentials are what is needed to reach a bucket.
type Credentials struct {
	// Provider says which kind of storage these are for: ProviderR2 (or
	// empty) or ProviderDrime.
	Provider string `json:"provider,omitempty"`

	AccountID       string `json:"account_id"`
	AccessKeyID     string `json:"access_key_id"`
	SecretAccessKey string `json:"secret_access_key"`
	Bucket          string `json:"bucket"`
	// Endpoint overrides the derived R2 endpoint, or Drime's base URL. Tests
	// point it at a local server; users never set it.
	Endpoint string `json:"endpoint,omitempty"`

	// DrimeToken is a Drime API token (Settings > Developer in Drime).
	DrimeToken string `json:"drime_token,omitempty"`
	// DrimeWorkspace picks a shared Drime workspace; empty is the personal
	// drive.
	DrimeWorkspace string `json:"drime_workspace,omitempty"`
	// DrimeRoot is the folder in Drime that backups go under; empty is
	// "r2backup".
	DrimeRoot string `json:"drime_root,omitempty"`
}

// IsDrime reports whether these credentials are for Drime.
func (c Credentials) IsDrime() bool { return c.Provider == ProviderDrime }

// Valid reports whether these credentials are complete enough to try.
func (c Credentials) Valid() error {
	if c.IsDrime() {
		if strings.TrimSpace(c.DrimeToken) == "" {
			return errors.New("incomplete credentials: missing Drime API token")
		}
		return nil
	}
	if c.Provider != "" && c.Provider != ProviderR2 {
		return fmt.Errorf("unknown storage provider %q", c.Provider)
	}
	var missing []string
	if c.AccountID == "" && c.Endpoint == "" {
		missing = append(missing, "account id")
	}
	if c.AccessKeyID == "" {
		missing = append(missing, "access key id")
	}
	if c.SecretAccessKey == "" {
		missing = append(missing, "secret access key")
	}
	if c.Bucket == "" {
		missing = append(missing, "bucket")
	}
	if len(missing) > 0 {
		return fmt.Errorf("incomplete credentials: missing %s", strings.Join(missing, ", "))
	}
	return nil
}

// Redacted returns a copy safe to print or log. The secret is the one field
// that must never reach a terminal, a log file or a bug report.
func (c Credentials) Redacted() Credentials {
	c.SecretAccessKey = mask(c.SecretAccessKey)
	c.DrimeToken = mask(c.DrimeToken)
	return c
}

func mask(s string) string {
	if s == "" {
		return ""
	}
	if len(s) <= 4 {
		return strings.Repeat("*", len(s))
	}
	return strings.Repeat("*", len(s)-4) + s[len(s)-4:]
}

// ErrNotFound means this machine has no credentials stored.
var ErrNotFound = errors.New("no credentials stored on this machine")

// Store reads and writes the credentials for one machine.
type Store struct {
	// path is the file used when the OS keystore is unavailable, and also the
	// container for the OS-protected blob where one is.
	path string
	// protector encrypts and decrypts. Never nil; see newProtector.
	protector protector
}

// protector is the per-platform secret protection. Implementations live in
// creds_windows.go, creds_darwin.go and creds_unix.go.
type protector interface {
	protect(plaintext []byte) ([]byte, error)
	unprotect(ciphertext []byte) ([]byte, error)
	// name is what to tell the user is guarding their secret.
	name() string
	// protected reports whether this is real protection or just file
	// permissions.
	protected() bool
}

// Open returns the store at path.
func Open(path string) *Store {
	return &Store{path: path, protector: newProtector()}
}

// Protection names what is guarding the secret, and whether that is more than
// file permissions.
func (s *Store) Protection() (string, bool) {
	return s.protector.name(), s.protector.protected()
}

// Save writes credentials for this machine.
func (s *Store) Save(c Credentials) error {
	if err := c.Valid(); err != nil {
		return err
	}
	plain, err := json.Marshal(c)
	if err != nil {
		return fmt.Errorf("encode credentials: %w", err)
	}
	return writeSealed(s.path, s.protector, plain)
}

// writeSealed protects plain and writes it to path, atomically and never
// readable by anyone else, not even for an instant.
func writeSealed(path string, p protector, plain []byte) error {
	sealed, err := p.protect(plain)
	if err != nil {
		return fmt.Errorf("protect secret with %s: %w", p.name(), err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create directory for %q: %w", path, err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".creds-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp file beside %q: %w", path, err)
	}
	name := tmp.Name()
	defer os.Remove(name)
	// 0600 before anything is written, not after: a secret must never exist
	// on disk as world-readable, not even for the instant between.
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("restrict permissions on %q: %w", name, err)
	}
	if _, err := tmp.Write(sealed); err != nil {
		tmp.Close()
		return fmt.Errorf("write %q: %w", name, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close %q: %w", name, err)
	}
	if err := os.Rename(name, path); err != nil {
		return fmt.Errorf("replace %q: %w", path, err)
	}
	return nil
}

// Load reads this machine's credentials.
func (s *Store) Load() (Credentials, error) {
	sealed, err := os.ReadFile(s.path)
	if os.IsNotExist(err) {
		return Credentials{}, ErrNotFound
	}
	if err != nil {
		return Credentials{}, fmt.Errorf("read %q: %w", s.path, err)
	}
	if len(sealed) == 0 {
		return Credentials{}, ErrNotFound
	}
	plain, err := s.protector.unprotect(sealed)
	if err != nil {
		return Credentials{}, fmt.Errorf("unlock credentials with %s: %w", s.protector.name(), err)
	}
	var c Credentials
	if err := json.Unmarshal(plain, &c); err != nil {
		return Credentials{}, fmt.Errorf("decode credentials from %q: %w", s.path, err)
	}
	return c, nil
}

// Delete forgets this machine's credentials. It does not touch the bucket.
func (s *Store) Delete() error {
	err := os.Remove(s.path)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("delete %q: %w", s.path, err)
	}
	return nil
}

// Exists reports whether anything is stored.
func (s *Store) Exists() bool {
	info, err := os.Stat(s.path)
	return err == nil && info.Size() > 0
}

// Secret is one small value kept under the same protection as the
// credentials: the key that opens the keys an account keeps, for one.
type Secret struct {
	path      string
	protector protector
}

// OpenSecret returns the secret stored at path.
func OpenSecret(path string) *Secret {
	return &Secret{path: path, protector: newProtector()}
}

// Save replaces the secret.
func (s *Secret) Save(b []byte) error { return writeSealed(s.path, s.protector, b) }

// Load reads the secret. ErrNotFound means none is stored.
func (s *Secret) Load() ([]byte, error) {
	sealed, err := os.ReadFile(s.path)
	if os.IsNotExist(err) || (err == nil && len(sealed) == 0) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("read %q: %w", s.path, err)
	}
	plain, err := s.protector.unprotect(sealed)
	if err != nil {
		return nil, fmt.Errorf("unlock %q with %s: %w", s.path, s.protector.name(), err)
	}
	return plain, nil
}

// Delete forgets the secret.
func (s *Secret) Delete() error {
	if err := os.Remove(s.path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("delete %q: %w", s.path, err)
	}
	return nil
}
