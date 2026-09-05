package cli

import (
	"context"
	"os"
	"runtime"
	"strings"
	"testing"
)

// headless makes BrowserAvailable false on Linux, which is the state of every
// install reached over SSH. On macOS and Windows there is always a URL
// handler, so those platforms cannot be put in this state and the test is
// skipped rather than silently asserting something else.
func headless(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("BrowserAvailable is unconditionally true on " + runtime.GOOS)
	}
	t.Setenv("DISPLAY", "")
	t.Setenv("WAYLAND_DISPLAY", "")
}

// A machine with no browser must not be offered a browser sign-in. The
// question would be unanswerable, and worse, it would consume the line meant
// for the next prompt and shift every answer by one.
func TestAskForKeysSkipsSignInWithNoBrowser(t *testing.T) {
	headless(t)

	var out strings.Builder
	in := strings.NewReader(strings.Join([]string{
		"acct-1",    // Cloudflare account ID
		"key-1",     // R2 access key ID
		"secret-1",  // R2 secret access key
		"my-bucket", // R2 bucket name
	}, "\n") + "\n")

	p := newPrompter(&Options{Out: &out, In: in})
	c, err := askForKeys(context.Background(), p)
	if err != nil {
		t.Fatalf("askForKeys: %v", err)
	}

	if c.AccountID != "acct-1" {
		t.Errorf("AccountID = %q, want acct-1 -- the answers have shifted", c.AccountID)
	}
	if c.AccessKeyID != "key-1" {
		t.Errorf("AccessKeyID = %q, want key-1", c.AccessKeyID)
	}
	if c.SecretAccessKey != "secret-1" {
		t.Errorf("SecretAccessKey = %q, want secret-1", c.SecretAccessKey)
	}
	if c.Bucket != "my-bucket" {
		t.Errorf("Bucket = %q, want my-bucket", c.Bucket)
	}
	if err := c.Valid(); err != nil {
		t.Errorf("credentials are not usable: %v", err)
	}

	// Nothing should have been said about signing in, because nothing could
	// have come of it.
	if s := out.String(); strings.Contains(strings.ToLower(s), "sign in with cloudflare") {
		t.Errorf("offered a browser sign-in on a machine with no browser:\n%s", s)
	}
}

// The token page link is the thing that replaces "go and find the right
// dashboard page", so it has to name the account and actually be shown.
func TestAskForKeysLinksToTheTokenPage(t *testing.T) {
	headless(t)

	var out strings.Builder
	in := strings.NewReader("acct-xyz\nkey\nsecret\nbucket\n")
	p := newPrompter(&Options{Out: &out, In: in})
	if _, err := askForKeys(context.Background(), p); err != nil {
		t.Fatalf("askForKeys: %v", err)
	}
	if want := tokenPageURL("acct-xyz"); !strings.Contains(out.String(), want) {
		t.Errorf("output does not link to %q:\n%s", want, out.String())
	}
}

func TestTokenPageURL(t *testing.T) {
	if got, want := tokenPageURL("abc123"), "https://dash.cloudflare.com/abc123/r2/api-tokens"; got != want {
		t.Errorf("tokenPageURL = %q, want %q", got, want)
	}
	// With no account known there is still a page worth pointing at; the
	// dashboard resolves :account itself once someone is signed in.
	if got := tokenPageURL(""); !strings.Contains(got, "r2/api-tokens") {
		t.Errorf("tokenPageURL(\"\") = %q, want an R2 API tokens page", got)
	}
}

// The client ID ships in the binary by design, but an empty or placeholder
// one would fail every sign-in with a message nobody could act on.
func TestCloudflareClientIDIsSet(t *testing.T) {
	if strings.TrimSpace(cloudflareClientID) == "" {
		t.Fatal("cloudflareClientID is empty")
	}
	if strings.Contains(strings.ToUpper(cloudflareClientID), "REPLACE") {
		t.Fatalf("cloudflareClientID is still a placeholder: %q", cloudflareClientID)
	}
}

func TestConfirmDefaultsOnBareEnter(t *testing.T) {
	for _, tc := range []struct {
		in   string
		def  bool
		want bool
	}{
		{"\n", true, true},
		{"\n", false, false},
		{"y\n", false, true},
		{"yes\n", false, true},
		{"n\n", true, false},
		{"no\n", true, false},
		// Anything unrecognised takes the default rather than guessing.
		{"maybe\n", true, true},
		{"maybe\n", false, false},
	} {
		p := newPrompter(&Options{Out: os.Stderr, In: strings.NewReader(tc.in)})
		got, err := confirm(p, "Q", tc.def)
		if err != nil {
			t.Fatalf("confirm(%q): %v", tc.in, err)
		}
		if got != tc.want {
			t.Errorf("confirm(%q, def=%v) = %v, want %v", tc.in, tc.def, got, tc.want)
		}
	}
}

func TestAskIndexRejectsOutOfRange(t *testing.T) {
	var out strings.Builder
	// Two bad answers, then a good one: a mistyped number should cost a
	// keystroke, not the whole sign-in.
	p := newPrompter(&Options{Out: &out, In: strings.NewReader("0\n9\n2\n")})
	got, err := askIndex(p, "Number", 3)
	if err != nil {
		t.Fatalf("askIndex: %v", err)
	}
	if got != 1 {
		t.Errorf("askIndex = %d, want 1 (the second option, zero-based)", got)
	}
}

func TestAskIndexGivesUpEventually(t *testing.T) {
	var out strings.Builder
	p := newPrompter(&Options{Out: &out, In: strings.NewReader(strings.Repeat("nope\n", 20))})
	if _, err := askIndex(p, "Number", 3); err == nil {
		t.Error("askIndex accepted endless bad input")
	}
}
