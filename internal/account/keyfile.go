package account

import (
	"encoding/json"
	"errors"
	"fmt"
)

// A key file carries this computer's storage keys to another computer without
// any account: the person moves the file themselves (a USB stick, a message
// to themselves) and types the passphrase on the other side. It is the same
// Argon2id and XChaCha20-Poly1305 the vault used, under a passphrase chosen at
// export.

// keyFileFormat names the file, so importing something else fails plainly.
const keyFileFormat = "r2backup-keys"

// MinKeyFilePassphrase is the shortest passphrase a key file takes. The file
// may sit in a downloads folder or a chat history, so it gets more than a PIN.
const MinKeyFilePassphrase = 8

type keyFile struct {
	Format  string          `json:"format"`
	Version int             `json:"version"`
	Vault   *EncryptedVault `json:"vault"`
}

// ErrNotKeyFile is a file that is not an r2backup key file.
var ErrNotKeyFile = errors.New("this is not an r2backup key file")

// ExportKeys seals k under passphrase as a key file.
func ExportKeys(passphrase string, k StoredKeys) ([]byte, error) {
	if len(passphrase) < MinKeyFilePassphrase {
		return nil, fmt.Errorf("use a passphrase of at least %d characters", MinKeyFilePassphrase)
	}
	if k.Primary == nil && k.Second == nil && k.Source == nil {
		return nil, errors.New("there are no storage keys on this computer to export")
	}
	plain, err := json.Marshal(k)
	if err != nil {
		return nil, err
	}
	v, err := Encrypt(passphrase, plain)
	if err != nil {
		return nil, err
	}
	return json.MarshalIndent(keyFile{Format: keyFileFormat, Version: 1, Vault: v}, "", "  ")
}

// ImportKeys opens a key file. A wrong passphrase is ErrDecryptFailed.
func ImportKeys(passphrase string, data []byte) (*StoredKeys, error) {
	var f keyFile
	if err := json.Unmarshal(data, &f); err != nil || f.Format != keyFileFormat || f.Vault == nil {
		return nil, ErrNotKeyFile
	}
	if f.Version != 1 {
		return nil, fmt.Errorf("this key file was made by a newer r2backup (version %d). Update and try again", f.Version)
	}
	plain, err := Decrypt(passphrase, f.Vault)
	if err != nil {
		return nil, err
	}
	var k StoredKeys
	if err := json.Unmarshal(plain, &k); err != nil {
		return nil, fmt.Errorf("the key file could not be read: %w", err)
	}
	return &k, nil
}
