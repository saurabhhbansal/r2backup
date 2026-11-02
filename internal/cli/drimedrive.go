package cli

import (
	"path/filepath"
	"strings"
)

// A backup folder inside Drime's synced folder on this PC is files that are
// already in Drime, mostly as online-only placeholders. Backing it up from
// here makes Drime's app download every one of them first, only to upload
// them again. Copying the same folder from Drime itself (Drime folders) does
// it without the download -- and with Ultra fast mode, without this PC at
// all. Adding such a folder says so.

// drimeSyncRoots lists the folders Drime's Windows app syncs. Tests replace
// it.
var drimeSyncRoots = platformDrimeSyncRoots

// drimeFolderFor reports whether path is inside one of roots, and if so the
// folder's path in Drime ("" for the whole drive).
func drimeFolderFor(path string, roots []string) (rel string, ok bool) {
	p := filepath.Clean(path)
	for _, r := range roots {
		r = filepath.Clean(r)
		if r == "." || r == "" {
			continue
		}
		if strings.EqualFold(p, r) {
			return "", true
		}
		prefix := r
		if !strings.HasSuffix(prefix, string(filepath.Separator)) {
			prefix += string(filepath.Separator)
		}
		if len(p) > len(prefix) && strings.EqualFold(p[:len(prefix)], prefix) {
			return filepath.ToSlash(p[len(prefix):]), true
		}
	}
	return "", false
}

// drimeDriveNote is what adding a folder from Drime's synced folder says.
func drimeDriveNote(rel string) string {
	where := "Drime"
	if rel != "" {
		where = rel + " in Drime"
	}
	return "This folder is in Drime's synced folder on this PC. Backing it up from here downloads every file from Drime first. " +
		"To keep a copy of it, copy " + where + " with Drime folders instead; with Ultra fast mode on, nothing goes through this PC."
}
