// Package account implements the cross-device account system: a local vault
// that encrypts this machine's R2 credentials under a passphrase, and a
// client for the account Worker that lets a second computer fetch that
// vault after proving it owns the same email address.
//
// # What losing the passphrase costs
//
// Forgetting the vault passphrase is not a data-loss event. It protects the
// *stored R2 credentials* on the account server, not the user's files: this
// package encrypts nothing on the backup path, so every backed-up byte
// stays exactly as readable -- through R2 directly, or by re-entering the
// access keys on this computer -- as it would have been with no vault at
// all. Forgetting the passphrase costs one trip back to the R2 dashboard to
// copy the access keys again; it costs no backed-up data.
package account

import (
	"crypto/rand"
	"errors"
	"fmt"

	"golang.org/x/crypto/argon2"
	"golang.org/x/crypto/chacha20poly1305"
)

// KDFParams records exactly how a key was derived from a passphrase, stored
// alongside the ciphertext it protects.
//
// Argon2id's recommended cost parameters change over time as hardware gets
// faster; a vault encrypted today must still decrypt after those
// recommendations move. Carrying the parameters actually used to derive the
// key, rather than assuming today's defaults, is what makes that possible:
// Decrypt always re-derives the key the same way Encrypt did, regardless of
// what DefaultKDFParams returns by the time it runs.
type KDFParams struct {
	// Time is the number of Argon2id passes.
	Time uint32 `json:"time"`
	// MemoryKiB is the memory cost in kibibytes.
	MemoryKiB uint32 `json:"memory_kib"`
	// Threads is the degree of parallelism.
	Threads uint8 `json:"threads"`
	// KeyLen is the derived key length in bytes (32 for XChaCha20-Poly1305).
	KeyLen uint32 `json:"key_len"`
}

// DefaultKDFParams returns the parameters new vaults are encrypted with.
// Chosen per the current OWASP/RFC 9106 guidance for argon2id run
// interactively (a user waiting on a passphrase prompt, not a background
// job): 64 MiB, 1 pass, 4 lanes.
func DefaultKDFParams() KDFParams {
	return KDFParams{
		Time:      1,
		MemoryKiB: 64 * 1024,
		Threads:   4,
		KeyLen:    chacha20poly1305.KeySize,
	}
}

// EncryptedVault is everything needed to decrypt a vault given the right
// passphrase, and nothing else -- this is exactly the blob that leaves this
// machine and is handed to the account server, which stores it and can
// never read it.
type EncryptedVault struct {
	Salt       []byte    `json:"salt"`
	Nonce      []byte    `json:"nonce"`
	Ciphertext []byte    `json:"ciphertext"`
	KDFParams  KDFParams `json:"kdf_params"`
}

const saltSize = 16

// deriveKey runs argon2id with the given parameters. Pulled out on its own
// because both Encrypt (with DefaultKDFParams) and Decrypt (with whatever
// params travelled alongside the ciphertext) need exactly this, and using
// two different code paths for the "same" derivation is how they'd quietly
// drift apart.
func deriveKey(passphrase string, salt []byte, params KDFParams) []byte {
	return argon2.IDKey([]byte(passphrase), salt, params.Time, params.MemoryKiB, params.Threads, params.KeyLen)
}

// Encrypt derives a key from passphrase with argon2id under a fresh random
// salt, then seals plaintext with XChaCha20-Poly1305 under a fresh random
// nonce. plaintext is expected to be the credentials JSON (see
// internal/creds.Credentials), but Encrypt does not parse or care about its
// shape -- it seals whatever bytes it is given.
//
// Called twice with the same passphrase and plaintext, Encrypt returns two
// different ciphertexts: the nonce (and salt) are drawn fresh from
// crypto/rand every time, which is what stops an observer who sees two
// vault blobs on the wire from ever telling whether they hold the same
// credentials.
func Encrypt(passphrase string, plaintext []byte) (*EncryptedVault, error) {
	params := DefaultKDFParams()

	salt := make([]byte, saltSize)
	if _, err := rand.Read(salt); err != nil {
		return nil, fmt.Errorf("generate salt: %w", err)
	}

	key := deriveKey(passphrase, salt, params)

	aead, err := chacha20poly1305.NewX(key)
	if err != nil {
		return nil, fmt.Errorf("init cipher: %w", err)
	}

	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("generate nonce: %w", err)
	}

	ciphertext := aead.Seal(nil, nonce, plaintext, nil)

	return &EncryptedVault{
		Salt:       salt,
		Nonce:      nonce,
		Ciphertext: ciphertext,
		KDFParams:  params,
	}, nil
}

// ErrDecryptFailed covers both a wrong passphrase and a tampered blob: the
// AEAD tag check is what fails in either case, and it fails the same way
// for both, on purpose. Reporting anything more specific ("the passphrase
// is wrong" vs. "the data is corrupt") would tell an attacker who has
// modified a stored blob whether their edit was detected as tampering or
// merely decrypted under the wrong guess, which is exactly the kind of
// oracle an AEAD exists to deny them.
var ErrDecryptFailed = errors.New("account: decryption failed (wrong passphrase or corrupted vault)")

// Decrypt reverses Encrypt: re-derive the key from passphrase using the
// EncryptedVault's own KDFParams (never DefaultKDFParams -- an older vault
// may have been sealed under different cost parameters), then open the
// AEAD. A wrong passphrase or a single flipped byte anywhere in the blob
// both surface as ErrDecryptFailed; there is no partial or "garbage"
// result, because a XChaCha20-Poly1305 authentication failure refuses to
// produce plaintext at all.
func Decrypt(passphrase string, ev *EncryptedVault) ([]byte, error) {
	if ev == nil {
		return nil, fmt.Errorf("account: nil vault")
	}

	key := deriveKey(passphrase, ev.Salt, ev.KDFParams)

	aead, err := chacha20poly1305.NewX(key)
	if err != nil {
		return nil, fmt.Errorf("init cipher: %w", err)
	}

	plaintext, err := aead.Open(nil, ev.Nonce, ev.Ciphertext, nil)
	if err != nil {
		return nil, ErrDecryptFailed
	}
	return plaintext, nil
}

// EncryptedVault's []byte fields round-trip through encoding/json as
// base64 automatically -- a caller wiring this into an HTTP request body
// (see Client.PutVault) can json.Marshal it directly with no extra
// encoding step.
