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
	in := strings.NewReader("1\n  tok-123  \nn\n")
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
	p := newPrompter(&Options{Out: &out, In: strings.NewReader("tok\nn\n")})
	c, err := askForDestination(context.Background(), p, "drime")
	if err != nil || !c.IsDrime() {
		t.Fatalf("got %+v %v", c.Redacted(), err)
	}
	if strings.Contains(out.String(), "Where should backups go") {
		t.Fatal("asked where to back up despite --provider")
	}
}

// Backblaze B2 asks for its bucket, region and key, works out the endpoint,
// and packs small files unless told not to.
func TestSetupB2Preset(t *testing.T) {
	var out strings.Builder
	in := strings.NewReader("3\nmy-bucket\nus-west-004\nkey-id\napp-key\n\n")
	c, err := askForDestination(context.Background(), newPrompter(&Options{Out: &out, In: in}), "")
	if err != nil {
		t.Fatalf("askForDestination: %v\n%s", err, out.String())
	}
	if c.Provider != creds.ProviderB2 || c.Bucket != "my-bucket" || c.AccessKeyID != "key-id" || !c.Bundle {
		t.Fatalf("got %+v", c.Redacted())
	}
	if ep := c.S3(); ep.URL != "https://s3.us-west-004.backblazeb2.com" || ep.Region != "us-west-004" || ep.VirtualHost {
		t.Fatalf("endpoint %+v", ep)
	}
}

// A custom endpoint needs its URL; Amazon S3 signs for its region with the
// bucket in the hostname.
func TestCustomAndAmazonEndpoints(t *testing.T) {
	custom := creds.Credentials{Provider: creds.ProviderCustom, AccessKeyID: "k", SecretAccessKey: "s", Bucket: "b"}
	if custom.Valid() == nil {
		t.Fatal("a custom endpoint without its URL passed validation")
	}
	custom.Endpoint = "https://minio.local:9000"
	if err := custom.Valid(); err != nil {
		t.Fatal(err)
	}
	if ep := custom.S3(); ep.URL != "https://minio.local:9000" || ep.Region != "us-east-1" {
		t.Fatalf("custom %+v", ep)
	}
	aws := creds.Credentials{Provider: creds.ProviderS3, Region: "eu-west-1", AccessKeyID: "k", SecretAccessKey: "s", Bucket: "b"}
	if ep := aws.S3(); ep.URL != "" || ep.Region != "eu-west-1" || !ep.VirtualHost {
		t.Fatalf("aws %+v", ep)
	}
	if aws.Label() != "Amazon S3" || destLabel(aws) != "Amazon S3 · b" {
		t.Fatalf("labels %q %q", aws.Label(), destLabel(aws))
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
