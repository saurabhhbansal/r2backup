package cli

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/saurabhhbansal/r2backup/internal/cfapi"
	"github.com/saurabhhbansal/r2backup/internal/config"
	"github.com/saurabhhbansal/r2backup/internal/creds"
	"github.com/saurabhhbansal/r2backup/internal/oauth"
	"github.com/saurabhhbansal/r2backup/internal/sets"
)

func TestSuggestName(t *testing.T) {
	cases := map[string]string{
		filepath.Join("C:", "Users", "me", "Photos"): "Photos",
		filepath.Join("home", "me", "My Docs"):       "My Docs",
	}
	if runtime.GOOS == "windows" {
		cases[`D:\`] = "Drive D"
		cases[`e:\`] = "Drive E"
	}
	for in, want := range cases {
		if got := suggestName(in); got != want {
			t.Errorf("suggestName(%q) = %q, want %q", in, got, want)
		}
	}
	if got := suggestName("/"); sets.ValidName(got) != nil {
		t.Errorf("the name suggested for a root, %q, is not a valid name", got)
	}
}

func TestToggleExclude(t *testing.T) {
	cases := []struct {
		name    string
		in      []string
		path    string
		exclude bool
		want    []string
		err     bool
	}{
		{"leave a folder out", nil, "node_modules", true, []string{"node_modules"}, false},
		{"slashes do not matter", nil, "/build/", true, []string{"build"}, false},
		{"put it back", []string{"build", "x"}, "build", false, []string{"x"}, false},
		{"a folder replaces rules inside it", []string{"build/cache", "build/tmp", "docs"}, "build", true, []string{"build", "docs"}, false},
		{"putting a folder back clears rules inside it", []string{"build/cache", "docs"}, "build", false, []string{"docs"}, false},
		{"a name that only starts the same is kept", []string{"build2"}, "build", true, []string{"build", "build2"}, false},
		{"left out from above cannot change here", []string{"build"}, "build/cache", false, nil, true},
		{"nothing chosen", nil, "/", true, nil, true},
	}
	for _, c := range cases {
		got, err := toggleExclude(c.in, c.path, c.exclude)
		if (err != nil) != c.err {
			t.Errorf("%s: err = %v", c.name, err)
			continue
		}
		if !c.err && !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

func TestLastProblem(t *testing.T) {
	cases := []struct{ out, want string }{
		{"", ""},
		{"Error: no credentials on this machine yet. Run: r2b setup\n", "no credentials on this machine yet. Run: r2b setup"},
		// r2b backup names each folder that failed, then a count that says less.
		{"Photos: the folder this set points at no longer exists\n  Nothing was deleted. When you know where it is: r2b relink \"Photos\" <new-path>\nError: 1 of 2 set(s) did not finish\n",
			"Photos: the folder this set points at no longer exists"},
		{"warning\r\nError: something else\r\n", "something else"},
	}
	for _, c := range cases {
		if got := lastProblem(c.out); got != c.want {
			t.Errorf("lastProblem(%q) = %q, want %q", c.out, got, c.want)
		}
	}
}

func TestTailWriterKeepsTheEnd(t *testing.T) {
	w := &tailWriter{}
	_, _ = w.Write([]byte(strings.Repeat("x", tailMax)))
	_, _ = w.Write([]byte("the end"))
	if len(w.buf) != tailMax || !strings.HasSuffix(string(w.buf), "the end") {
		t.Fatalf("kept %d bytes ending %q", len(w.buf), string(w.buf[len(w.buf)-10:]))
	}
}

func TestMirrorMustBeSomewhereElse(t *testing.T) {
	r2 := creds.Credentials{AccountID: "abc", Bucket: "b", AccessKeyID: "k", SecretAccessKey: "s"}
	other := r2
	other.Bucket = "b2"
	upper := r2
	upper.AccountID = "ABC"
	upper.AccessKeyID = "different key, same bucket"
	drime := creds.Credentials{Provider: creds.ProviderDrime, DrimeToken: "t"}
	drime2 := creds.Credentials{Provider: creds.ProviderDrime, DrimeToken: " t "}
	if !samePlace(r2, upper) {
		t.Error("the same bucket through other keys is the same place")
	}
	if samePlace(r2, other) || samePlace(r2, drime) {
		t.Error("another bucket, or Drime, is somewhere else")
	}
	if !samePlace(drime, drime2) {
		t.Error("the same Drime token is the same place")
	}
	if got := destLabel(drime); got != "Drime · r2backup" {
		t.Errorf("destLabel(drime) = %q", got)
	}
	if got := destLabel(r2); got != "Cloudflare R2 · b" {
		t.Errorf("destLabel(r2) = %q", got)
	}
}

func TestCloudflareSignInErrorsSayWhatToDo(t *testing.T) {
	for _, err := range []error{oauth.ErrDenied, oauth.ErrNoBrowser, oauth.ErrPortsBusy,
		context.DeadlineExceeded, cfapi.ErrNoAccounts, cfapi.ErrUnauthorized, errors.New("boom")} {
		msg := cfError(err).Error()
		if strings.Contains(msg, "oauth:") || strings.Contains(msg, "cfapi:") {
			t.Errorf("%v reads as %q, which shows the package name", err, msg)
		}
	}
}

// trayForTest is a taskbar backend on a fresh data directory, with nothing
// connected: what the panel's folder requests can do without a network.
func trayForTest(t *testing.T) *trayBackend {
	t.Helper()
	t.Setenv(config.EnvDataDir, t.TempDir())
	t.Setenv("R2BACKUP_MACHINE", "testpc")
	d, err := openDashboard(&Options{Out: os.Stderr, Err: os.Stderr})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(d.close)
	return &trayBackend{d: d}
}

func call(t *testing.T, tb *trayBackend, method string, args any) (any, error) {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	return tb.Call(context.Background(), method, raw)
}

func addSet(t *testing.T, tb *trayBackend, name, root string) {
	t.Helper()
	if err := tb.d.app.sets.Add(sets.Set{Name: name, Root: root, Machine: "testpc",
		Prefix: "machines/testpc/" + name, RetentionDays: 30}); err != nil {
		t.Fatal(err)
	}
}

func TestAddCheckSuggestsANameThatWorks(t *testing.T) {
	tb := trayForTest(t)
	base := t.TempDir()
	photos := filepath.Join(base, "Photos")
	if err := os.MkdirAll(filepath.Join(photos, "2026"), 0o755); err != nil {
		t.Fatal(err)
	}
	addSet(t, tb, "Photos", filepath.Join(t.TempDir(), "elsewhere"))
	addSet(t, tb, "Base", base)

	out, err := call(t, tb, "addCheck", map[string]string{"path": photos})
	if err != nil {
		t.Fatal(err)
	}
	res := out.(addCheckOut)
	if res.Name != "Photos 2" {
		t.Errorf("name = %q: Photos is taken, so it should be numbered", res.Name)
	}
	if !res.Exists {
		t.Error("the folder is there")
	}
	if res.Overlaps != "Base" {
		t.Errorf("overlaps = %q, want Base, which already covers it", res.Overlaps)
	}
}

func TestRenameRelinkAndRetentionFromThePanel(t *testing.T) {
	tb := trayForTest(t)
	gone := filepath.Join(t.TempDir(), "gone")
	addSet(t, tb, "Photos", gone)
	addSet(t, tb, "Docs", t.TempDir())
	if err := tb.d.app.sets.MarkNeedsAttention("Photos", "the folder this set points at no longer exists"); err != nil {
		t.Fatal(err)
	}

	info, err := call(t, tb, "folderInfo", map[string]string{"name": "Photos"})
	if err != nil {
		t.Fatal(err)
	}
	m := info.(map[string]any)
	if m["missing"] != true || strings.Contains(m["note"].(string), "set points at") {
		t.Errorf("folderInfo = %v: it should say the folder is missing, in the panel's words", m)
	}

	if _, err := call(t, tb, "folderRename", map[string]string{"name": "Photos", "to": "Docs"}); err == nil {
		t.Error("renaming onto a name in use should be refused")
	}
	if _, err := call(t, tb, "folderRename", map[string]string{"name": "Photos", "to": "a/b"}); err == nil {
		t.Error("a name with a slash should be refused")
	}
	if _, err := call(t, tb, "folderRename", map[string]string{"name": "Photos", "to": "Pictures"}); err != nil {
		t.Fatal(err)
	}
	s, err := tb.d.app.sets.Get("Pictures")
	if err != nil {
		t.Fatal(err)
	}
	if s.Prefix != "machines/testpc/Photos" {
		t.Errorf("prefix = %q: renaming must not move what is stored", s.Prefix)
	}
	info, _ = call(t, tb, "folderInfo", map[string]string{"name": "Pictures"})
	if info.(map[string]any)["storedAs"] != "Photos" {
		t.Errorf("folderInfo should say other computers still see it as Photos: %v", info)
	}

	if _, err := call(t, tb, "folderRelink", map[string]string{"name": "Pictures", "path": filepath.Join(gone, "still-not")}); err == nil {
		t.Error("relinking to a folder that is not there should be refused")
	}
	found := t.TempDir()
	if _, err := call(t, tb, "folderRelink", map[string]string{"name": "Pictures", "path": found}); err != nil {
		t.Fatal(err)
	}
	s, _ = tb.d.app.sets.Get("Pictures")
	if s.Root != found || s.Status == sets.StatusNeedsAttention {
		t.Errorf("after relink: root %q, status %q", s.Root, s.Status)
	}

	if _, err := call(t, tb, "folderRetention", map[string]any{"name": "Pictures", "days": 90}); err != nil {
		t.Fatal(err)
	}
	if s, _ = tb.d.app.sets.Get("Pictures"); s.RetentionDays != 90 {
		t.Errorf("retention = %d, want 90", s.RetentionDays)
	}
	for _, bad := range []int{0, -1, 3} {
		if _, err := call(t, tb, "folderRetention", map[string]any{"name": "Pictures", "days": bad}); err == nil {
			t.Errorf("retention %d should be refused: turning the trash off here would hide what is in it", bad)
		}
	}
}

func TestChoosingWhatToLeaveOutBeforeAdding(t *testing.T) {
	tb := trayForTest(t)
	root := t.TempDir()
	for _, f := range []string{"keep.txt", "node_modules/a.js", "src/main.go"} {
		p := filepath.Join(root, filepath.FromSlash(f))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	out, err := call(t, tb, "excludeToggle", map[string]any{"excludes": []string{}, "path": "node_modules", "exclude": true})
	if err != nil {
		t.Fatal(err)
	}
	ex := out.([]string)
	out, err = call(t, tb, "folderList", map[string]any{"path": root, "excludes": ex})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, e := range out.(folderListOut).Entries {
		got[e.Name] = e.State
	}
	want := map[string]string{"node_modules": "off", "src": "on", "keep.txt": "on"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("entries = %v, want %v", got, want)
	}
}

func TestPanelRequestsThatNeedSomethingFirst(t *testing.T) {
	tb := trayForTest(t)
	if _, err := call(t, tb, "backupStop", nil); err == nil {
		t.Error("stopping with nothing running should say so")
	}
	if _, err := call(t, tb, "mirrorAuto", map[string]bool{"on": true}); err == nil {
		t.Error("mirroring automatically with no second copy set up should be refused")
	}
	if _, err := call(t, tb, "mirrorSetup", map[string]string{"provider": "drime", "token": "t"}); err == nil ||
		!strings.Contains(err.Error(), "choose where backups go first") {
		t.Errorf("a second copy before the first is connected: %v", err)
	}
	if err := tb.d.app.creds.Save(creds.Credentials{Provider: creds.ProviderDrime, DrimeToken: "t"}); err != nil {
		t.Fatal(err)
	}
	if _, err := call(t, tb, "mirrorSetup", map[string]string{"provider": "drime", "token": " t "}); err == nil ||
		!strings.Contains(err.Error(), "already go") {
		t.Errorf("a second copy in the same place: %v", err)
	}
	out, err := call(t, tb, "mirrorInfo", nil)
	if err != nil {
		t.Fatal(err)
	}
	info := out.(mirrorInfoOut)
	if info.Set || info.Suggest != "r2" || info.Main != "Drime · r2backup" || !info.Job.Idle {
		t.Errorf("mirrorInfo = %+v", info)
	}
	if _, err := call(t, tb, "mirrorRun", nil); err == nil {
		t.Error("copying with no folders should be refused")
	}
}
