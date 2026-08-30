// Package e2e runs full backup -> mutate -> backup -> restore -> byte-compare
// cycles against a real S3-compatible server (test/minio), the same way
// internal/backup and internal/restore's own integration tests do, except
// every test here is built around one specific platform edge case that has
// broken a real backup tool: a path over Windows' MAX_PATH, a name Windows
// cannot create at all, a file that changes out from under the upload
// reading it, a tree too large to hold any assumption about ordering.
//
// A case that cannot exist on the platform running the test (a symlink farm
// on a machine with no symlink privilege, say) is skipped with a message
// naming exactly why -- never silently passed, because a skip that doesn't
// say why is indistinguishable from a test nobody wrote.
package e2e

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/saurabhhbansal/r2backup/internal/backup"
	"github.com/saurabhhbansal/r2backup/internal/index"
	"github.com/saurabhhbansal/r2backup/internal/remote"
	"github.com/saurabhhbansal/r2backup/internal/restore"
	"github.com/saurabhhbansal/r2backup/internal/sets"
	tminio "github.com/saurabhhbansal/r2backup/test/minio"
)

// harness bundles what nearly every test here needs: a real MinIO-backed
// client, a fresh index, and a set pointed at a fresh local tree. It mirrors
// internal/backup/backup_test.go's harness deliberately -- these are the
// same three collaborators, just assembled once per package instead of once
// per test file.
type harness struct {
	client *remote.Client
	db     *index.DB
	set    sets.Set
	root   string
}

// newHarness builds a harness with an empty root directory; the caller
// populates it (usually via fixtures.Build) before calling backup.
func newHarness(t *testing.T, name string) *harness {
	t.Helper()
	return newHarnessWithClient(t, name, nil)
}

// newHarnessWithClient works like newHarness but lets the caller install a
// custom remote.Config (a hooked or throttled HTTPClient, most often) via
// test/minio.StartWithConfig.
func newHarnessWithClient(t *testing.T, name string, mutate func(*remote.Config)) *harness {
	t.Helper()
	root := t.TempDir()
	db, err := index.Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	// The index is attached as the place unfinished uploads are recorded,
	// because app.connect does it for every real command and this suite is
	// here to run what the product runs. Without it the large-file cases --
	// the only ones that go up in parts at all -- would exercise a multipart
	// path no user has. The caller's mutate runs afterwards so a test can
	// still take it away deliberately.
	client, cleanup := tminio.StartWithConfig(t, func(cfg *remote.Config) {
		cfg.Resume = backup.ResumeStoreFor(db)
		if mutate != nil {
			mutate(cfg)
		}
	})
	t.Cleanup(cleanup)

	return &harness{
		client: client,
		db:     db,
		root:   root,
		set: sets.Set{
			Name:          name,
			Prefix:        "machines/e2e-pc/" + name,
			Root:          root,
			Machine:       "e2e-pc",
			RetentionDays: sets.DefaultRetentionDays,
		},
	}
}

// backupRun runs one backup and fails the test on an unexpected error. It
// does not assert Succeeded() -- several cases here exist precisely to prove
// a run can finish with reported Failures or Problems without stopping.
func (h *harness) backupRun(t *testing.T, opts ...func(*backup.Options)) *backup.Report {
	t.Helper()
	o := backup.Options{
		Set:         h.set,
		Index:       h.db,
		Client:      h.client,
		DetectMoves: true,
	}
	for _, fn := range opts {
		fn(&o)
	}
	rep, err := backup.Run(context.Background(), o)
	if err != nil {
		t.Fatalf("backup.Run: %v", err)
	}
	return rep
}

// restoreInto restores h's set into target, a fresh directory the test
// controls -- never h.root, so the original tree survives for comparison.
func (h *harness) restoreInto(t *testing.T, target string, opts ...func(*restore.Options)) *restore.Report {
	t.Helper()
	o := restore.Options{
		Set:    h.set,
		Client: h.client,
		Target: target,
	}
	for _, fn := range opts {
		fn(&o)
	}
	rep, err := restore.Run(context.Background(), o)
	if err != nil {
		t.Fatalf("restore.Run: %v", err)
	}
	return rep
}

// currentPrefix is the object-key prefix everything live sits under, for
// tests that inject or inspect objects directly rather than only going
// through backup.Run.
func (h *harness) currentPrefix() string {
	return h.set.Prefix + "/current/"
}
