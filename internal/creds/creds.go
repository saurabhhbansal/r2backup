// Package creds stores the R2 credentials this machine uses.
//
// They have to be readable without a person present. A scheduled backup runs
// at 3am with nobody at the keyboard, so anything that needs a passphrase
// typed at run time cannot be the storage for these -- it would mean the
// unattended backups the product exists to provide simply never happen.
//
// So the protection is the operating system's: DPAPI on Windows, the Keychain
// on macOS, libsecret on Linux, each of which unlocks for the logged-in user
// and for nobody else. Where none is available the file is written 0600 and
// Protected() reports false, so the caller can say so plainly rather than
// implying a protection that is not there.
package creds

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Credentials are what is needed to reach a bucket.
type Credentials struct {
	AccountID       string `json:"account_id"`
	AccessKeyID     string `json:"access_key_id"`
	SecretAccessKey string `json:"secret_access_key"`
	Bucket          string `json:"bucket"`
	// Endpoint overrides the derived R2 endpoint. Tests point it at a local
	// server; users never set it.
	Endpoint string `json:"endpoint,omitempty"`
}

// Valid reports whether these credentials are complete enough to try.
func (c Credentials) Valid() error {
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
	sealed, err := s.protector.protect(plain)
	if err != nil {
		return fmt.Errorf("protect credentials with %s: %w", s.protector.name(), err)
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return fmt.Errorf("create directory for %q: %w", s.path, err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".creds-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp file beside %q: %w", s.path, err)
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
	if err := os.Rename(name, s.path); err != nil {
		return fmt.Errorf("replace %q: %w", s.path, err)
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
