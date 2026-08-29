package restore

import (
	"time"

	"github.com/saurabhhbansal/r2backup/internal/remote"
	"github.com/saurabhhbansal/r2backup/internal/sets"
)

// fileMeta returns ordinary regular-file metadata for a fake object: a
// fixed mtime and a non-executable mode, which is what classify needs to
// see to call it a file rather than an empty-directory marker.
func fileMeta() remote.Metadata {
	return remote.Metadata{ModTime: time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC), Mode: 0o644}
}

// dirMeta returns the metadata backup writes for an empty-directory
// marker: zero size and an owner-executable mode. See classify's doc for
// why the executable bit is the signal.
func dirMeta() remote.Metadata {
	return remote.Metadata{ModTime: time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC), Mode: 0o755}
}

// symlinkMeta returns the metadata backup writes for a symlink pointing at
// target.
func symlinkMeta(target string) remote.Metadata {
	return remote.Metadata{ModTime: time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC), Mode: 0o777, Symlink: target}
}

// testSet builds a minimal sets.Set for tests that don't need any of the
// bookkeeping fields.
func testSet(name, root string) sets.Set {
	return sets.Set{Name: name, Prefix: name, Root: root, Machine: "test-pc"}
}
