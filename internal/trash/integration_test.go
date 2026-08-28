package trash_test

import (
	"bytes"
	"context"
	"io"
	"testing"
	"time"

	"github.com/saurabhhbansal/r2backup/internal/remote"
	"github.com/saurabhhbansal/r2backup/internal/trash"
	minio "github.com/saurabhhbansal/r2backup/test/minio"
)

// newClient starts a real MinIO server for the duration of the test,
// matching the pattern internal/remote's own integration tests use, so
// trash is exercised against actual S3 wire behavior: a real
// CopyObject, real pagination, real DeleteObjects -- not a hand-rolled
// mock's idea of how those behave. minio.Start skips the test cleanly
// when no MinIO binary can be obtained or started.
func newClient(t *testing.T) *remote.Client {
	t.Helper()
	client, cleanup := minio.Start(t)
	t.Cleanup(cleanup)
	return client
}

func putObject(t *testing.T, c *remote.Client, key string, content []byte) {
	t.Helper()
	err := c.Put(context.Background(), remote.PutInput{
		Key:      key,
		Body:     bytes.NewReader(content),
		Size:     int64(len(content)),
		Metadata: remote.Metadata{ModTime: time.Now(), Mode: 0o644},
	})
	if err != nil {
		t.Fatalf("seed %q: %v", key, err)
	}
}

func TestIntegrationMoveThenRestoreIsByteIdentical(t *testing.T) {
	c := newClient(t)
	ctx := context.Background()

	content := []byte("the quick brown fox jumps over the lazy dog, repeatedly, for good measure")
	putObject(t, c, "myset/current/docs/report.pdf", content)

	now := time.Date(2026, 8, 28, 14, 30, 0, 0, time.UTC)
	tr := trash.New(c, trash.Clock{Now: func() time.Time { return now }})

	moveRes, err := tr.Move(ctx, "myset", []string{"docs/report.pdf"}, 30)
	if err != nil {
		t.Fatalf("Move: %v", err)
	}
	if len(moveRes.Moved) != 1 {
		t.Fatalf("Moved = %v, want 1 entry", moveRes.Moved)
	}
	trashKey := moveRes.Moved[0].TrashKey

	// The trashed copy must be a real, independently readable object --
	// prove that by reading it back before touching the original at all.
	trashedObj, err := c.Get(ctx, trashKey)
	if err != nil {
		t.Fatalf("Get trashed object %q: %v", trashKey, err)
	}
	trashedBytes, err := io.ReadAll(trashedObj.Body)
	trashedObj.Body.Close()
	if err != nil {
		t.Fatalf("read trashed object: %v", err)
	}
	if !bytes.Equal(trashedBytes, content) {
		t.Fatalf("trashed object content mismatch: got %d bytes, want %d bytes", len(trashedBytes), len(content))
	}

	// Simulate the caller doing what it would after a real trash-then-
	// overwrite: the original at the live key changes.
	if err := c.Delete(ctx, "myset/current/docs/report.pdf"); err != nil {
		t.Fatalf("delete live key: %v", err)
	}

	if err := tr.Restore(ctx, "myset", trash.Entry{RelPath: "docs/report.pdf", TrashKey: trashKey}); err != nil {
		t.Fatalf("Restore: %v", err)
	}

	restored, err := c.Get(ctx, "myset/current/docs/report.pdf")
	if err != nil {
		t.Fatalf("Get restored object: %v", err)
	}
	defer restored.Body.Close()
	restoredBytes, err := io.ReadAll(restored.Body)
	if err != nil {
		t.Fatalf("read restored object: %v", err)
	}
	if !bytes.Equal(restoredBytes, content) {
		t.Fatalf("restored object is not byte-identical: got %d bytes, want %d bytes", len(restoredBytes), len(content))
	}
}

func TestIntegrationPruneRemovesOnlyIntendedPrefixes(t *testing.T) {
	c := newClient(t)
	ctx := context.Background()

	// Three trashed objects laid down directly at the keys trash.Move
	// would have produced, one per date, so Prune's date filtering can be
	// tested without waiting on a real clock to age anything.
	oldKey := "myset/trash/2020-01-01/old-file~090000-aaaaaaaaaaaa.txt"
	boundaryKey := "myset/trash/2026-07-29/boundary-file~090000-bbbbbbbbbbbb.txt"
	recentKey := "myset/trash/2026-08-28/recent-file~090000-cccccccccccc.txt"
	// A live object, to prove Prune never reaches into current/ even
	// when it shares the same bucket and set prefix.
	liveKey := "myset/current/still-here.txt"

	putObject(t, c, oldKey, []byte("old"))
	putObject(t, c, boundaryKey, []byte("boundary"))
	putObject(t, c, recentKey, []byte("recent"))
	putObject(t, c, liveKey, []byte("live"))

	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	tr := trash.New(c, trash.Clock{Now: func() time.Time { return now }})

	res, err := tr.Prune(ctx, "myset", 30)
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if len(res.DatesPruned) != 1 || res.DatesPruned[0] != "2020-01-01" {
		t.Fatalf("DatesPruned = %v, want [2020-01-01]", res.DatesPruned)
	}

	if _, err := c.Head(ctx, oldKey); err == nil {
		t.Error("old key still exists after Prune")
	}
	if _, err := c.Head(ctx, boundaryKey); err != nil {
		t.Errorf("boundary key (exactly 30 days old) was pruned: %v", err)
	}
	if _, err := c.Head(ctx, recentKey); err != nil {
		t.Errorf("recent key was pruned: %v", err)
	}
	if _, err := c.Head(ctx, liveKey); err != nil {
		t.Errorf("Prune touched the live key: %v", err)
	}
}
