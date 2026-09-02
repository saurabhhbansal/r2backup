package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/saurabhhbansal/r2backup/internal/account"
	"github.com/saurabhhbansal/r2backup/internal/creds"
	"github.com/saurabhhbansal/r2backup/internal/remote"
	minio "github.com/saurabhhbansal/r2backup/test/minio"
)

// bucketWithABackupInIt starts a real object store, puts one backed-up file in
// it under the layout `add` produces, and returns credentials that reach it.
func bucketWithABackupInIt(t *testing.T) (*remote.Client, creds.Credentials) {
	t.Helper()
	var cfg remote.Config
	client, cleanup := minio.StartWithConfig(t, func(c *remote.Config) { cfg = *c })
	t.Cleanup(cleanup)

	body := []byte("the report\n")
	if err := client.Put(context.Background(), remote.PutInput{
		Key:      "machines/desktop/Documents/current/report.txt",
		Body:     bytes.NewReader(body),
		Size:     int64(len(body)),
		Metadata: remote.Metadata{ModTime: time.Now(), Mode: 0o644},
	}); err != nil {
		t.Fatalf("seed the bucket: %v", err)
	}
	return client, creds.Credentials{
		AccountID:       "acct",
		AccessKeyID:     cfg.AccessKeyID,
		SecretAccessKey: cfg.SecretAccessKey,
		Bucket:          cfg.Bucket,
		Endpoint:        cfg.Endpoint,
	}
}

// storeVault puts c on the fake worker, encrypted the way the first computer
// would have encrypted it.
func storeVault(t *testing.T, w *fakeWorker, pass string, c creds.Credentials) {
	t.Helper()
	plain, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	v, err := account.Encrypt(pass, plain)
	if err != nil {
		t.Fatal(err)
	}
	w.vault = v
}

// TestSetupOnASecondComputer runs the real `setup` command, through cobra,
// with nothing on the machine at all: no credentials, no sets.json, no index.
// An email address, a code and a password are the only things carried over
// from the first computer, and at the end this one can see what is in the
// bucket.
func TestSetupOnASecondComputer(t *testing.T) {
	_, c := bucketWithABackupInIt(t)
	w, stop := newFakeWorker(t)
	defer stop()
	storeVault(t, w, "hunter2hunter2", c)
	t.Setenv("R2BACKUP_DATA_DIR", t.TempDir())

	var out, errOut bytes.Buffer
	opts := &Options{
		Out: &out, Err: &errOut,
		In: strings.NewReader("me@example.com\n123456\nhunter2hunter2\n"),
	}
	root := NewRoot(opts)
	root.SetOut(&out)
	root.SetErr(&errOut)
	root.SetArgs([]string{"setup"})
	if err := root.Execute(); err != nil {
		t.Fatalf("setup: %v\n--- output ---\n%s", err, out.String())
	}

	got := out.String()
	for _, want := range []string{
		"Found the credentials you saved from another computer",
		"Unlocked",
		// The README promises that setup says which of the two protections a
		// computer got, on every path in, not only the one where the keys
		// were typed by hand.
		"guarded by",
		"Connection works",
		// The closing line has to name what is actually restorable. A fresh
		// computer's whole reason to exist is the data already in the bucket,
		// and it has no local record naming any of it.
		"Documents",
		"r2b restore Documents --to",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("setup output is missing %q\n--- output ---\n%s", want, got)
		}
	}

	// And the credentials really landed, not just the message about them.
	a, err := openApp()
	if err != nil {
		t.Fatal(err)
	}
	defer a.close()
	saved, err := a.creds.Load()
	if err != nil {
		t.Fatalf("credentials were not saved: %v", err)
	}
	if saved != c {
		t.Errorf("saved %+v, want %+v", saved, c)
	}
	if len(w.devices) == 0 {
		t.Error("the computer did not register itself; `account devices` would not list it")
	}
}

// TestRestoreOnAComputerThatNeverRanAdd is the half of the account feature
// that did not exist. Credentials arrived and then nothing could be done with
// them: `restore` read the set out of the local sets.json, and a machine that
// has never run `add` has an empty one, so the data was sitting there with no
// way to name it.
func TestRestoreOnAComputerThatNeverRanAdd(t *testing.T) {
	_, c := bucketWithABackupInIt(t)
	dataDir := t.TempDir()
	t.Setenv("R2BACKUP_DATA_DIR", dataDir)

	a, err := openApp()
	if err != nil {
		t.Fatal(err)
	}
	if err := a.creds.Save(c); err != nil {
		t.Fatal(err)
	}
	a.close()

	into := t.TempDir()
	var out, errOut bytes.Buffer
	root := NewRoot(&Options{Out: &out, Err: &errOut})
	root.SetOut(&out)
	root.SetErr(&errOut)
	root.SetArgs([]string{"restore", "Documents", "--to", into})
	if err := root.Execute(); err != nil {
		t.Fatalf("restore: %v\n--- output ---\n%s%s", err, out.String(), errOut.String())
	}

	body, err := os.ReadFile(filepath.Join(into, "report.txt"))
	if err != nil {
		t.Fatalf("the file was not restored: %v\n--- output ---\n%s", err, out.String())
	}
	if string(body) != "the report\n" {
		t.Errorf("restored %q, want %q", body, "the report\n")
	}
}

// A name that is not there must say what is, rather than leaving a computer
// with no local record to consult at a dead end.
func TestAMistypedSetNameListsWhatIsThere(t *testing.T) {
	_, c := bucketWithABackupInIt(t)
	t.Setenv("R2BACKUP_DATA_DIR", t.TempDir())

	a, err := openApp()
	if err != nil {
		t.Fatal(err)
	}
	if err := a.creds.Save(c); err != nil {
		t.Fatal(err)
	}
	a.close()

	var out, errOut bytes.Buffer
	root := NewRoot(&Options{Out: &out, Err: &errOut})
	root.SetOut(&out)
	root.SetErr(&errOut)
	root.SetArgs([]string{"restore", "Documnets", "--to", t.TempDir()})
	err = root.Execute()
	if err == nil {
		t.Fatal("restoring a set that does not exist should fail")
	}
	msg := err.Error()
	if !strings.Contains(msg, "Documents") || !strings.Contains(msg, "desktop") {
		t.Errorf("the error should list what is actually in the bucket; got:\n%s", msg)
	}
}
