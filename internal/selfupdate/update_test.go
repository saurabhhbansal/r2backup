package selfupdate

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func sum(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func TestNewerComparesNumericallyNotAsText(t *testing.T) {
	// The one that matters: "v1.10.0" < "v1.9.0" as strings, which would make
	// the updater refuse every release after .9 forever.
	cases := []struct {
		current, other string
		want           bool
	}{
		{"v1.9.0", "v1.10.0", true},
		{"v1.10.0", "v1.9.0", false},
		{"v1.2.3", "v1.2.4", true},
		{"v1.2.3", "v1.2.3", false},
		{"v1.2.3", "v1.2.2", false},
		{"v0.9.0", "v1.0.0", true},
		{"1.2.3", "v1.2.4", true},
		{"v1.2.3", "v1.2.4-rc1", true},
		{"dev", "v0.0.1", true},
		{"v2.0.0", "v1.99.99", false},
	}
	for _, tc := range cases {
		if got := Newer(tc.current, tc.other); got != tc.want {
			t.Errorf("Newer(%q, %q) = %v, want %v", tc.current, tc.other, got, tc.want)
		}
	}
}

func TestAssetNameMatchesTheReleaseArtifacts(t *testing.T) {
	if got := AssetName("v1.2.3", "windows", "amd64"); got != "r2backup_1.2.3_windows_amd64.zip" {
		t.Errorf("windows asset = %q", got)
	}
	if got := AssetName("1.2.3", "linux", "arm64"); got != "r2backup_1.2.3_linux_arm64.tar.gz" {
		t.Errorf("linux asset = %q", got)
	}
}

// applyInto exercises Apply against a throwaway "binary" by pointing the
// package at a copy of the test executable's directory.
func TestApplyRefusesAChecksumMismatchAndChangesNothing(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "r2backup-fake")
	original := []byte("the version that works")
	if err := os.WriteFile(target, original, 0o755); err != nil {
		t.Fatal(err)
	}

	err := applyTo(target, bytes.NewReader([]byte("a corrupted download")), sum([]byte("something else entirely")))
	if err == nil {
		t.Fatal("Apply accepted a binary whose checksum did not match")
	}
	if !strings.Contains(err.Error(), "checksum mismatch") {
		t.Errorf("error should name the mismatch, got: %v", err)
	}
	got, _ := os.ReadFile(target)
	if string(got) != string(original) {
		t.Fatal("a failed update damaged the working binary; the user would be left with nothing to run")
	}
	leftovers(t, dir, "r2backup-fake")
}

func TestApplyReplacesTheBinaryAndKeepsPermissions(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "r2backup-fake")
	if err := os.WriteFile(target, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	next := []byte("new and improved")
	if err := applyTo(target, bytes.NewReader(next), sum(next)); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(next) {
		t.Fatalf("binary was not replaced: %q", got)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(target)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm()&0o100 == 0 {
			t.Error("the replacement is not executable; the next scheduled run would fail")
		}
	}
}

// TestAtMostOneStaleFileEverAccumulates is the user's actual question about
// this feature: does junk pile up on the machine after every update?
func TestAtMostOneStaleFileEverAccumulates(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "r2backup-fake")
	if err := os.WriteFile(target, []byte("v0"), 0o755); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 25; i++ {
		body := []byte{byte(i)}
		if err := applyTo(target, bytes.NewReader(body), sum(body)); err != nil {
			t.Fatalf("update %d: %v", i, err)
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		// The binary itself, plus at most one .old on Windows. Never more,
		// because the suffix is fixed rather than numbered.
		if len(entries) > 2 {
			var names []string
			for _, e := range entries {
				names = append(names, e.Name())
			}
			t.Fatalf("after %d updates the directory holds %v -- stale binaries are accumulating", i, names)
		}
	}
}

func TestApplyLeavesNoTempFileOnSuccess(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "r2backup-fake")
	if err := os.WriteFile(target, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	body := []byte("new")
	if err := applyTo(target, bytes.NewReader(body), sum(body)); err != nil {
		t.Fatal(err)
	}
	leftovers(t, dir, "r2backup-fake")
}

// leftovers fails if anything besides the binary (and, on Windows, its single
// .old) is sitting in the directory.
func leftovers(t *testing.T, dir, binary string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		name := e.Name()
		if name == binary || name == binary+OldSuffix {
			continue
		}
		t.Errorf("unexpected file left behind: %q", name)
	}
}

func TestCleanupIsSafeWhenThereIsNothingToClean(t *testing.T) {
	// Called at the start of every run, including the very first one.
	if err := Cleanup(); err != nil {
		t.Fatalf("Cleanup on a fresh install should do nothing quietly: %v", err)
	}
}

// TestLatestDoesNotClaimThereAreNoReleasesWhenItCannotSee reproduces what a
// private repository actually returns. GitHub answers 404 for "this repo has
// no releases" and for "you cannot see this repo" identically, so the message
// must not pick one. r2backup's own repository is private and has a published
// release, which is exactly the case the old message got backwards -- it told
// the user no release existed while its release page listed one.
func TestLatestDoesNotClaimThereAreNoReleasesWhenItCannotSee(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
	}))
	defer srv.Close()

	restore, restoreSite := githubAPI, siteAPI
	githubAPI, siteAPI = srv.URL, srv.URL
	defer func() { githubAPI, siteAPI = restore, restoreSite }()

	_, err := Latest(context.Background(), "someone/private")
	if err == nil {
		t.Fatal("Latest returned no error for a 404")
	}
	msg := err.Error()
	for _, want := range []string{"none has been published", "private"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error does not mention %q, so it states one cause as if it were certain: %s", want, msg)
		}
	}
}

const listingJSON = `{"tag_name":"v9.9.9","body":"notes","assets":[
 {"name":"checksums.txt","browser_download_url":"%[1]s/download/v9.9.9/checksums.txt"},
 {"name":"r2backup_9.9.9_windows_amd64.zip","browser_download_url":"%[1]s/download/v9.9.9/r2backup_9.9.9_windows_amd64.zip"},
 {"name":"r2backup_9.9.9_linux_amd64.tar.gz","browser_download_url":"%[1]s/download/v9.9.9/r2backup_9.9.9_linux_amd64.tar.gz"}]}`

// The site is asked first, so a network that blocks GitHub still gets updates,
// and GitHub is not asked at all when the site answers.
func TestLatestPrefersTheSite(t *testing.T) {
	githubHits := 0
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		githubHits++
		http.Error(w, "should not be asked", http.StatusTeapot)
	}))
	defer gh.Close()
	var site *httptest.Server
	site = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/release/latest.json" {
			http.NotFound(w, r)
			return
		}
		fmt.Fprintf(w, listingJSON, site.URL)
	}))
	defer site.Close()

	restore, restoreSite := githubAPI, siteAPI
	githubAPI, siteAPI = gh.URL, site.URL
	defer func() { githubAPI, siteAPI = restore, restoreSite }()

	rel, err := Latest(context.Background(), "someone/repo")
	if err != nil {
		t.Fatal(err)
	}
	if rel.Version != "v9.9.9" || rel.Checksums != site.URL+"/download/v9.9.9/checksums.txt" {
		t.Errorf("got %+v", rel)
	}
	if got := rel.Assets["windows_amd64"]; got != site.URL+"/download/v9.9.9/r2backup_9.9.9_windows_amd64.zip" {
		t.Errorf("windows asset = %q", got)
	}
	if githubHits != 0 {
		t.Errorf("GitHub was asked %d times although the site answered", githubHits)
	}
}

// A site that is down falls back to GitHub, as before the site existed.
func TestLatestFallsBackToGitHub(t *testing.T) {
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "down", http.StatusBadGateway)
	}))
	defer site.Close()
	var gh *httptest.Server
	gh = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/someone/repo/releases/latest" {
			http.NotFound(w, r)
			return
		}
		fmt.Fprintf(w, listingJSON, gh.URL)
	}))
	defer gh.Close()

	restore, restoreSite := githubAPI, siteAPI
	githubAPI, siteAPI = gh.URL, site.URL
	defer func() { githubAPI, siteAPI = restore, restoreSite }()

	rel, err := Latest(context.Background(), "someone/repo")
	if err != nil {
		t.Fatal(err)
	}
	if rel.Version != "v9.9.9" || !strings.HasPrefix(rel.Assets["linux_amd64"], gh.URL) {
		t.Errorf("got %+v", rel)
	}
}
