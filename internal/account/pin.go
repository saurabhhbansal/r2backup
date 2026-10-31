package account

import (
	"context"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"golang.org/x/crypto/argon2"
	"golang.org/x/crypto/chacha20poly1305"
)

// Signing in is email, then the six-digit code sent to it, then a six-digit
// PIN. The PIN does two jobs:
//
//   - It is the second thing the server checks. The code proves the email;
//     the PIN proves it is the person who set the account up.
//   - It locks the storage keys the account keeps. Argon2id stretches the PIN
//     into 64 bytes: the first 32 are the proof sent to the server, the last
//     32 never leave this computer. With the key share the server hands back
//     after a correct PIN, they make the key that seals the storage keys, so
//     the server stores ciphertext it cannot open.
//
// Six digits is a small space. What stops guessing is the server's lockout,
// and the fact that an offline search needs both its database and its
// secrets. See worker/migrations/0004_pin.sql.

// PINLength is how many digits a PIN has.
const PINLength = 6

// ValidPIN reports whether pin is six digits.
func ValidPIN(pin string) error {
	if len(pin) != PINLength {
		return fmt.Errorf("the PIN is %d digits", PINLength)
	}
	for _, r := range pin {
		if r < '0' || r > '9' {
			return fmt.Errorf("the PIN is %d digits, numbers only", PINLength)
		}
	}
	return nil
}

// PINKDFParams are what a new PIN is stretched with. More passes than the
// passphrase vault used, because a PIN has far less to it than a passphrase.
func PINKDFParams() KDFParams {
	return KDFParams{Time: 3, MemoryKiB: 64 * 1024, Threads: 4, KeyLen: 64}
}

var b64 = base64.RawURLEncoding

// SealKeySize is the size of the key that seals the stored keys.
const SealKeySize = chacha20poly1305.KeySize

// derivePIN stretches pin into the proof the server checks and the half of
// the seal key that stays here.
func derivePIN(pin string, salt []byte, p KDFParams) (proof, wrap []byte, err error) {
	if p.KeyLen != 64 {
		return nil, nil, fmt.Errorf("account: PIN parameters ask for a %d-byte key, want 64", p.KeyLen)
	}
	if p.Time == 0 || p.MemoryKiB == 0 || p.Threads == 0 {
		return nil, nil, errors.New("account: PIN parameters are incomplete")
	}
	k := argon2.IDKey([]byte(pin), salt, p.Time, p.MemoryKiB, p.Threads, p.KeyLen)
	return k[:32], k[32:], nil
}

// sealKey joins the PIN's half and the server's share.
func sealKey(wrap, share []byte) ([]byte, error) {
	if len(share) < 32 {
		return nil, errors.New("account: the server's key share is too short")
	}
	ikm := append(append([]byte{}, wrap...), share...)
	return hkdf.Key(sha256.New, ikm, nil, "r2backup sealed keys v1", SealKeySize)
}

// PINChallenge is what a correct email code returns: a short-lived token for
// the PIN step, and how to stretch the PIN when the account already has one.
type PINChallenge struct {
	Email    string
	PINToken string
	// PINSet is false for an account that has never set a PIN -- new, or from
	// before PINs. The next step is then SetupPIN rather than VerifyPIN.
	PINSet bool
	Salt   []byte
	KDF    KDFParams
}

// Session is a signed-in account: the bearer token, and the key that opens
// the storage keys it keeps.
type Session struct {
	Token   string
	SealKey []byte
	// LegacyKeys are keys this account saved before PINs, which the server
	// could read. SetupPIN returns them once; the caller saves them sealed
	// with PutKeys, which is what makes the server delete its readable copy.
	LegacyKeys *StoredKeys
}

// Errors from the PIN step.
var (
	// ErrPINRequired means the account has no PIN yet. Every request but
	// SetupPIN is refused until it has one.
	ErrPINRequired = errors.New("account: a PIN needs to be set first")
	// ErrPINAlreadySet means SetupPIN was asked for an account with a PIN.
	ErrPINAlreadySet = errors.New("account: this account already has a PIN")
)

// WrongPINError is a PIN that did not match.
type WrongPINError struct{ TriesLeft int }

func (e *WrongPINError) Error() string {
	if e.TriesLeft == 1 {
		return "that PIN is not right. One more wrong try locks the account for a while"
	}
	return fmt.Sprintf("that PIN is not right. %d tries left", e.TriesLeft)
}

// PINLockedError is an account locked after too many wrong PINs.
type PINLockedError struct{ RetryAfterSeconds int }

func (e *PINLockedError) Error() string {
	m := (e.RetryAfterSeconds + 59) / 60
	if m <= 1 {
		return "too many wrong PINs. Try again in a minute"
	}
	return fmt.Sprintf("too many wrong PINs. Try again in %d minutes", m)
}

// Verify exchanges an email and the code sent to it for the PIN step. The
// code is single-use either way.
func (c *Client) Verify(ctx context.Context, email, code string) (*PINChallenge, error) {
	var out struct {
		PINToken  string          `json:"pin_token"`
		PINSet    bool            `json:"pin_set"`
		Salt      string          `json:"salt"`
		KDFParams json.RawMessage `json:"kdf_params"`
	}
	if _, err := c.do(ctx, http.MethodPost, "/auth/verify", "", map[string]string{"email": email, "code": code}, &out); err != nil {
		return nil, err
	}
	if out.PINToken == "" {
		return nil, fmt.Errorf("%w: no pin_token", ErrBadResponse)
	}
	ch := &PINChallenge{Email: email, PINToken: out.PINToken, PINSet: out.PINSet}
	if out.PINSet {
		salt, err := b64.DecodeString(out.Salt)
		if err != nil {
			return nil, fmt.Errorf("%w: salt: %v", ErrBadResponse, err)
		}
		if err := json.Unmarshal(out.KDFParams, &ch.KDF); err != nil {
			return nil, fmt.Errorf("%w: kdf_params: %v", ErrBadResponse, err)
		}
		ch.Salt = salt
	}
	return ch, nil
}

type sessionWire struct {
	Token      string      `json:"token"`
	KeyShare   string      `json:"key_share"`
	LegacyKeys *StoredKeys `json:"legacy_keys"`
}

func (w sessionWire) session(wrap []byte) (*Session, error) {
	share, err := b64.DecodeString(w.KeyShare)
	if err != nil || w.Token == "" {
		return nil, fmt.Errorf("%w: session", ErrBadResponse)
	}
	key, err := sealKey(wrap, share)
	if err != nil {
		return nil, err
	}
	return &Session{Token: w.Token, SealKey: key, LegacyKeys: w.LegacyKeys}, nil
}

// VerifyPIN checks pin against an account that has one.
func (c *Client) VerifyPIN(ctx context.Context, ch *PINChallenge, pin string) (*Session, error) {
	if err := ValidPIN(pin); err != nil {
		return nil, err
	}
	proof, wrap, err := derivePIN(pin, ch.Salt, ch.KDF)
	if err != nil {
		return nil, err
	}
	var out sessionWire
	if _, err := c.do(ctx, http.MethodPost, "/auth/pin", ch.PINToken, map[string]string{"proof": b64.EncodeToString(proof)}, &out); err != nil {
		return nil, err
	}
	return out.session(wrap)
}

func (c *Client) newPIN(ctx context.Context, path, token, pin string) (*Session, error) {
	if err := ValidPIN(pin); err != nil {
		return nil, err
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return nil, err
	}
	params := PINKDFParams()
	proof, wrap, err := derivePIN(pin, salt, params)
	if err != nil {
		return nil, err
	}
	body := map[string]any{"salt": b64.EncodeToString(salt), "kdf_params": params, "proof": b64.EncodeToString(proof)}
	var out sessionWire
	if _, err := c.do(ctx, http.MethodPost, path, token, body, &out); err != nil {
		return nil, err
	}
	return out.session(wrap)
}

// SetupPIN sets the first PIN on an account. token is the PIN-step token from
// Verify, or a session saved before PINs existed.
func (c *Client) SetupPIN(ctx context.Context, token, pin string) (*Session, error) {
	return c.newPIN(ctx, "/auth/pin/setup", token, pin)
}

// ResetPIN replaces a forgotten PIN. It needs the PIN-step token from a fresh
// email code. The keys the account kept are deleted -- they were locked with
// the old PIN -- and every other computer is signed out.
func (c *Client) ResetPIN(ctx context.Context, pinToken, pin string) (*Session, error) {
	return c.newPIN(ctx, "/auth/pin/reset", pinToken, pin)
}

// keysAD binds a sealed blob to its purpose.
var keysAD = []byte("r2backup stored keys v1")

// GetKeys fetches and opens the keys kept for the account. ErrNotFound means
// none have been saved yet; ErrDecryptFailed means they were sealed with a
// key this one is not (a PIN reset elsewhere since).
func (c *Client) GetKeys(ctx context.Context, token string, key []byte) (*StoredKeys, error) {
	var out struct {
		Sealed struct {
			Ciphertext string `json:"ciphertext"`
			Nonce      string `json:"nonce"`
		} `json:"sealed"`
	}
	if _, err := c.do(ctx, http.MethodGet, "/keys", token, nil, &out); err != nil {
		return nil, err
	}
	ct, err1 := b64.DecodeString(out.Sealed.Ciphertext)
	nonce, err2 := b64.DecodeString(out.Sealed.Nonce)
	if err1 != nil || err2 != nil {
		return nil, fmt.Errorf("%w: sealed keys", ErrBadResponse)
	}
	aead, err := chacha20poly1305.NewX(key)
	if err != nil {
		return nil, err
	}
	if len(nonce) != aead.NonceSize() {
		return nil, ErrDecryptFailed
	}
	plain, err := aead.Open(nil, nonce, ct, keysAD)
	if err != nil {
		return nil, ErrDecryptFailed
	}
	var k StoredKeys
	if err := json.Unmarshal(plain, &k); err != nil {
		return nil, fmt.Errorf("account: the saved keys could not be read: %w", err)
	}
	return &k, nil
}

// PutKeys seals k with key and replaces the keys kept for the account.
func (c *Client) PutKeys(ctx context.Context, token string, key []byte, k StoredKeys) error {
	plain, err := json.Marshal(k)
	if err != nil {
		return err
	}
	aead, err := chacha20poly1305.NewX(key)
	if err != nil {
		return err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return err
	}
	ct := aead.Seal(nil, nonce, plain, keysAD)
	body := map[string]any{"sealed": map[string]string{"ciphertext": b64.EncodeToString(ct), "nonce": b64.EncodeToString(nonce)}}
	_, err = c.do(ctx, http.MethodPut, "/keys", token, body, nil)
	return err
}

// DeleteKeys forgets the keys kept for the account.
func (c *Client) DeleteKeys(ctx context.Context, token string) error {
	_, err := c.do(ctx, http.MethodDelete, "/keys", token, nil, nil)
	return err
}
