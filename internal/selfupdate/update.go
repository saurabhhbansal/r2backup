// Package selfupdate replaces this binary in place.
//
// This is the part the previous product got wrong, and the reason it is worth
// its own package. That build was a onefile PyInstaller executable installed by
// Inno Setup. A running copy held its own .exe open; Windows Restart Manager
// closes an application by asking its window to shut, and the GUI hid to the
// tray instead of exiting while the service had no window at all; so Setup fell
// back to replacing files on reboot and told the user to restart.
//
// None of that mechanism exists here. There is no installer and no resident
// process, so updating is a file swap. On Unix a running binary can be unlinked
// outright. On Windows it cannot be overwritten -- but it can be renamed, which
// is all this needs.
package selfupdate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// OldSuffix is appended to the outgoing binary on Windows.
//
// It is a fixed name on purpose. A counter or a timestamp would accumulate one
// file per update forever; with a single fixed name, a hundred updates can
// leave at most one stale file behind.
const OldSuffix = ".old"

// Release is a published version.
type Release struct {
	Version string
	// Assets maps "<goos>_<goarch>" to a download URL.
	Assets map[string]string
	// Checksums is the URL of the checksums file.
	Checksums string
	Notes     string
}

// AssetName is the release asset for a platform.
func AssetName(version, goos, goarch string) string {
	name := fmt.Sprintf("r2backup_%s_%s_%s", strings.TrimPrefix(version, "v"), goos, goarch)
	if goos == "windows" {
		return name + ".zip"
	}
	return name + ".tar.gz"
}

// Platform is this binary's platform key.
func Platform() string { return runtime.GOOS + "_" + runtime.GOARCH }

// Cleanup removes the previous binary left beside this one by an earlier
// update. Call it early in every run: the scheduler runs every thirty minutes,
// so a stale file survives at most that long without anyone doing anything.
//
// A failure here is never worth failing a backup over, so it reports what
// happened and callers may ignore it.
func Cleanup() error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	old := self + OldSuffix
	err = os.Remove(old)
	switch {
	case err == nil:
		return nil
	case os.IsNotExist(err):
		return nil
	default:
		// Still running, still locked, or not ours. The reboot-time deletion
		// scheduled at update time is the backstop.
		return fmt.Errorf("remove previous binary %q: %w", old, err)
	}
}

// Latest asks GitHub for the newest release.
func Latest(ctx context.Context, repo string) (*Release, error) {
	url := "https://api.github.com/repos/" + repo + "/releases/latest"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return nil, fmt.Errorf("check for updates: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("no releases published for %s yet", repo)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("check for updates: github returned %s", resp.Status)
	}

	var payload struct {
		TagName string `json:"tag_name"`
		Body    string `json:"body"`
		Assets  []struct {
			Name string `json:"name"`
			URL  string `json:"browser_download_url"`
		} `json:"assets"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&payload); err != nil {
		return nil, fmt.Errorf("parse release listing: %w", err)
	}

	rel := &Release{Version: payload.TagName, Notes: payload.Body, Assets: map[string]string{}}
	for _, a := range payload.Assets {
		if a.Name == "checksums.txt" {
			rel.Checksums = a.URL
			continue
		}
		for _, plat := range []string{
			"windows_amd64", "windows_arm64",
			"linux_amd64", "linux_arm64",
			"darwin_amd64", "darwin_arm64",
		} {
			parts := strings.SplitN(plat, "_", 2)
			if strings.Contains(a.Name, parts[0]) && strings.Contains(a.Name, parts[1]) {
				rel.Assets[plat] = a.URL
			}
		}
	}
	return rel, nil
}

// Newer reports whether other is a later version than current. Both are
// compared as dotted integers, so v1.10.0 correctly beats v1.9.0 -- a string
// comparison would get that backwards.
func Newer(current, other string) bool {
	c := parseVersion(current)
	o := parseVersion(other)
	for i := 0; i < 3; i++ {
		if o[i] != c[i] {
			return o[i] > c[i]
		}
	}
	return false
}

func parseVersion(v string) [3]int {
	var out [3]int
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		v = v[:i]
	}
	for i, part := range strings.SplitN(v, ".", 3) {
		if i > 2 {
			break
		}
		n := 0
		for _, r := range part {
			if r < '0' || r > '9' {
				break
			}
			n = n*10 + int(r-'0')
		}
		out[i] = n
	}
	return out
}

// Apply replaces the running binary with the contents of newBinary.
//
// The order matters and is the whole point: the replacement is written and
// verified beside the target first, so a failed download can never leave the
// user without a working binary.
func Apply(newBinary io.Reader, wantSHA256 string) error {
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate the running binary: %w", err)
	}
	self, err = filepath.EvalSymlinks(self)
	if err != nil {
		return fmt.Errorf("resolve %q: %w", self, err)
	}
	return applyTo(self, newBinary, wantSHA256)
}

// applyTo is Apply against an explicit path, so the swap can be tested without
// replacing the test binary itself.
func applyTo(self string, newBinary io.Reader, wantSHA256 string) error {
	dir := filepath.Dir(self)

	staged, err := os.CreateTemp(dir, ".r2backup-new-*")
	if err != nil {
		return fmt.Errorf("stage the new binary beside %q: %w", self, err)
	}
	stagedName := staged.Name()
	defer os.Remove(stagedName)

	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(staged, h), newBinary); err != nil {
		staged.Close()
		return fmt.Errorf("download the new binary: %w", err)
	}
	if err := staged.Close(); err != nil {
		return fmt.Errorf("finish writing %q: %w", stagedName, err)
	}

	if wantSHA256 != "" {
		got := hex.EncodeToString(h.Sum(nil))
		if !strings.EqualFold(got, wantSHA256) {
			return fmt.Errorf("checksum mismatch: expected %s, got %s. The download was not what it claimed to be, so nothing was replaced", wantSHA256, got)
		}
	}
	// Match the existing binary's permissions rather than the temp file's.
	if info, err := os.Stat(self); err == nil {
		if err := os.Chmod(stagedName, info.Mode()); err != nil {
			return fmt.Errorf("set permissions on %q: %w", stagedName, err)
		}
	}
	return swap(self, stagedName)
}
