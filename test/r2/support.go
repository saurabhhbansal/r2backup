//go:build realr2

// Package r2 exercises the product against a real Cloudflare R2 bucket
// instead of MinIO. It exists because MinIO and real R2 disagree on things
// that only matter under production load: multipart part-count and
// part-size limits, the exact error shapes R2 returns for a throttle or a
// missing key, and metadata casing. test/e2e and every integration test
// under internal/ catch everything else against MinIO, cheaply and on
// every CI run; this suite is deliberately the expensive, credential-gated
// exception, meant to run before a release rather than on every commit.
//
// Every test here reads its bucket and credentials from four environment
// variables and skips cleanly, naming exactly which are missing, when they
// are not set:
//
//	R2BACKUP_TEST_ACCOUNT_ID
//	R2BACKUP_TEST_ACCESS_KEY_ID
//	R2BACKUP_TEST_SECRET_ACCESS_KEY
//	R2BACKUP_TEST_BUCKET
//
// The whole package sits behind the "realr2" build tag so `go test ./...`
// never touches a real bucket by accident; it must be requested explicitly
// with `go test -tags realr2 ./test/r2/...`.
package r2

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/saurabhhbansal/r2backup/internal/backup"
	"github.com/saurabhhbansal/r2backup/internal/index"
	"github.com/saurabhhbansal/r2backup/internal/remote"
	"github.com/saurabhhbansal/r2backup/internal/restore"
	"github.com/saurabhhbansal/r2backup/internal/sets"
)

// envAccountID etc. name the four variables this suite reads. Named as
// constants, rather than inlined, so a future reader (or a CI workflow
// file) can grep for the exact spelling once.
const (
	envAccountID = "R2BACKUP_TEST_ACCOUNT_ID"
	envAccessKey = "R2BACKUP_TEST_ACCESS_KEY_ID"
	envSecretKey = "R2BACKUP_TEST_SECRET_ACCESS_KEY"
	envBucket    = "R2BACKUP_TEST_BUCKET"
)

// harness is this package's equivalent of test/e2e's harness, rewired for a
// real bucket: it carries a unique prefix instead of a throwaway local
// server, and it always registers its own teardown before doing anything
// else, so a bucket is never left holding this run's objects even when the
// test that created them fails outright.
type harness struct {
	client *remote.Client
	db     *index.DB
	set    sets.Set
	root   string
	prefix string
}

// newHarness skips the calling test, naming every missing environment
// variable, if the four credentials are not fully set. Otherwise it builds
// a client against the real bucket, picks a prefix unique to this run, and
// registers a cleanup that deletes everything under that prefix -- run via
// t.Cleanup, so it fires whether the test passes, fails, or calls
// t.Fatal partway through building its own fixture.
func newHarness(t *testing.T, name string) *harness {
	t.Helper()

	cfg, missing := configFromEnv()
	if len(missing) > 0 {
		t.Skipf("realr2 suite needs credentials this environment does not have; missing: %v", missing)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	client, err := remote.New(ctx, cfg)
	if err != nil {
		t.Fatalf("build real R2 client: %v", err)
	}

	prefix, err := uniquePrefix(name)
	if err != nil {
		t.Fatalf("build a unique test prefix: %v", err)
	}

	// Registered before a single object exists under prefix, and before the
	// index or the local tree, so a failure anywhere below this line still
	// leaves the bucket clean. deleteAll only ever touches keys under
	// prefix -- see its doc -- so there is no risk of it reaching into
	// anything else this bucket holds.
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cleanupCancel()
		if err := deleteAll(cleanupCtx, client, prefix); err != nil {
			// Not t.Fatal: the test itself is already over by the time
			// Cleanup runs, and a failed cleanup should still be visible
			// without masking whatever the test itself reported.
			t.Errorf("cleanup: failed to delete everything under %q: %v", prefix, err)
		}
	})

	root := t.TempDir()
	db, err := index.Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatalf("open index: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	return &harness{
		client: client,
		db:     db,
		root:   root,
		prefix: prefix,
		set: sets.Set{
			Name:          name,
			Prefix:        prefix + "/" + name,
			Root:          root,
			Machine:       "realr2-ci",
			RetentionDays: sets.DefaultRetentionDays,
		},
	}
}

// configFromEnv reads the four credentials, returning the names of any that
// are unset (blank counts as unset -- a workflow that exports an empty
// string by mistake should skip the same way as one that never set the
// variable at all).
func configFromEnv() (remote.Config, []string) {
	vars := map[string]string{
		envAccountID: os.Getenv(envAccountID),
		envAccessKey: os.Getenv(envAccessKey),
		envSecretKey: os.Getenv(envSecretKey),
		envBucket:    os.Getenv(envBucket),
	}
	var missing []string
	for _, name := range []string{envAccountID, envAccessKey, envSecretKey, envBucket} {
		if vars[name] == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		return remote.Config{}, missing
	}
	return remote.Config{
		AccountID:       vars[envAccountID],
		Bucket:          vars[envBucket],
		AccessKeyID:     vars[envAccessKey],
		SecretAccessKey: vars[envSecretKey],
	}, nil
}

// uniquePrefix builds a bucket prefix scoped to this one run, so concurrent
// CI runs (or a run left over from a killed process) can never collide with
// or clean up after each other.
func uniquePrefix(name string) (string, error) {
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", err
	}
	return fmt.Sprintf("r2backup-realr2-test/%s-%d-%s", name, time.Now().UnixNano(), hex.EncodeToString(buf[:])), nil
}

// deleteAll removes every object under prefix, current mirror, trash, and
// anything else -- this suite's entire cleanup mechanism. It lists before
// it deletes rather than trying to predict every key this run might have
// written (a moved-to-trash object, a pruned one that failed to prune, a
// partially-completed multipart upload's parts are all covered by nothing
// but scanning the prefix), and it is safe to call on a prefix that turns
// out to hold nothing at all.
func deleteAll(ctx context.Context, client *remote.Client, prefix string) error {
	entries, err := client.List(ctx, prefix+"/")
	if err != nil {
		return fmt.Errorf("list %q for cleanup: %w", prefix, err)
	}
	if len(entries) == 0 {
		return nil
	}
	keys := make([]string, len(entries))
	for i, e := range entries {
		keys[i] = e.Key
	}
	return client.DeleteBatch(ctx, keys)
}

// backupRun and restoreInto mirror test/e2e's harness helpers exactly, so a
// reader who has seen one has seen both; the only difference is which
// client and prefix they are wired to underneath.

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
