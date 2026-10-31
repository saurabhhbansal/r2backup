package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/saurabhhbansal/r2backup/internal/account"
	"github.com/saurabhhbansal/r2backup/internal/config"
	"github.com/saurabhhbansal/r2backup/internal/creds"
	"github.com/saurabhhbansal/r2backup/internal/ui"
)

// The panel's getting-started screen, call by call, against the fake account
// service: what panel.html sends for each button.

func TestPanelSignInChoosesThenEntersAPIN(t *testing.T) {
	tb := trayForTest(t)
	w, stop := newFakeWorker(t)
	defer stop()

	if _, err := call(t, tb, "signInStart", map[string]string{"email": "me@example.com"}); err != nil {
		t.Fatal(err)
	}
	if _, err := call(t, tb, "signInVerify", map[string]string{"email": "me@example.com", "code": "000000"}); err == nil {
		t.Fatal("a wrong code was accepted")
	}
	if _, err := call(t, tb, "signInPin", map[string]string{"pin": testPIN}); err == nil {
		t.Fatal("a PIN with no email code before it was accepted")
	}
	out, err := call(t, tb, "signInVerify", map[string]string{"email": "me@example.com", "code": "123456"})
	if err != nil {
		t.Fatal(err)
	}
	if out.(map[string]bool)["pinSet"] {
		t.Fatal("a new account says it has a PIN")
	}
	// Not signed in until the PIN step is done.
	if tok, _ := account.LoadToken(); tok != "" {
		t.Fatal("signed in before the PIN")
	}
	if _, err := call(t, tb, "signInPin", map[string]string{"pin": testPIN}); err != nil {
		t.Fatalf("choosing the first PIN: %v", err)
	}
	if accountMode() != accountModeAccount {
		t.Errorf("mode = %q after signing in", accountMode())
	}
	cfg, err := tb.accountConfig(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Mode != "account" || !cfg.SignedIn || cfg.NeedsPIN {
		t.Errorf("accountConfig = %+v", cfg)
	}

	// Another sign-in enters that PIN. Wrong ones say how many tries are
	// left, then lock.
	if _, err := call(t, tb, "signOut", nil); err != nil {
		t.Fatal(err)
	}
	out, err = call(t, tb, "signInVerify", map[string]string{"email": "me@example.com", "code": "123456"})
	if err != nil || !out.(map[string]bool)["pinSet"] {
		t.Fatalf("verify again: %v %v", out, err)
	}
	_, err = call(t, tb, "signInPin", map[string]string{"pin": "111111"})
	if err == nil || !strings.Contains(err.Error(), "4 tries left") {
		t.Fatalf("a wrong PIN: %v", err)
	}
	if _, err := call(t, tb, "signInPin", map[string]string{"pin": testPIN}); err != nil {
		t.Fatalf("the right PIN after a wrong one: %v", err)
	}
	if w.wrong != 0 {
		t.Errorf("the right PIN did not clear the wrong count: %d", w.wrong)
	}

	_, _ = call(t, tb, "signOut", nil)
	if _, err := call(t, tb, "signInVerify", map[string]string{"email": "me@example.com", "code": "123456"}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4; i++ {
		_, _ = call(t, tb, "signInPin", map[string]string{"pin": "111111"})
	}
	_, err = call(t, tb, "signInPin", map[string]string{"pin": "111111"})
	if err == nil || !strings.Contains(err.Error(), "Try again in 15 minutes") {
		t.Fatalf("five wrong PINs should lock: %v", err)
	}
}

func TestPanelForgotPINDeletesTheSavedKeys(t *testing.T) {
	tb := trayForTest(t)
	w, stop := newFakeWorker(t)
	defer stop()
	storeKeys(t, account.StoredKeys{Primary: &creds.Credentials{Provider: creds.ProviderDrime, DrimeToken: "t"}})
	if w.sealed == nil {
		t.Fatal("setup stored nothing")
	}

	if _, err := call(t, tb, "pinReset", map[string]string{"pin": "135790"}); err == nil {
		t.Fatal("a reset with no email code before it was accepted")
	}
	if _, err := call(t, tb, "signInVerify", map[string]string{"email": "me@example.com", "code": "123456"}); err != nil {
		t.Fatal(err)
	}
	if _, err := call(t, tb, "pinReset", map[string]string{"pin": "135790"}); err != nil {
		t.Fatalf("pinReset: %v", err)
	}
	if w.sealed != nil {
		t.Error("the keys locked with the old PIN are still kept")
	}
	if _, err := account.LoadSealKey(); err != nil {
		t.Errorf("signed in without a key after the reset: %v", err)
	}
	_, _ = call(t, tb, "signOut", nil)
	_, _ = call(t, tb, "signInVerify", map[string]string{"email": "me@example.com", "code": "123456"})
	if _, err := call(t, tb, "signInPin", map[string]string{"pin": "135790"}); err != nil {
		t.Fatalf("the new PIN does not work: %v", err)
	}
}

func TestPanelAsksAnOldSignInForAPIN(t *testing.T) {
	tb := trayForTest(t)
	w, stop := newFakeWorker(t)
	defer stop()
	w.legacyToken = fakeToken(`{"sub":"me@example.com","exp":4000000000}`)
	if err := account.SaveToken(w.legacyToken); err != nil {
		t.Fatal(err)
	}

	st := tb.summarize(nil, ui.Overview{}, time.Now())
	if !st.SignedIn || !st.NeedsPIN || st.Mode != "account" {
		t.Fatalf("state = signedIn %v needsPin %v mode %q", st.SignedIn, st.NeedsPIN, st.Mode)
	}
	cfg, err := tb.accountConfig(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.NeedsPIN {
		t.Errorf("accountConfig does not ask for a PIN: %+v", cfg)
	}
	if _, err := call(t, tb, "setPin", map[string]string{"pin": "12345"}); err == nil {
		t.Error("a 5-digit PIN was accepted")
	}
	if _, err := call(t, tb, "setPin", map[string]string{"pin": testPIN}); err != nil {
		t.Fatalf("setPin: %v", err)
	}
	if st := tb.summarize(nil, ui.Overview{}, time.Now()); st.NeedsPIN {
		t.Error("still asked for a PIN after setting one")
	}
}

func TestPanelWithoutAnAccount(t *testing.T) {
	tb := trayForTest(t)
	if st := tb.summarize(nil, ui.Overview{}, time.Now()); st.Mode != "" || st.SignedIn {
		t.Fatalf("a fresh computer: mode %q signedIn %v", st.Mode, st.SignedIn)
	}
	if _, err := call(t, tb, "useLocal", nil); err != nil {
		t.Fatal(err)
	}
	if st := tb.summarize(nil, ui.Overview{}, time.Now()); st.Mode != "local" || st.SignedIn || st.NeedsPIN {
		t.Fatalf("after useLocal: mode %q signedIn %v", st.Mode, st.SignedIn)
	}
}

func TestPanelPackingAndKeyFiles(t *testing.T) {
	tb := trayForTest(t)
	second := creds.Credentials{Provider: creds.ProviderB2, Region: "us-west-004", AccessKeyID: "kid", SecretAccessKey: "sec", Bucket: "b"}
	if err := tb.d.app.mirror.Save(second); err != nil {
		t.Fatal(err)
	}
	out, err := call(t, tb, "setBundle", map[string]any{"which": "second", "on": true})
	if err != nil {
		t.Fatal(err)
	}
	if st := out.(*storeOut); !st.Bundle || st.Provider != "b2" || st.Label != "Backblaze B2 · b" {
		t.Errorf("setBundle = %+v", st)
	}
	if c, _ := tb.d.app.mirror.Load(); !c.Bundle {
		t.Error("packing was not saved")
	}

	path := filepath.Join(t.TempDir(), "keys.r2b")
	if _, err := call(t, tb, "keysExport", map[string]string{"path": path, "passphrase": "short"}); err == nil {
		t.Error("a passphrase under 8 characters was accepted")
	}
	if _, err := call(t, tb, "keysExport", map[string]string{"path": path, "passphrase": "long enough"}); err != nil {
		t.Fatal(err)
	}
	if _, err := call(t, tb, "keysExport", map[string]string{"path": path, "passphrase": "long enough"}); err == nil ||
		!strings.Contains(err.Error(), "there already") {
		t.Errorf("exporting over a file: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}

	// Another computer.
	tb2 := trayForTest(t)
	if _, err := call(t, tb2, "keysImport", map[string]string{"path": path, "passphrase": "not it at all"}); err == nil {
		t.Error("the wrong passphrase opened the file")
	}
	out, err = call(t, tb2, "keysImport", map[string]string{"path": path, "passphrase": "long enough"})
	if err != nil {
		t.Fatal(err)
	}
	if got := out.(importedKeys); got.Second != "Backblaze B2 · b" || got.Primary != "" {
		t.Errorf("keysImport = %+v", got)
	}
	if c, err := tb2.d.app.mirror.Load(); err != nil || c != (func() creds.Credentials { s := second; s.Bundle = true; return s })() {
		t.Errorf("imported second copy = %+v %v", c, err)
	}
	if accountMode() != accountModeLocal {
		t.Errorf("importing a key file should mean no account; mode %q", accountMode())
	}
}

func TestPanelDrimeFoldersNeedS3Storage(t *testing.T) {
	tb := trayForTest(t)
	out, err := call(t, tb, "drimeSources", nil)
	if err != nil {
		t.Fatal(err)
	}
	d := out.(drimeSourcesOut)
	if d.DestErr == "" || d.HaveToken || len(d.Sources) != 0 {
		t.Errorf("with nothing connected: %+v", d)
	}
	if _, err := call(t, tb, "drimeSourceAdd", map[string]any{"token": "tok", "folder": "Backups/Laptop"}); err == nil ||
		!strings.Contains(err.Error(), "S3-compatible storage first") {
		t.Errorf("adding a folder with nowhere to copy it: %v", err)
	}
	// The token given with it is kept, so the next try does not ask again.
	out, _ = call(t, tb, "drimeSources", nil)
	if !out.(drimeSourcesOut).HaveToken {
		t.Error("the Drime token was not kept")
	}
	if err := tb.d.app.creds.Save(creds.Credentials{AccountID: "a", AccessKeyID: "k", SecretAccessKey: "s", Bucket: "b"}); err != nil {
		t.Fatal(err)
	}
	out, _ = call(t, tb, "drimeSources", nil)
	if d := out.(drimeSourcesOut); d.Dest != "Cloudflare R2 · b" || d.DestErr != "" {
		t.Errorf("with R2 connected: %+v", d)
	}
	if err := addSourceSetting(t, "Laptop", "Backups/Laptop"); err != nil {
		t.Fatal(err)
	}
	out, err = call(t, tb, "drimeSourceRemove", map[string]string{"name": "Laptop"})
	if err != nil {
		t.Fatal(err)
	}
	if n := len(out.(drimeSourcesOut).Sources); n != 0 {
		t.Errorf("drimeSourceRemove returned %d folders, want the list without the one removed", n)
	}
}

func addSourceSetting(t *testing.T, name, folder string) error {
	t.Helper()
	st, err := config.LoadSettings()
	if err != nil {
		return err
	}
	st.MirrorSources = append(st.MirrorSources, config.MirrorSource{Name: name, Folder: folder})
	return config.SaveSettings(st)
}
