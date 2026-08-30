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
	"encoding/json"
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

// UnmarshalJSON accepts kdf_params as either a JSON object -- the shape the
// account Worker sends today -- or a JSON string containing one, which is
// what GET /vault used to send: the Worker stores kdf_params re-serialised
// as a D1 string column (see the comment in worker/src/handlers/vault.ts on
// why) and, for a while, handed that stored string back on the wire
// unparsed instead of decoding it first. That has been fixed server-side,
// but this client cannot assume the fix has reached whatever server it
// happens to be talking to: a Worker deploy and a client's own update run
// on entirely separate schedules, and a copy of this binary built before
// today has no way to know the server-side bug even existed, let alone that
// it was fixed. Tolerating both shapes here means a stale server, an old
// vault row nobody has rewritten, and a client that hasn't been updated yet
// can all still land on a KDFParams that decrypts, instead of failing with
// a raw JSON decode error at the one moment -- signing in -- where a person
// has no way to act on it.
func (k *KDFParams) UnmarshalJSON(data []byte) error {
	// A local named type, not KDFParams itself, so this doesn't recurse into
	// the very UnmarshalJSON being defined.
	type plain KDFParams

	var direct plain
	if err := json.Unmarshal(data, &direct); err == nil {
		*k = KDFParams(direct)
		return nil
	}

	// Not an object. The only other shape worth trying is a string that
	// itself holds one -- anything else is just a malformed response, and
	// that should fail exactly as loudly as it would have before this
	// method existed.
	var asString string
	if err := json.Unmarshal(data, &asString); err != nil {
		return fmt.Errorf("account: kdf_params is neither an object nor a string: %w", err)
	}
	var nested plain
	if err := json.Unmarshal([]byte(asString), &nested); err != nil {
		return fmt.Errorf("account: kdf_params string does not contain a valid object: %w", err)
	}
	*k = KDFParams(nested)
	return nil
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
