package cli

import (
	"context"
	"strings"
	"testing"

	"github.com/saurabhhbansal/r2backup/internal/creds"
)

// Picking Drime asks for one token and nothing else -- no account id, no
// bucket, no key pair, and no browser sign-in to Cloudflare.
func TestSetupDrimeAsksOnlyForToken(t *testing.T) {
	var out strings.Builder
	in := strings.NewReader("1\n  tok-123  \n")
	p := newPrompter(&Options{Out: &out, In: in})
	c, err := askForDestination(context.Background(), p, "")
	if err != nil {
		t.Fatalf("askForDestination: %v", err)
	}
	if !c.IsDrime() || c.DrimeToken != "tok-123" {
		t.Fatalf("got %+v", c.Redacted())
	}
	if err := c.Valid(); err != nil {
		t.Fatalf("Drime credentials not valid: %v", err)
	}
	if strings.Contains(out.String(), "Cloudflare account ID") {
		t.Fatalf("asked for R2 details on the Drime path:\n%s", out.String())
	}
}

func TestSetupProviderFlagSkipsQuestion(t *testing.T) {
	var out strings.Builder
	p := newPrompter(&Options{Out: &out, In: strings.NewReader("tok\n")})
	c, err := askForDestination(context.Background(), p, "drime")
	if err != nil || !c.IsDrime() {
		t.Fatalf("got %+v %v", c.Redacted(), err)
	}
	if strings.Contains(out.String(), "Where should backups go") {
		t.Fatal("asked where to back up despite --provider")
	}
}

// A credentials file from before Drime existed has no provider and must
// still read as R2.
func TestOldCredentialsAreR2(t *testing.T) {
	c := creds.Credentials{AccountID: "a", AccessKeyID: "k", SecretAccessKey: "s", Bucket: "b"}
	if c.IsDrime() || c.Valid() != nil {
		t.Fatalf("old credentials misread: drime=%v valid=%v", c.IsDrime(), c.Valid())
	}
	if (creds.Credentials{Provider: creds.ProviderDrime}).Valid() == nil {
		t.Fatal("Drime credentials without a token passed validation")
	}
	if strings.Contains(creds.Credentials{Provider: "drime", DrimeToken: "secret-token-1234"}.Redacted().DrimeToken, "secret") {
		t.Fatal("token not redacted")
	}
}
