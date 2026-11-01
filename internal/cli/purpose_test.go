package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/saurabhhbansal/r2backup/internal/account"
	"github.com/saurabhhbansal/r2backup/internal/config"
	"github.com/saurabhhbansal/r2backup/internal/creds"
)

// What someone is using r2backup for: asked after getting started, saved in
// settings, carried with the keys, and changed later.

var (
	drimeKeys = creds.Credentials{Provider: creds.ProviderDrime, DrimeToken: "drime-token-1234"}
	b2Keys    = creds.Credentials{Provider: creds.ProviderB2, Bucket: "second", Region: "us-west-004",
		AccessKeyID: "key-id", SecretAccessKey: "app-key", Bundle: true}
	customKeys = creds.Credentials{Provider: creds.ProviderCustom, Endpoint: "https://minio.local:9000",
		Bucket: "main", AccessKeyID: "k", SecretAccessKey: "s", Bundle: true}
)

// noStorage stands in for the checks that would reach Drime or S3.
func noStorage(t *testing.T) {
	t.Helper()
	check, second, fin := setupCheck, secondCheck, setupFinish
	setupCheck = func(context.Context, *app, *prompter) error { return nil }
	secondCheck = func(context.Context, creds.Credentials) error { return nil }
	setupFinish = func(context.Context, *app, *Options, *prompter) error { return nil }
	t.Cleanup(func() { setupCheck, secondCheck, setupFinish = check, second, fin })
}

func TestPurposeIsSavedInSettings(t *testing.T) {
	t.Setenv(config.EnvDataDir, t.TempDir())
	if storedPurpose() != "" {
		t.Fatal("a fresh computer has a purpose")
	}
	if err := savePurpose(purposeS3); err != nil {
		t.Fatal(err)
	}
	path, _ := config.SettingsPath()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var onDisk map[string]any
	if err := json.Unmarshal(raw, &onDisk); err != nil {
		t.Fatal(err)
	}
	if onDisk["purpose"] != "s3" {
		t.Fatalf("settings.json = %s", raw)
	}
	if storedPurpose() != purposeS3 {
		t.Fatalf("read back %q", storedPurpose())
	}
	// Other settings are kept.
	if err := setMirrorAuto(true); err != nil {
		t.Fatal(err)
	}
	if storedPurpose() != purposeS3 {
		t.Fatal("saving another setting lost the purpose")
	}
}

func TestParsePurpose(t *testing.T) {
	for in, want := range map[string]string{"both": purposeBoth, "Primary+Second": purposeBoth, " s3 ": purposeS3, "s3-only": purposeS3} {
		if got, ok := parsePurpose(in); !ok || got != want {
			t.Errorf("parsePurpose(%q) = %q, %v", in, got, ok)
		}
	}
	if _, ok := parsePurpose("drive"); ok {
		t.Error("an unknown purpose parsed")
	}
}

// A computer set up before the question existed reads its purpose off its
// destinations, so the account screen does not show nothing chosen.
func TestPurposeIsInferredFromOlderSetups(t *testing.T) {
	cases := []struct {
		primary, second *creds.Credentials
		want            string
	}{
		{nil, nil, ""},
		{&drimeKeys, nil, purposeBoth},
		{&drimeKeys, &b2Keys, purposeBoth},
		{&b2Keys, &drimeKeys, purposeBoth},
		{&b2Keys, nil, purposeS3},
	}
	for _, c := range cases {
		if got := inferPurpose(c.primary, c.second); got != c.want {
			t.Errorf("inferPurpose(%v, %v) = %q, want %q", c.primary != nil, c.second != nil, got, c.want)
		}
	}
}

func purposeSet(t *testing.T, tb *trayBackend, p string) purposeResult {
	t.Helper()
	out, err := call(t, tb, "purposeSet", map[string]string{"purpose": p})
	if err != nil {
		t.Fatalf("purposeSet %s: %v", p, err)
	}
	return out.(purposeResult)
}

// Getting started in the panel: the purpose is saved and says what to set
// up next.
func TestPanelPurposeOnAFreshComputer(t *testing.T) {
	tb := trayForTest(t)
	if r := purposeSet(t, tb, "both"); !r.Saved || r.Need != needDrime {
		t.Fatalf("both = %+v", r)
	}
	got, err := call(t, tb, "purposeGet", nil)
	if err != nil || got.(map[string]string)["purpose"] != purposeBoth {
		t.Fatalf("purposeGet = %v, %v", got, err)
	}
	if r := purposeSet(t, tb, "s3"); !r.Saved || r.Need != needS3 {
		t.Fatalf("s3 = %+v", r)
	}
	if _, err := call(t, tb, "purposeSet", map[string]string{"purpose": "tape"}); err == nil {
		t.Fatal("an unknown purpose was accepted")
	}
	cfg, err := tb.accountConfig(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Purpose != purposeS3 {
		t.Fatalf("accountConfig purpose = %q", cfg.Purpose)
	}
}

// Changing it later: S3 storage only to adding a second copy, and back.
func TestPanelPurposeSwitches(t *testing.T) {
	tb := trayForTest(t)
	a := tb.d.app
	if err := a.creds.Save(customKeys); err != nil {
		t.Fatal(err)
	}
	_ = savePurpose(purposeS3)

	// No second copy yet: nothing changes until one is added.
	if r := purposeSet(t, tb, "both"); r.Saved || r.Need != needSecond {
		t.Fatalf("both without a second copy = %+v", r)
	}
	if storedPurpose() != purposeS3 {
		t.Fatal("the purpose changed before a second copy was set up")
	}
	// Drime added as the second copy, then the purpose holds.
	if err := a.mirror.Save(drimeKeys); err != nil {
		t.Fatal(err)
	}
	_ = setMirrorAuto(true)
	if r := purposeSet(t, tb, "both"); !r.Saved || r.Need != "" {
		t.Fatalf("both with a second copy = %+v", r)
	}
	if storedPurpose() != purposeBoth {
		t.Fatal("not saved")
	}

	// Back to S3 only: the Drime second copy is dropped, backups stay put.
	if r := purposeSet(t, tb, "s3"); !r.Saved || r.Moved != "" {
		t.Fatalf("s3 = %+v", r)
	}
	if a.mirror.Exists() {
		t.Fatal("the Drime second copy is still set up")
	}
	if c, _ := a.creds.Load(); c != customKeys {
		t.Fatalf("where backups go changed: %+v", c.Redacted())
	}
	if st, _ := config.LoadSettings(); st.MirrorAuto {
		t.Fatal("still copying after every backup")
	}
}

// Backups going to Drime with an S3 second copy: S3 storage only makes the
// second copy where backups go.
func TestPanelPurposeS3MovesBackupsToTheSecondCopy(t *testing.T) {
	tb := trayForTest(t)
	a := tb.d.app
	_ = a.creds.Save(drimeKeys)
	_ = a.mirror.Save(b2Keys)
	_ = setMirrorAuto(true)
	_ = savePurpose(purposeBoth)

	r := purposeSet(t, tb, "s3")
	if !r.Saved || r.Moved != destLabel(b2Keys) {
		t.Fatalf("s3 = %+v", r)
	}
	if c, _ := a.creds.Load(); c != b2Keys {
		t.Fatalf("backups go to %+v", c.Redacted())
	}
	if a.mirror.Exists() {
		t.Fatal("the second copy is still set up")
	}
	cfg, _ := tb.accountConfig(t.Context())
	if cfg.Purpose != purposeS3 || cfg.Second != nil || cfg.Primary.Provider != creds.ProviderB2 {
		t.Fatalf("accountConfig = %+v", cfg)
	}
}

// Drime alone has no S3 storage to move to: it is asked for first.
func TestPanelPurposeS3NeedsStorageFromDrimeAlone(t *testing.T) {
	tb := trayForTest(t)
	_ = tb.d.app.creds.Save(drimeKeys)
	if r := purposeSet(t, tb, "s3"); r.Saved || r.Need != needS3 {
		t.Fatalf("s3 = %+v", r)
	}
	if c, _ := tb.d.app.creds.Load(); c != drimeKeys {
		t.Fatal("where backups go changed")
	}
	if storedPurpose() != "" {
		t.Fatal("saved before S3 storage was connected")
	}
}

// The purpose travels sealed with the keys, so the second computer is not
// asked again; and the account screen sees a blob without it as not saved.
func TestPurposeTravelsWithTheKeys(t *testing.T) {
	tb := trayForTest(t)
	_ = tb.d.app.creds.Save(drimeKeys)
	_ = tb.d.app.mirror.Save(b2Keys)
	_ = savePurpose(purposeBoth)
	k := localStoredKeys(tb.d.app)
	if k.Purpose != purposeBoth {
		t.Fatalf("local keys purpose = %q", k.Purpose)
	}
	if sameKeys(k, account.StoredKeys{Primary: k.Primary, Second: k.Second}) {
		t.Fatal("keys saved without the purpose read as saved")
	}

	// A key file carries it too.
	file := filepath.Join(t.TempDir(), "keys.r2bkeys")
	if err := exportKeys(tb.d.app, file, "long enough passphrase"); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(file)
	got, err := account.ImportKeys("long enough passphrase", data)
	if err != nil || got.Purpose != purposeBoth {
		t.Fatalf("key file purpose = %+v, %v", got, err)
	}

	// adoptPurpose takes it on a computer that has not chosen, and leaves a
	// choice already made alone.
	t.Setenv(config.EnvDataDir, t.TempDir())
	adoptPurpose(purposeS3)
	if storedPurpose() != purposeS3 {
		t.Fatal("not adopted")
	}
	adoptPurpose(purposeBoth)
	if storedPurpose() != purposeS3 {
		t.Fatal("overwrote a purpose chosen here")
	}
}

func TestSetupFlagsAgree(t *testing.T) {
	for _, bad := range [][3]string{
		{"drime", "s3", ""},
		{"b2", "both", ""},
		{"", "s3", "r2"},
		{"", "", "r2"},
		{"", "both", "drime"},
		{"", "tape", ""},
	} {
		if _, err := parseSetupFlags(bad[0], bad[1], bad[2]); err == nil {
			t.Errorf("parseSetupFlags(%q, %q, %q) accepted", bad[0], bad[1], bad[2])
		}
	}
	f, err := parseSetupFlags("", "both", "b2")
	if err != nil || f.purpose != purposeBoth || f.second != "b2" {
		t.Fatalf("got %+v, %v", f, err)
	}
}

// runSetupWith runs setup with a script of typed lines.
func runSetupWith(t *testing.T, f setupFlags, lines ...string) (*app, string) {
	t.Helper()
	a, err := openApp()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.close)
	opts := conversation(lines...)
	if err := runSetup(context.Background(), a, opts, f); err != nil {
		t.Fatalf("setup: %v\n%s", err, said(opts))
	}
	return a, said(opts)
}

// The setup flow, both purposes, both account modes.
func TestSetupAsksThePurpose(t *testing.T) {
	type check func(t *testing.T, a *app, out string)
	both := func(t *testing.T, a *app, out string) {
		if c, _ := a.creds.Load(); !c.IsDrime() || c.DrimeToken != "tok" || c.Bundle {
			t.Errorf("backups go to %+v", c.Redacted())
		}
		if c, err := a.mirror.Load(); err != nil || c.Provider != creds.ProviderCustom || !c.Bundle {
			t.Errorf("second copy %+v, %v", c.Redacted(), err)
		}
		if st, _ := config.LoadSettings(); !st.MirrorAuto || st.Purpose != purposeBoth {
			t.Errorf("settings %+v", st)
		}
		for _, want := range []string{purposeQuestion, purposeBothText, purposeS3Text, "Where should the second copy go?", "Second copy:"} {
			if !strings.Contains(out, want) {
				t.Errorf("missing %q in\n%s", want, out)
			}
		}
		if strings.Contains(out, "Drime                  --") {
			t.Error("offered Drime for the second copy")
		}
	}
	s3only := func(t *testing.T, a *app, out string) {
		if c, _ := a.creds.Load(); c.Provider != creds.ProviderB2 || !c.Bundle {
			t.Errorf("backups go to %+v", c.Redacted())
		}
		if a.mirror.Exists() {
			t.Error("S3 storage only set up a second copy")
		}
		if storedPurpose() != purposeS3 {
			t.Errorf("purpose %q", storedPurpose())
		}
		if after := out[strings.Index(out, "Choose 1 or 2"):]; strings.Contains(after, "Drime") {
			t.Errorf("S3 storage only mentioned Drime after the question:\n%s", after)
		}
	}
	bothLines := []string{"1", "tok", "", "4", "https://minio.local:9000", "bucket", "", "key", "secret", ""}
	s3Lines := []string{"2", "2", "bucket", "us-west-004", "key", "secret", ""}

	for _, tc := range []struct {
		name    string
		account bool
		lines   []string
		check   check
	}{
		{"local both", false, bothLines, both},
		{"local s3", false, s3Lines, s3only},
		{"account both", true, bothLines, both},
		{"account s3", true, s3Lines, s3only},
	} {
		t.Run(tc.name, func(t *testing.T) {
			noStorage(t)
			_, stop := newFakeWorker(t)
			defer stop()
			t.Setenv(config.EnvDataDir, t.TempDir())
			signIn := []string{""}
			if tc.account {
				signIn = []string{"me@example.com", "123456", testPIN, testPIN}
			}
			a, out := runSetupWith(t, setupFlags{}, append(signIn, tc.lines...)...)
			tc.check(t, a, out)
			if !tc.account {
				return
			}
			// What the account keeps for the next computer has the purpose.
			token, key, err := accountSession()
			if err != nil {
				t.Fatal(err)
			}
			k, err := accountClient().GetKeys(context.Background(), token, key)
			if err != nil {
				t.Fatal(err)
			}
			if k.Purpose != storedPurpose() || (k.Purpose == purposeBoth) != (k.Second != nil) {
				t.Errorf("account keeps purpose %q, second %v", k.Purpose, k.Second != nil)
			}
		})
	}
}

// --purpose and --second answer the questions for a script.
func TestSetupPurposeFlags(t *testing.T) {
	noStorage(t)
	_, stop := newFakeWorker(t)
	defer stop()
	t.Setenv(config.EnvDataDir, t.TempDir())
	f, err := parseSetupFlags("", "both", "b2")
	if err != nil {
		t.Fatal(err)
	}
	a, out := runSetupWith(t, f, "", "tok", "", "bucket", "us-west-004", "key", "secret", "")
	if strings.Contains(out, purposeQuestion) || strings.Contains(out, "Where should the second copy go?") {
		t.Errorf("asked what the flags answered:\n%s", out)
	}
	if c, err := a.mirror.Load(); err != nil || c.Provider != creds.ProviderB2 {
		t.Fatalf("second copy %+v, %v", c.Redacted(), err)
	}
	if storedPurpose() != purposeBoth {
		t.Fatal("purpose not saved")
	}
}

// A second computer signed in to an account that keeps a purpose is not
// asked again.
func TestSetupSecondComputerBringsThePurpose(t *testing.T) {
	noStorage(t)
	_, stop := newFakeWorker(t)
	defer stop()
	t.Setenv(config.EnvDataDir, t.TempDir())
	storeKeys(t, account.StoredKeys{Primary: &b2Keys, Purpose: purposeS3})

	a, out := runSetupWith(t, setupFlags{}, "me@example.com", "123456", testPIN)
	if strings.Contains(out, purposeQuestion) {
		t.Errorf("asked again:\n%s", out)
	}
	if !strings.Contains(out, "Using r2backup for: "+purposeS3Title) {
		t.Errorf("did not say the purpose:\n%s", out)
	}
	if storedPurpose() != purposeS3 {
		t.Fatalf("purpose %q", storedPurpose())
	}
	if c, _ := a.creds.Load(); c != b2Keys {
		t.Fatal("keys not pulled")
	}
}

// `r2b setup purpose` shows it and changes it.
func TestSetupPurposeCommand(t *testing.T) {
	t.Setenv(config.EnvDataDir, t.TempDir())
	t.Setenv("R2BACKUP_ACCOUNT_API", "http://127.0.0.1:1") // never reached: not signed in
	run := func(args ...string) (string, error) {
		var out bytes.Buffer
		root := NewRoot(&Options{Out: &out, Err: &out})
		root.SetOut(&out)
		root.SetErr(&out)
		root.SetArgs(append([]string{"setup", "purpose"}, args...))
		err := root.Execute()
		return out.String(), err
	}
	if out, err := run(); err != nil || !strings.Contains(out, "Not chosen yet") {
		t.Fatalf("%q %v", out, err)
	}
	a, err := openApp()
	if err != nil {
		t.Fatal(err)
	}
	_ = a.creds.Save(drimeKeys)
	_ = a.mirror.Save(b2Keys)
	a.close()
	if out, err := run(); err != nil || !strings.Contains(out, purposeBothTitle) || !strings.Contains(out, "second copy on Backblaze B2") {
		t.Fatalf("%q %v", out, err)
	}
	out, err := run("s3")
	if err != nil || !strings.Contains(out, "Backups go to Backblaze B2 · second now") || !strings.Contains(out, purposeS3Title) {
		t.Fatalf("%q %v", out, err)
	}
	if storedPurpose() != purposeS3 {
		t.Fatal("not saved")
	}
	if _, err := run("both"); err == nil {
		t.Fatal("both with no second copy was accepted")
	}
}
