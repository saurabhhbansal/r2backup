package backup_test

import (
	"context"
	"crypto/rand"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/saurabhhbansal/r2backup/internal/backup"
	"github.com/saurabhhbansal/r2backup/internal/index"
	"github.com/saurabhhbansal/r2backup/internal/remote"
	"github.com/saurabhhbansal/r2backup/internal/sets"
	tminio "github.com/saurabhhbansal/r2backup/test/minio"
)

// A failed server-side move used to be recorded as done anyway: backup.go
// rewrote the index for every plan.Move regardless of whether CopyObject
// actually succeeded, so the next run said "unchanged" for a file the
// bucket never received, and the file was gone from the backup for good.
// This mirrors resume_e2e_test.go's approach -- a real MinIO server with a
// hooked transport that breaks one specific kind of request on demand --
// aimed at CopyObject instead of a multipart PUT.

// failCopyTransport fails every CopyObject request while armed (armed != 0)
// and passes every request through untouched once disarmed. It is shared
// across runs against the same server so a test can fail the copy on one
// run and let it succeed on the next.
type failCopyTransport struct {
	next  http.RoundTripper
	armed int32
}

func (f *failCopyTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	// CopyObject is a PUT with no body of its own -- the bytes travel
	// server-side -- identified by the X-Amz-Copy-Source header rather than
	// by any body hook, which is why this needs its own transport instead of
	// hook_transport_test.go's putHookTransport.
	if r.Method == http.MethodPut && r.Header.Get("X-Amz-Copy-Source") != "" && atomic.LoadInt32(&f.armed) != 0 {
		return nil, errSimulatedCopyFailure
	}
	return f.next.RoundTrip(r)
}

type simulatedCopyFailure struct{}

func (simulatedCopyFailure) Error() string { return "simulated CopyObject failure" }

var errSimulatedCopyFailure = simulatedCopyFailure{}

// TestAFailedMoveLeavesTheIndexAtTheOldKey is the regression test for that
// bug: it fails on the unfixed backup.go (the old loop rewrote the index for
// every m := range p.Moves, whether or not the copy behind it worked) and
// passes once the record loop is driven by the engine's OnMoved callback,
// which only fires for copies that actually landed.
func TestAFailedMoveLeavesTheIndexAtTheOldKey(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	content := make([]byte, 8192) // above plan.MinMoveSize, or the rename is a plain re-upload
	if _, err := rand.Read(content); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "dir", "big.bin"), content, 0o644); err != nil {
		t.Fatal(err)
	}

	db, err := index.Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	breaker := &failCopyTransport{next: http.DefaultTransport}
	client, cleanup := tminio.StartWithConfig(t, func(c *remote.Config) {
		c.HTTPClient = &http.Client{Transport: breaker}
		// One attempt: a test that means to fail a request does not want to
		// sit through the standard retryer's six attempts and backoff first.
		c.MaxRetryAttempts = 1
		c.Resume = backup.ResumeStoreFor(db)
	})
	t.Cleanup(cleanup)

	set := sets.Set{
		Name:          "Docs",
		Prefix:        "machines/test-pc/Docs",
		Root:          root,
		Machine:       "test-pc",
		RetentionDays: sets.DefaultRetentionDays,
	}
	opts := backup.Options{Set: set, Index: db, Client: client, DetectMoves: true}

	// First run: the file goes up at its original key.
	first, err := backup.Run(context.Background(), opts)
	if err != nil {
		t.Fatalf("first run: %v", err)
	}
	if first.Uploaded != 1 {
		t.Fatalf("first run uploaded %d file(s), want 1", first.Uploaded)
	}

	// Rename dir/big.bin -> dir2/big.bin, then arm the breaker so the
	// server-side copy this rename should produce fails.
	if err := os.Rename(filepath.Join(root, "dir"), filepath.Join(root, "dir2")); err != nil {
		t.Fatal(err)
	}
	atomic.StoreInt32(&breaker.armed, 1)

	second, err := backup.Run(context.Background(), opts)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if len(second.Failures) == 0 {
		t.Fatal("a broken CopyObject should have been reported as a failure")
	}
	if second.Moved != 0 {
		t.Errorf("Moved = %d, want 0 -- the copy failed", second.Moved)
	}

	// This is the bug: the index must still point at the old key, since the
	// copy never landed, and must not have recorded the new key at all.
	if _, err := db.Get(set.Name, "dir/big.bin"); err != nil {
		t.Errorf("the old key was forgotten even though its copy never landed: %v", err)
	}
	if _, err := db.Get(set.Name, "dir2/big.bin"); !errors.Is(err, index.ErrNotFound) {
		t.Error("the new key was recorded in the index even though CopyObject failed -- the object was never written to it")
	}

	// Now let copies succeed and run again. The move that failed above must
	// still be outstanding -- the whole point of leaving the old key alone
	// -- and must actually complete this time.
	atomic.StoreInt32(&breaker.armed, 0)
	third, err := backup.Run(context.Background(), opts)
	if err != nil {
		t.Fatalf("third run: %v", err)
	}
	if third.Moved != 1 {
		t.Errorf("Moved = %d, want 1 -- the retried rename should now succeed", third.Moved)
	}
	if len(third.Failures) != 0 {
		t.Errorf("third run reported failures: %v", third.Failures)
	}

	obj, err := client.Get(context.Background(), "machines/test-pc/Docs/current/dir2/big.bin")
	if err != nil {
		t.Fatalf("the file never actually arrived at its new key in the bucket: %v", err)
	}
	obj.Body.Close()

	if _, err := db.Get(set.Name, "dir2/big.bin"); err != nil {
		t.Errorf("the successful move was not recorded at the new key: %v", err)
	}
	if _, err := db.Get(set.Name, "dir/big.bin"); !errors.Is(err, index.ErrNotFound) {
		t.Error("the old key is still recorded in the index after a successful move")
	}
}
