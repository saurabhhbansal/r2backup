package cli

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/saurabhhbansal/r2backup/internal/account"
	"github.com/saurabhhbansal/r2backup/internal/config"
	"github.com/saurabhhbansal/r2backup/internal/creds"
	"github.com/saurabhhbansal/r2backup/internal/remote"
	"github.com/saurabhhbansal/r2backup/internal/sets"
)

// The terminal commands that do what the taskbar panel does, run through
// cobra the way a shell would, against in-memory storage and the fake
// account service.

// memStore is a small in-memory remote.Store.
type memStore struct {
	mu     sync.Mutex
	bucket string
	objs   map[string][]byte
}

func (m *memStore) Put(_ context.Context, in remote.PutInput) error {
	b, err := io.ReadAll(in.Body)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.objs[in.Key] = b
	return nil
}
func (m *memStore) Get(_ context.Context, key string) (*remote.Object, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.objs[key]
	if !ok {
		return nil, remote.ErrNotFound
	}
	return &remote.Object{Body: io.NopCloser(bytes.NewReader(b)), Size: int64(len(b))}, nil
}
func (m *memStore) Head(_ context.Context, key string) (*remote.HeadResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.objs[key]
	if !ok {
		return nil, remote.ErrNotFound
	}
	return &remote.HeadResult{Size: int64(len(b)), LastModified: time.Now()}, nil
}
func (m *memStore) List(_ context.Context, prefix string) ([]remote.ListEntry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []remote.ListEntry
	for k, b := range m.objs {
		if strings.HasPrefix(k, prefix) {
			out = append(out, remote.ListEntry{Key: k, Size: int64(len(b)), ModTime: time.Now()})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}
func (m *memStore) ListPrefixes(_ context.Context, prefix string) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	seen := map[string]bool{}
	var out []string
	for k := range m.objs {
		rest, ok := strings.CutPrefix(k, prefix)
		if !ok {
			continue
		}
		if i := strings.Index(rest, "/"); i > 0 && !seen[rest[:i]] {
			seen[rest[:i]] = true
			out = append(out, rest[:i])
		}
	}
	sort.Strings(out)
	return out, nil
}
func (m *memStore) Copy(_ context.Context, src, dst string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.objs[src]
	if !ok {
		return remote.ErrNotFound
	}
	m.objs[dst] = append([]byte(nil), b...)
	return nil
}
func (m *memStore) Delete(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.objs, key)
	return nil
}
func (m *memStore) DeleteBatch(ctx context.Context, keys []string) error {
	for _, k := range keys {
		_ = m.Delete(ctx, k)
	}
	return nil
}
func (m *memStore) DeletePrefix(_ context.Context, prefix string) (remote.PrefixDeletion, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var d remote.PrefixDeletion
	for k, b := range m.objs {
		if strings.HasPrefix(k, prefix) {
			d.Objects++
			d.Bytes += int64(len(b))
			delete(m.objs, k)
		}
	}
	return d, nil
}
func (m *memStore) AbandonStaleUploads(context.Context) (int, error) { return 0, nil }
func (m *memStore) ListMultipartUploads(context.Context, string) ([]remote.MultipartUpload, error) {
	return nil, nil
}
func (m *memStore) AbortUpload(context.Context, string, string) error { return nil }
func (m *memStore) Bucket() string                                    { return m.bucket }

func (m *memStore) has(prefix string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for k := range m.objs {
		if strings.HasPrefix(k, prefix) {
			return true
		}
	}
	return false
}

func (m *memStore) put(keys ...string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, k := range keys {
		m.objs[k] = []byte("data of " + k)
	}
}

// b2 is a destination in a bucket of the in-memory storage.
func b2(bucket string) creds.Credentials {
	return creds.Credentials{Provider: creds.ProviderB2, Bucket: bucket, Region: "us-west-004",
		AccessKeyID: "key-" + bucket, SecretAccessKey: "secret-" + bucket}
}

// parityWorld is a fresh data directory whose storage is in memory: "main"
// where backups go and, when second is true, "second" as the second copy.
func parityWorld(t *testing.T, second bool, folders ...sets.Set) (main, sec *memStore) {
	t.Helper()
	t.Setenv(config.EnvDataDir, t.TempDir())
	t.Setenv("R2BACKUP_MACHINE", "testpc")
	t.Setenv(EnvNoUpdateCheck, "1")
	stores := map[string]*memStore{}
	var mu sync.Mutex
	testStore = func(c creds.Credentials) (remote.Store, bool) {
		mu.Lock()
		defer mu.Unlock()
		s, ok := stores[c.Bucket]
		if !ok {
			s = &memStore{bucket: c.Bucket, objs: map[string][]byte{}}
			stores[c.Bucket] = s
		}
		return s, true
	}
	t.Cleanup(func() { testStore = nil })

	a, err := openApp()
	if err != nil {
		t.Fatal(err)
	}
	defer a.close()
	if err := a.creds.Save(b2("main")); err != nil {
		t.Fatal(err)
	}
	if second {
		if err := a.mirror.Save(b2("second")); err != nil {
			t.Fatal(err)
		}
	}
	for _, s := range folders {
		if err := a.sets.Add(s); err != nil {
			t.Fatal(err)
		}
	}
	s1, _ := testStore(b2("main"))
	s2, _ := testStore(b2("second"))
	return s1.(*memStore), s2.(*memStore)
}

func folder(t *testing.T, name string) sets.Set {
	return sets.Set{Name: name, Root: t.TempDir(), Machine: "testpc", Prefix: "machines/testpc/" + name, RetentionDays: 30}
}

// r2b runs the command line with opts and returns what it printed.
func r2b(t *testing.T, opts *Options, args ...string) (string, error) {
	t.Helper()
	if opts == nil {
		opts = &Options{Out: &bytes.Buffer{}, Err: &bytes.Buffer{}}
	}
	root := NewRoot(opts)
	root.SetOut(opts.Out)
	root.SetErr(opts.Err)
	root.SetArgs(args)
	err := root.ExecuteContext(context.Background())
	return opts.Out.(*bytes.Buffer).String(), err
}

func TestBackupsListsByComputerAndDeletesFromBothOrOnlyHere(t *testing.T) {
	main, second := parityWorld(t, true, folder(t, "Docs"))
	main.put("machines/testpc/Docs/current/a.txt", "machines/testpc/Old/current/b.txt",
		"machines/Desk/Photos/current/c.jpg")
	second.put("machines/testpc/Old/current/b.txt", "machines/Desk/Photos/current/c.jpg")

	out, err := r2b(t, nil, "backups")
	if err != nil {
		t.Fatal(err)
	}
	this, desk := strings.Index(out, "This computer"), strings.Index(out, "Desk")
	if this < 0 || desk < this || !strings.Contains(out, "No longer backed up") || !strings.Contains(out, "Photos") {
		t.Fatalf("backups list:\n%s", out)
	}

	if _, err := r2b(t, nil, "backups", "delete", "Docs", "--yes"); err == nil ||
		!strings.Contains(err.Error(), "backed up by this computer") {
		t.Fatalf("deleted a folder this computer backs up: %v", err)
	}
	if _, err := r2b(t, nil, "backups", "delete", "Photos"); err != errNeedsYes {
		t.Fatalf("with nobody to ask and no --yes: %v", err)
	}

	// Asked, and answered "only here": the second copy keeps it.
	opts := conversation("y", "2")
	if _, err := r2b(t, opts, "backups", "delete", "Old"); err != nil {
		t.Fatalf("%v\n%s", err, said(opts))
	}
	if main.has("machines/testpc/Old/") || !second.has("machines/testpc/Old/") {
		t.Errorf("only-here delete: main=%v second=%v", main.has("machines/testpc/Old/"), second.has("machines/testpc/Old/"))
	}
	if !strings.Contains(said(opts), "Every version is deleted") || !strings.Contains(said(opts), "The second copy keeps it") {
		t.Errorf("did not ask as the panel does:\n%s", said(opts))
	}

	// --yes is both, as the panel's default.
	if _, err := r2b(t, nil, "backups", "delete", "Photos", "--yes"); err != nil {
		t.Fatal(err)
	}
	if main.has("machines/Desk/Photos/") || second.has("machines/Desk/Photos/") {
		t.Error("Photos is still stored after deleting it from both")
	}
}

// trashKey is where the trash package keeps relPath trashed today.
func trashKey(set, relPath string) string {
	return "machines/testpc/" + set + "/trash/" + time.Now().UTC().Format("2006-01-02") + "/" +
		strings.TrimSuffix(relPath, filepath.Ext(relPath)) + "~120000-abcdef012345" + filepath.Ext(relPath)
}

func TestTrashDeleteForeverAndEmptyGoFromBothCopies(t *testing.T) {
	main, second := parityWorld(t, true, folder(t, "Docs"))
	a, b := trashKey("Docs", "a.txt"), trashKey("Docs", "sub/b.txt")
	main.put(a, b)
	second.put(a, b)

	opts := conversation("y")
	if _, err := r2b(t, opts, "trash", "rm", "Docs", "a.txt"); err != nil {
		t.Fatalf("%v\n%s", err, said(opts))
	}
	if main.has(a) || second.has(a) || !main.has(b) {
		t.Fatalf("trash rm: main a=%v second a=%v main b=%v", main.has(a), second.has(a), main.has(b))
	}
	if !strings.Contains(said(opts), "and the second copy. This can’t be undone.") {
		t.Errorf("did not say the second copy goes too:\n%s", said(opts))
	}
	if _, err := r2b(t, nil, "trash", "rm", "Docs", "nothing.txt", "--yes"); err == nil {
		t.Error("deleting a file that is not in the trash said it worked")
	}
	if _, err := r2b(t, nil, "trash", "empty", "--no"); err == nil || !main.has(b) {
		t.Fatalf("--no emptied the trash: %v", err)
	}
	out, err := r2b(t, nil, "trash", "empty", "--yes")
	if err != nil {
		t.Fatal(err)
	}
	if main.has(b) || second.has(b) || !strings.Contains(out, "Trash emptied (1 file)") {
		t.Fatalf("trash empty: %q", out)
	}
}

func TestTrashRecoverPutsTheFileBack(t *testing.T) {
	f := folder(t, "Docs")
	main, _ := parityWorld(t, false, f)
	key := trashKey("Docs", "notes.txt")
	main.put(key)

	out, err := r2b(t, nil, "trash", "recover", "Docs", "notes.txt")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	got, err := os.ReadFile(filepath.Join(f.Root, "notes.txt"))
	if err != nil || string(got) != "data of "+key {
		t.Fatalf("recovered %q, %v", got, err)
	}
}

func TestPackIsPerDestination(t *testing.T) {
	parityWorld(t, false)
	if out, err := r2b(t, nil, "pack", "on"); err != nil || !strings.Contains(out, "Packing small files at Backblaze B2 · main") {
		t.Fatalf("%q %v", out, err)
	}
	if _, err := r2b(t, nil, "pack", "off", "--second-copy"); err == nil {
		t.Error("changed packing on a second copy that does not exist")
	}
	if _, err := r2b(t, nil, "pack", "maybe"); err == nil {
		t.Error("took maybe for an answer")
	}
	a, _ := openApp()
	c, _ := a.creds.Load()
	a.close()
	if !c.Bundle {
		t.Error("pack on did not stick")
	}
}

// Usage and cost are gone: no command shows them and none sets a limit.
func TestNoUsageOrBudgetCommands(t *testing.T) {
	t.Setenv(config.EnvDataDir, t.TempDir())
	for _, args := range [][]string{{"usage"}, {"budget"}, {"budget", "set", "5"}} {
		if out, err := r2b(t, nil, args...); err == nil {
			t.Errorf("r2b %s still runs:\n%s", strings.Join(args, " "), out)
		}
	}
}

func TestMirrorOffAsksThenForgetsTheSecondCopy(t *testing.T) {
	_, second := parityWorld(t, true)
	second.put("machines/testpc/Docs/current/a.txt")
	if out, err := r2b(t, nil, "mirror", "off", "--no"); err != nil || !strings.Contains(out, "Still keeping") {
		t.Fatalf("%q %v", out, err)
	}
	opts := conversation("y")
	if _, err := r2b(t, opts, "mirror", "off"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(said(opts), "What’s in Backblaze B2 · second stays.") {
		t.Errorf("did not say what stays:\n%s", said(opts))
	}
	a, _ := openApp()
	defer a.close()
	if a.mirror.Exists() || !second.has("machines/") {
		t.Error("mirror off: the second copy is still set, or what was in it went")
	}
}

func TestMirrorSetupRefusesWhereBackupsAlreadyGo(t *testing.T) {
	parityWorld(t, false)
	a, err := openApp()
	if err != nil {
		t.Fatal(err)
	}
	defer a.close()
	// The same bucket as where backups go.
	same := conversation("main", "us-west-004", "key-main", "secret-main", "")
	if err := runMirrorSetup(context.Background(), a, same, "b2"); err == nil ||
		!strings.Contains(err.Error(), "A second copy has to be somewhere else") {
		t.Fatalf("same place: %v", err)
	}
	other := conversation("other", "us-west-004", "k", "s", "")
	if err := runMirrorSetup(context.Background(), a, other, "b2"); err != nil {
		t.Fatalf("%v\n%s", err, said(other))
	}
	if c, err := a.mirror.Load(); err != nil || c.Bucket != "other" {
		t.Fatalf("second copy %+v %v", c.Redacted(), err)
	}
	// Copying starts by itself, as when a second copy is added in the panel.
	if s, _ := config.LoadSettings(); !s.MirrorAuto {
		t.Error("a second copy was added and copying after every backup is off")
	}
	if !strings.Contains(said(other), "r2b mirror auto off") {
		t.Errorf("did not say how to stop copying:\n%s", said(other))
	}
}

// Copying back from the second copy is for getting files back: it never
// removes anything from where backups go, as in the panel.
func TestMirrorReverseNeverRemovesFromWhereBackupsGo(t *testing.T) {
	main, second := parityWorld(t, true)
	main.put("machines/testpc/Docs/current/only-here.txt")
	second.put("machines/testpc/Docs/current/from-second.txt")
	a, err := openApp()
	if err != nil {
		t.Fatal(err)
	}
	defer a.close()
	opts := &Options{Out: &bytes.Buffer{}, Err: &bytes.Buffer{}}
	if err := runMirror(context.Background(), a, opts, nil, mirrorFlags{reverse: true}); err != errNeedsYes || main.has("machines/testpc/Docs/current/from-second.txt") {
		t.Fatalf("copied back without asking: %v", err)
	}
	opts.Yes = true
	if err := runMirror(context.Background(), a, opts, nil, mirrorFlags{reverse: true, everything: true}); err != nil {
		t.Fatal(err)
	}
	if !main.has("machines/testpc/Docs/current/only-here.txt") || !main.has("machines/testpc/Docs/current/from-second.txt") {
		t.Fatal("copying back removed a file from where backups go, or did not copy")
	}
}

func TestRemovePurgeAsksForTheNameAndWhere(t *testing.T) {
	main, second := parityWorld(t, true, folder(t, "Docs"), folder(t, "Music"))
	main.put("machines/testpc/Docs/current/a", "machines/testpc/Music/current/m")
	second.put("machines/testpc/Docs/current/a", "machines/testpc/Music/current/m")

	if _, err := r2b(t, nil, "remove", "Docs", "--purge"); err != errNeedsYes {
		t.Fatalf("purged with nobody to ask: %v", err)
	}
	if _, err := r2b(t, conversation("Doc"), "remove", "Docs", "--purge"); err == nil || !main.has("machines/testpc/Docs/") {
		t.Fatalf("a wrong name deleted it: %v", err)
	}
	opts := conversation("Docs", "2")
	if _, err := r2b(t, opts, "remove", "Docs", "--purge"); err != nil {
		t.Fatalf("%v\n%s", err, said(opts))
	}
	if main.has("machines/testpc/Docs/") || !second.has("machines/testpc/Docs/") {
		t.Error("only here: the second copy should keep it")
	}
	if _, err := r2b(t, nil, "remove", "Music", "--purge", "--yes"); err != nil {
		t.Fatal(err)
	}
	if main.has("machines/testpc/Music/") || second.has("machines/testpc/Music/") {
		t.Error("--yes should delete it from both, as the panel's default")
	}
}

func TestEditRetentionTakesThePanelsChoices(t *testing.T) {
	parityWorld(t, false, folder(t, "Docs"))
	if _, err := r2b(t, nil, "edit", "Docs", "--retention", "45"); err == nil {
		t.Error("took 45 days, which the panel does not offer")
	}
	out, err := r2b(t, nil, "edit", "Docs", "--retention", "90")
	if err != nil || !strings.Contains(out, "kept for 90 days") {
		t.Fatalf("%q %v", out, err)
	}
	a, _ := openApp()
	defer a.close()
	if s, _ := a.sets.Get("Docs"); s.RetentionDays != 90 {
		t.Errorf("retention = %d", s.RetentionDays)
	}
}

// signInFake signs this computer in to the fake account, with a PIN.
func signInFake(t *testing.T) {
	t.Helper()
	c := accountClient()
	ctx := context.Background()
	ch, err := c.Verify(ctx, "me@example.com", "123456")
	if err != nil {
		t.Fatal(err)
	}
	var s *account.Session
	if ch.PINSet {
		s, err = c.VerifyPIN(ctx, ch, testPIN)
	} else {
		s, err = c.SetupPIN(ctx, ch.PINToken, testPIN)
	}
	if err != nil {
		t.Fatal(err)
	}
	if err := finishSignIn(ctx, s); err != nil {
		t.Fatal(err)
	}
}

func TestAccountLoginKeepsThisComputersKeysAndSavesThem(t *testing.T) {
	parityWorld(t, false)
	w, stop := newFakeWorker(t)
	defer stop()
	opts := conversation("me@example.com", "123456", testPIN, testPIN)
	if _, err := r2b(t, opts, "account", "login"); err != nil {
		t.Fatalf("%v\n%s", err, said(opts))
	}
	if !strings.Contains(said(opts), "This computer's keys are saved to your account") || w.sealed == nil {
		t.Fatalf("login did not save this computer's keys:\n%s", said(opts))
	}
	a, _ := openApp()
	c, _ := a.creds.Load()
	a.close()
	if c.Bucket != "main" {
		t.Errorf("signing in changed where backups go: %+v", c.Redacted())
	}

	out, err := r2b(t, nil, "account")
	if err != nil {
		t.Fatal(err)
	}
	// The fake service's token carries no email, so the screen says
	// "Signed in" where the real one names the address.
	for _, want := range []string{"Signed in", "Hosted account, locked with your PIN", "Backblaze B2 · main",
		"Pack small files", "Keys saved to your account", "Computers"} {
		if !strings.Contains(out, want) {
			t.Errorf("account screen lacks %q:\n%s", want, out)
		}
	}
}

func TestAccountScreenWithoutAnAccount(t *testing.T) {
	parityWorld(t, true)
	_ = setAccountMode(accountModeLocal)
	out, err := r2b(t, nil, "account")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"No account", "Keys stay on this computer", "Second copy", "Backblaze B2 · second",
		"Copy after every backup", "This computer", "testpc"} {
		if !strings.Contains(out, want) {
			t.Errorf("account screen lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "Keys saved to your account") || strings.Contains(out, "Keys not saved") {
		t.Errorf("talks about an account that is not there:\n%s", out)
	}
}

// Forgot PIN, from the PIN prompt: a fresh code, a new PIN, and the keys
// locked with the old one gone.
func TestSignInForgotPINResetsAfterAFreshCode(t *testing.T) {
	t.Setenv(config.EnvDataDir, t.TempDir())
	w, stop := newFakeWorker(t)
	defer stop()
	storeKeys(t, account.StoredKeys{Primary: &creds.Credentials{Provider: creds.ProviderDrime, DrimeToken: "t"}})

	opts := conversation("me@example.com", "123456", "", "y", "123456", "135790", "135790")
	s, err := signIn(context.Background(), newPrompter(opts), accountClient())
	if err != nil || s == nil {
		t.Fatalf("signIn: %v\n%s", err, said(opts))
	}
	if w.sealed != nil {
		t.Error("the keys locked with the old PIN are still kept")
	}
	for _, want := range []string{"Reset your PIN?", "Your other computers are signed out", "New PIN set."} {
		if !strings.Contains(said(opts), want) {
			t.Errorf("missing %q:\n%s", want, said(opts))
		}
	}
	// And changing your mind goes back to the PIN.
	_ = account.ClearToken()
	opts = conversation("me@example.com", "123456", "", "n", "135790")
	if _, err := signIn(context.Background(), newPrompter(opts), accountClient()); err != nil {
		t.Fatalf("keeping the PIN after all: %v\n%s", err, said(opts))
	}
}

func TestAccountResetPINSavesThisComputersKeysAgain(t *testing.T) {
	parityWorld(t, false)
	w, stop := newFakeWorker(t)
	defer stop()
	storeKeys(t, account.StoredKeys{Primary: &creds.Credentials{Provider: creds.ProviderDrime, DrimeToken: "t"}})
	opts := conversation("me@example.com", "y", "123456", "135790", "135790")
	if _, err := r2b(t, opts, "account", "reset-pin"); err != nil {
		t.Fatalf("%v\n%s", err, said(opts))
	}
	if w.sealed == nil || !strings.Contains(said(opts), "saved to your account, locked with the new PIN") {
		t.Fatalf("this computer's keys were not saved after the reset:\n%s", said(opts))
	}
}

func TestAccountSaveKeysLogoutAndRename(t *testing.T) {
	parityWorld(t, false)
	w, stop := newFakeWorker(t)
	defer stop()
	if _, err := r2b(t, nil, "account", "save-keys"); err == nil {
		t.Error("saved keys with nobody signed in")
	}
	signInFake(t)
	if out, err := r2b(t, nil, "account", "save-keys"); err != nil || w.sealed == nil {
		t.Fatalf("save-keys: %q %v", out, err)
	}

	if _, err := r2b(t, nil, "account", "logout"); err != errNeedsYes {
		t.Fatalf("signed out with nobody to ask: %v", err)
	}
	if out, _ := r2b(t, conversation("n"), "account", "logout"); !strings.Contains(out, "Still signed in") {
		t.Errorf("said no: %q", out)
	}
	if tok, _ := account.LoadToken(); tok == "" {
		t.Fatal("signed out after saying no")
	}
	if _, err := r2b(t, nil, "account", "logout", "--yes"); err != nil {
		t.Fatal(err)
	}
	if tok, _ := account.LoadToken(); tok != "" {
		t.Fatal("still signed in after --yes")
	}

	t.Setenv("R2BACKUP_MACHINE", "")
	if _, err := r2b(t, nil, "account", "rename", "a/b"); err == nil {
		t.Error("took a name with a slash")
	}
	if out, err := r2b(t, nil, "account", "rename", "Study"); err != nil || !strings.Contains(out, "Renamed to Study") {
		t.Fatalf("%q %v", out, err)
	}
	if machineName() != "Study" {
		t.Errorf("machine = %q", machineName())
	}
}

// Running setup again on a computer that keeps its keys to itself asks
// before moving where backups go, as the panel's Change… does.
func TestSetupAgainAsksBeforeMovingBackups(t *testing.T) {
	parityWorld(t, false)
	_, stop := newFakeWorker(t)
	defer stop()
	a, err := openApp()
	if err != nil {
		t.Fatal(err)
	}
	defer a.close()
	opts := conversation("", "n")
	if err := runSetup(context.Background(), a, opts, setupFlags{}); err != nil {
		t.Fatalf("%v\n%s", err, said(opts))
	}
	if !strings.Contains(said(opts), "Back up somewhere else?") || !strings.Contains(said(opts), "Backups still go to Backblaze B2 · main") {
		t.Fatalf("did not ask:\n%s", said(opts))
	}
	if strings.Contains(said(opts), "Where should backups go?") {
		t.Errorf("asked for a destination after being told no:\n%s", said(opts))
	}
}

// Signing in during setup on a computer that has keys keeps them.
func TestSetupSignInKeepsTheKeysAlreadyHere(t *testing.T) {
	parityWorld(t, false)
	w, stop := newFakeWorker(t)
	defer stop()
	storeKeys(t, account.StoredKeys{Primary: &creds.Credentials{Provider: creds.ProviderDrime, DrimeToken: "elsewhere"}})
	a, err := openApp()
	if err != nil {
		t.Fatal(err)
	}
	defer a.close()
	opts := conversation("me@example.com", "123456", testPIN)
	if err := runSetup(context.Background(), a, opts, setupFlags{}); err != nil {
		t.Fatalf("%v\n%s", err, said(opts))
	}
	if c, _ := a.creds.Load(); c.Bucket != "main" {
		t.Fatalf("setup replaced this computer's keys with the account's: %+v", c.Redacted())
	}
	if !strings.Contains(said(opts), "This computer's keys stay as they are") {
		t.Errorf("did not say the keys stay:\n%s", said(opts))
	}
	_ = w
}

func TestKeysImportSavesThemToTheAccount(t *testing.T) {
	parityWorld(t, false)
	w, stop := newFakeWorker(t)
	defer stop()
	file := filepath.Join(t.TempDir(), "keys.r2bkeys")
	if _, err := r2b(t, conversation("correct horse", "correct horse"), "keys", "export", file); err != nil {
		t.Fatal(err)
	}
	signInFake(t)
	w.sealed = nil
	if _, err := r2b(t, conversation("correct horse"), "keys", "import", file); err != nil {
		t.Fatal(err)
	}
	if w.sealed == nil {
		t.Error("imported keys were not saved to the account")
	}
}
