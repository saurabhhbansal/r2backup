package cli

import (
	"encoding/base64"
	"testing"
	"time"

	"github.com/saurabhhbansal/r2backup/internal/account"
	"github.com/saurabhhbansal/r2backup/internal/config"
	"github.com/saurabhhbansal/r2backup/internal/creds"
)

func fakeToken(payload string) string {
	return "h." + base64.RawURLEncoding.EncodeToString([]byte(payload)) + ".s"
}

func TestTokenClaims(t *testing.T) {
	email, exp := tokenClaims(fakeToken(`{"sub":"a@b.com","exp":2000000000}`))
	if email != "a@b.com" || exp.Unix() != 2000000000 {
		t.Fatalf("got %q %v", email, exp)
	}
	if e, _ := tokenClaims("not-a-token"); e != "" {
		t.Fatalf("garbage gave %q", e)
	}
}

func TestSignedInHereReadsTheSavedSession(t *testing.T) {
	t.Setenv(config.EnvDataDir, t.TempDir())
	now := time.Unix(1_900_000_000, 0)
	if _, ok := signedInHere(now); ok {
		t.Fatal("signed in with no session saved")
	}
	if err := account.SaveToken(fakeToken(`{"sub":"a@b.com","exp":1950000000}`)); err != nil {
		t.Fatal(err)
	}
	if email, ok := signedInHere(now); !ok || email != "a@b.com" {
		t.Fatalf("got %q %v", email, ok)
	}
	if _, ok := signedInHere(time.Unix(1_960_000_000, 0)); ok {
		t.Fatal("an expired session still counts as signed in")
	}
}

func TestDescribeStoreNeverShowsTheKey(t *testing.T) {
	d := describeStore(creds.Credentials{Provider: creds.ProviderDrime, DrimeToken: "secret-token-3f2a"})
	if d.Provider != "drime" || d.Hint != "Folder r2backup · token ••••3f2a" {
		t.Fatalf("drime: %+v", d)
	}
	r := describeStore(creds.Credentials{AccountID: "acc", AccessKeyID: "AKIAXYZQ7XZ", SecretAccessKey: "very-secret", Bucket: "b"})
	if r.Provider != "r2" || r.Hint != "Bucket b · key ••••Q7XZ" {
		t.Fatalf("r2: %+v", r)
	}
	if masked("abc") != "••••" {
		t.Fatal("a short key is shown whole")
	}
}
