package cli

import (
	"context"
	"encoding/json"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestDrimeFolderFor(t *testing.T) {
	root := filepath.Join(t.TempDir(), "Drime", "Drime")
	cases := []struct {
		path string
		rel  string
		ok   bool
	}{
		{root, "", true},
		{filepath.Join(root, "Code Projects"), "Code Projects", true},
		{filepath.Join(root, "Photos", "2024"), "Photos/2024", true},
		{root + "-other", "", false},
		{filepath.Dir(root), "", false},
		{filepath.Join(t.TempDir(), "elsewhere"), "", false},
	}
	for _, c := range cases {
		rel, ok := drimeFolderFor(c.path, []string{root})
		if rel != c.rel || ok != c.ok {
			t.Errorf("%s: got %q %v, want %q %v", c.path, rel, ok, c.rel, c.ok)
		}
	}
	if runtime.GOOS == "windows" {
		if rel, ok := drimeFolderFor(strings.ToUpper(filepath.Join(root, "x")), []string{root}); !ok || rel != "X" {
			t.Errorf("Windows paths are not case sensitive: %q %v", rel, ok)
		}
	}
	if _, ok := drimeFolderFor(root, nil); ok {
		t.Error("no Drime on this PC, but a match")
	}
}

func TestPlatformDrimeSyncRootsDoesNotFail(t *testing.T) {
	// Real on Windows, where it reads the registry; nothing elsewhere. A
	// computer without Drime has no roots, and that is all CI can check.
	for _, r := range platformDrimeSyncRoots() {
		if !filepath.IsAbs(r) {
			t.Errorf("sync root %q is not a full path", r)
		}
	}
}

func fakeDrimeRoot(t *testing.T) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "Drime")
	drimeSyncRoots = func() []string { return []string{root} }
	t.Cleanup(func() { drimeSyncRoots = platformDrimeSyncRoots })
	return root
}

func TestAddCheckSaysAFolderIsInDrime(t *testing.T) {
	tb := trayForTest(t)
	root := fakeDrimeRoot(t)
	raw, _ := json.Marshal(map[string]string{"path": filepath.Join(root, "Photos")})
	out, err := tb.Call(context.Background(), "addCheck", raw)
	if err != nil {
		t.Fatal(err)
	}
	res := out.(addCheckOut)
	if !res.InDrime || res.DrimeFolder != "Photos" || !strings.Contains(res.DrimeNote, "downloads every file from Drime") {
		t.Fatalf("addCheck = %+v", res)
	}

	raw, _ = json.Marshal(map[string]string{"path": t.TempDir()})
	out, err = tb.Call(context.Background(), "addCheck", raw)
	if err != nil || out.(addCheckOut).InDrime {
		t.Fatalf("an ordinary folder: %+v %v", out, err)
	}
}
