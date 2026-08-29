package account

import (
	"bytes"
	"errors"
	"testing"

	"golang.org/x/crypto/chacha20poly1305"
)

func TestEncryptDecryptRoundTrip(t *testing.T) {
	plaintext := []byte(`{"account_id":"cbfede9ea66a3477b3dab34db4b21ab8","access_key_id":"AKIAIOSFODNN7EXAMPLE"}`)

	ev, err := Encrypt("correct horse battery staple", plaintext)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}

	got, err := Decrypt("correct horse battery staple", ev)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if !bytes.Equal(got, plaintext) {
		t.Errorf("round trip changed the plaintext:\n got  %q\nwant %q", got, plaintext)
	}
}

func TestDecryptWrongPassphraseFailsCleanly(t *testing.T) {
	plaintext := []byte("super secret r2 credentials")

	ev, err := Encrypt("correct horse battery staple", plaintext)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}

	got, err := Decrypt("wrong passphrase", ev)
	if err == nil {
		t.Fatalf("Decrypt with wrong passphrase succeeded, got plaintext %q", got)
	}
	if !errors.Is(err, ErrDecryptFailed) {
		t.Errorf("Decrypt error = %v, want ErrDecryptFailed", err)
	}
	if got != nil {
		t.Errorf("Decrypt returned non-nil plaintext %q alongside an error", got)
	}
}

func TestEncryptCiphertextDiffersAcrossRuns(t *testing.T) {
	plaintext := []byte("identical input, every time")

	a, err := Encrypt("same passphrase", plaintext)
	if err != nil {
		t.Fatalf("Encrypt (a): %v", err)
	}
	b, err := Encrypt("same passphrase", plaintext)
	if err != nil {
		t.Fatalf("Encrypt (b): %v", err)
	}

	if bytes.Equal(a.Nonce, b.Nonce) {
		t.Errorf("two encryptions produced the same nonce: %x", a.Nonce)
	}
	if bytes.Equal(a.Salt, b.Salt) {
		t.Errorf("two encryptions produced the same salt: %x", a.Salt)
	}
	if bytes.Equal(a.Ciphertext, b.Ciphertext) {
		t.Errorf("two encryptions of identical plaintext produced identical ciphertext")
	}

	// Both must still independently decrypt to the same plaintext -- the
	// point is that an observer can't tell the two blobs hold the same
	// data, not that decryption stops working.
	gotA, err := Decrypt("same passphrase", a)
	if err != nil {
		t.Fatalf("Decrypt(a): %v", err)
	}
	gotB, err := Decrypt("same passphrase", b)
	if err != nil {
		t.Fatalf("Decrypt(b): %v", err)
	}
	if !bytes.Equal(gotA, plaintext) || !bytes.Equal(gotB, plaintext) {
		t.Errorf("decrypted plaintexts diverged from the original")
	}
}

func TestDecryptDetectsTampering(t *testing.T) {
	plaintext := []byte("do not modify this")

	ev, err := Encrypt("a passphrase", plaintext)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}

	tampered := *ev
	tampered.Ciphertext = append([]byte(nil), ev.Ciphertext...)
	tampered.Ciphertext[0] ^= 0x01 // flip a single bit

	if _, err := Decrypt("a passphrase", &tampered); !errors.Is(err, ErrDecryptFailed) {
		t.Errorf("Decrypt of tampered ciphertext = %v, want ErrDecryptFailed", err)
	}

	// Tampering with the nonce must be caught too -- it's authenticated
	// alongside the ciphertext even though it isn't secret.
	tamperedNonce := *ev
	tamperedNonce.Nonce = append([]byte(nil), ev.Nonce...)
	tamperedNonce.Nonce[0] ^= 0x01

	if _, err := Decrypt("a passphrase", &tamperedNonce); !errors.Is(err, ErrDecryptFailed) {
		t.Errorf("Decrypt of tampered nonce = %v, want ErrDecryptFailed", err)
	}
}

// TestKDFParamsRoundTrip builds a vault by hand under KDF parameters that
// are deliberately not DefaultKDFParams(), to prove Decrypt re-derives the
// key using whatever parameters travelled alongside the ciphertext rather
// than assuming today's defaults -- exactly the scenario that matters once
// DefaultKDFParams changes in a future release and an old vault must still
// open.
func TestKDFParamsRoundTrip(t *testing.T) {
	plaintext := []byte("kdf params must travel with the blob")
	params := KDFParams{Time: 2, MemoryKiB: 32 * 1024, Threads: 2, KeyLen: chacha20poly1305.KeySize}
	if params == DefaultKDFParams() {
		t.Fatal("test setup bug: params must differ from DefaultKDFParams to prove anything")
	}

	salt := bytes.Repeat([]byte{0x42}, saltSize)
	key := deriveKey("a passphrase", salt, params)
	if len(key) != int(params.KeyLen) {
		t.Fatalf("deriveKey returned %d bytes, want %d", len(key), params.KeyLen)
	}

	aead, err := chacha20poly1305.NewX(key)
	if err != nil {
		t.Fatalf("NewX: %v", err)
	}
	nonce := bytes.Repeat([]byte{0x24}, aead.NonceSize())
	ciphertext := aead.Seal(nil, nonce, plaintext, nil)

	ev := &EncryptedVault{Salt: salt, Nonce: nonce, Ciphertext: ciphertext, KDFParams: params}

	got, err := Decrypt("a passphrase", ev)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if !bytes.Equal(got, plaintext) {
		t.Errorf("plaintext mismatch after round trip through explicit KDFParams")
	}

	// Decrypting under DefaultKDFParams instead would derive the wrong key
	// entirely and must fail, confirming Decrypt actually used ev.KDFParams
	// rather than silently falling back to the default.
	wrongParams := *ev
	wrongParams.KDFParams = DefaultKDFParams()
	if _, err := Decrypt("a passphrase", &wrongParams); !errors.Is(err, ErrDecryptFailed) {
		t.Errorf("Decrypt with mismatched KDFParams = %v, want ErrDecryptFailed", err)
	}
}
