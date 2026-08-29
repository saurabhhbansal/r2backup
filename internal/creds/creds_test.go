package creds

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func store(t *testing.T) *Store {
	t.Helper()
	return Open(filepath.Join(t.TempDir(), "creds"))
}

func sample() Credentials {
	return Credentials{
		AccountID:       "cbfede9ea66a3477b3dab34db4b21ab8",
		AccessKeyID:     "AKIAIOSFODNN7EXAMPLE",
		SecretAccessKey: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
		Bucket:          "cf-backup",
	}
}

func TestRoundTrip(t *testing.T) {
	s := store(t)
	want := sample()
	if err := s.Save(want); err != nil {
		t.Fatal(err)
	}
	got, err := s.Load()
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Errorf("round trip changed the credentials:\n got %+v\nwant %+v", got.Redacted(), want.Redacted())
	}
}

func TestLoadWithNothingStored(t *testing.T) {
	if _, err := store(t).Load(); !errors.Is(err, ErrNotFound) {
		t.Fatalf("got %v, want ErrNotFound", err)
	}
}

func TestFileIsNotReadableByOthers(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix permission bits do not apply on Windows, where DPAPI does the protecting")
	}
	s := store(t)
	if err := s.Save(sample()); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(s.path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("credentials file is mode %o, want 0600 -- a secret must not be readable by other users", perm)
	}
}

func TestSaveLeavesNoTempFileBehind(t *testing.T) {
	// A temp file holding the secret, left in place, is the same leak as a
	// world-readable one.
	s := store(t)
	if err := s.Save(sample()); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Dir(s.path))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("expected only the credentials file, found %v", names)
	}
}

func TestRedactedNeverCarriesTheSecret(t *testing.T) {
	c := sample()
	r := c.Redacted()
	if strings.Contains(r.SecretAccessKey, "wJalrXUtnFEMI") {
		t.Fatalf("Redacted() still contains the secret: %q", r.SecretAccessKey)
	}
	if !strings.HasSuffix(r.SecretAccessKey, "EKEY") {
		t.Errorf("Redacted() should keep the last four characters so a user can tell which key it is, got %q", r.SecretAccessKey)
	}
	// The other fields are not secret and must survive, or the user cannot
	// tell which account they are looking at.
	if r.AccountID != c.AccountID || r.Bucket != c.Bucket || r.AccessKeyID != c.AccessKeyID {
		t.Error("Redacted() removed a field that is not secret")
	}
}

func TestMaskHandlesShortAndEmptySecrets(t *testing.T) {
	cases := map[string]string{
		"":       "",
		"a":      "*",
		"abcd":   "****",
		"abcde":  "*bcde",
		"secret": "**cret",
	}
	for in, want := range cases {
		if got := mask(in); got != want {
			t.Errorf("mask(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestValidNamesEveryMissingField(t *testing.T) {
	err := Credentials{}.Valid()
	if err == nil {
		t.Fatal("empty credentials should not be valid")
	}
	for _, want := range []string{"account id", "access key id", "secret access key", "bucket"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error should name the missing %q; got %v", want, err)
		}
	}
}

func TestEndpointSubstitutesForAccountID(t *testing.T) {
	// Tests point at a local server and have no account id; that is legal.
	c := Credentials{Endpoint: "http://127.0.0.1:9000", AccessKeyID: "a", SecretAccessKey: "b", Bucket: "c"}
	if err := c.Valid(); err != nil {
		t.Fatalf("an explicit endpoint should stand in for an account id: %v", err)
	}
}

func TestSaveRefusesIncompleteCredentials(t *testing.T) {
	// Writing half a credential set produces a store that fails later, at
	// 3am, with a worse error than the one available right now.
	if err := store(t).Save(Credentials{AccountID: "x"}); err == nil {
		t.Fatal("Save accepted incomplete credentials")
	}
}

func TestDeleteIsIdempotentAndExistsTracksIt(t *testing.T) {
	s := store(t)
	if s.Exists() {
		t.Error("a fresh store should hold nothing")
	}
	if err := s.Save(sample()); err != nil {
		t.Fatal(err)
	}
	if !s.Exists() {
		t.Error("Exists() is false after a successful Save")
	}
	if err := s.Delete(); err != nil {
		t.Fatal(err)
	}
	if s.Exists() {
		t.Error("Exists() is true after Delete")
	}
	if err := s.Delete(); err != nil {
		t.Fatalf("deleting an absent store should not error: %v", err)
	}
}

func TestProtectionIsDescribedHonestly(t *testing.T) {
	// The user is told what is actually guarding the key. Claiming encryption
	// that is not happening would be worse than admitting file permissions.
	name, protected := store(t).Protection()
	if name == "" {
		t.Fatal("Protection() must name something")
	}
	if runtime.GOOS == "windows" && !protected {
		t.Error("Windows should report real protection via DPAPI")
	}
	if runtime.GOOS != "windows" && protected {
		t.Error("without a keystore backend this must report false, not imply encryption")
	}
}
