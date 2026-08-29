package account

import (
	"bytes"
	"encoding/json"
	"testing"
)

// TestVaultSurvivesTheWireNotJustMemory is the test that was missing.
//
// Encrypt and Decrypt round-tripped perfectly in memory, and the server's own
// tests used a fake that echoed back whatever shape it was handed. Neither
// crossed the boundary where the vault is serialised, stored under a fixed set
// of columns, and read back. The salt was being dropped there -- and a vault
// without its salt cannot be opened by anyone, ever, including the person who
// wrote it.
//
// Marshalling through JSON and decrypting the result is what catches a field
// that quietly fails to make the trip.
func TestVaultSurvivesTheWireNotJustMemory(t *testing.T) {
	const passphrase = "correct horse battery staple"
	secret := []byte(`{"account_id":"abc","access_key_id":"key","secret_access_key":"shh","bucket":"b"}`)

	sealed, err := Encrypt(passphrase, secret)
	if err != nil {
		t.Fatal(err)
	}

	wire, err := json.Marshal(sealed)
	if err != nil {
		t.Fatal(err)
	}

	// Every field the client needs must actually appear on the wire. Checking
	// the decoded struct alone would not catch a field the server never
	// persists, so assert on the JSON itself.
	var asMap map[string]json.RawMessage
	if err := json.Unmarshal(wire, &asMap); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"salt", "nonce", "ciphertext", "kdf_params"} {
		if _, ok := asMap[field]; !ok {
			t.Errorf("%q is missing from the serialised vault; a server storing only the fields it sees would drop it", field)
		}
	}

	var back EncryptedVault
	if err := json.Unmarshal(wire, &back); err != nil {
		t.Fatal(err)
	}
	got, err := Decrypt(passphrase, &back)
	if err != nil {
		t.Fatalf("a vault that went through JSON could not be decrypted: %v", err)
	}
	if !bytes.Equal(got, secret) {
		t.Fatalf("round trip changed the plaintext:\n got %s\nwant %s", got, secret)
	}
}

// TestAVaultMissingItsSaltFailsLoudly proves the failure mode is a clear error
// rather than silently derived garbage.
func TestAVaultMissingItsSaltFailsLoudly(t *testing.T) {
	sealed, err := Encrypt("passphrase", []byte("secret"))
	if err != nil {
		t.Fatal(err)
	}
	sealed.Salt = nil
	if _, err := Decrypt("passphrase", sealed); err == nil {
		t.Fatal("a vault with no salt decrypted successfully, which should be impossible")
	}
}
