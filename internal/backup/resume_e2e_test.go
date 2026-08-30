package backup_test

import (
	"context"
	"crypto/rand"
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

// Resuming, through the whole stack rather than through remote.Put.
//
// internal/remote proves the protocol against a real server. What this proves
// is the wiring: that a backup actually reaches that protocol, that the store
// it is given is the index, and that a second run of the same set finishes
// the file the first one was cut off partway through. Every one of those is a
// join between packages, which is where a feature quietly turns out not to be
// connected to anything.

type cutPartsAfter struct {
	next  http.RoundTripper
	allow int32
}

func (c *cutPartsAfter) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Method == http.MethodPut && r.URL.Query().Get("uploadId") != "" {
		if atomic.AddInt32(&c.allow, -1) < 0 {
			return nil, errCutPart
		}
	}
	return c.next.RoundTrip(r)
}

type cutErr struct{}

func (cutErr) Error() string { return "connection cut" }

var errCutPart = cutErr{}

const (
	resumePartSize  = 5 << 20
	resumeThreshold = 6 << 20
	resumeFileSize  = 12 << 20 // three parts
)

func TestABackupFinishesAFileTheLastRunWasCutOffPartway(t *testing.T) {
	root := t.TempDir()
	content := make([]byte, resumeFileSize)
	if _, err := rand.Read(content); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "video.bin"), content, 0o644); err != nil {
		t.Fatal(err)
	}

	db, err := index.Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	// One transport both runs share, so the second run talks to the same
	// server -- and therefore to the same half-finished upload -- as the
	// first. allow is what decides which run is the interrupted one.
	cut := &cutPartsAfter{next: http.DefaultTransport, allow: 1}
	client, cleanup := tminio.StartWithConfig(t, func(c *remote.Config) {
		c.HTTPClient = &http.Client{Transport: cut}
		c.MultipartThreshold = resumeThreshold
		c.PartSize = resumePartSize
		c.MaxRetryAttempts = 1
		// The join under test: the index, handed to the client as the place
		// unfinished uploads are written down.
		c.Resume = backup.ResumeStoreFor(db)
	})
	t.Cleanup(cleanup)

	set := sets.Set{
		Name: "Videos", Prefix: "machines/test-pc/Videos",
		Root: root, Machine: "test-pc", RetentionDays: sets.DefaultRetentionDays,
	}
	opts := backup.Options{Set: set, Index: db, Client: client}

	// First run: one part lands, then the connection dies.
	rep, err := backup.Run(context.Background(), opts)
	if err == nil && len(rep.Failures) == 0 {
		t.Fatal("the cut connection should have stopped the upload")
	}

	// The interrupted upload was written down, in the index, by the backup.
	pending, ok, err := db.PendingUploadFor("machines/test-pc/Videos/current/video.bin")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("a backup cut off mid-upload recorded nothing to resume from")
	}
	if len(pending.Parts) == 0 {
		t.Fatal("the part that landed was not recorded")
	}
	done, total, files, err := db.PendingBytes()
	if err != nil {
		t.Fatal(err)
	}
	if files != 1 || done == 0 || total != resumeFileSize {
		t.Fatalf("PendingBytes = %d of %d over %d files, want one file part-done", done, total, files)
	}
	// Nothing was recorded as uploaded, because nothing finished.
	if n, _ := db.Count(set.Name); n != 0 {
		t.Errorf("the index claims %d objects for a file that never finished", n)
	}

	// Second run, connection healthy. This is what the scheduler, or the
	// logon trigger, does on its own.
	atomic.StoreInt32(&cut.allow, 1<<30)
	rep, err = backup.Run(context.Background(), opts)
	if err != nil {
		t.Fatalf("the resuming run failed: %v", err)
	}
	if len(rep.Failures) != 0 {
		t.Fatalf("the resuming run reported failures: %v", rep.Failures)
	}
	if rep.Uploaded != 1 {
		t.Errorf("Uploaded = %d, want the 1 file that was outstanding", rep.Uploaded)
	}

	// The record is gone, and the object is whole.
	if _, ok, _ := db.PendingUploadFor("machines/test-pc/Videos/current/video.bin"); ok {
		t.Error("a finished upload left its resume record behind")
	}
	obj, err := client.Get(context.Background(), "machines/test-pc/Videos/current/video.bin")
	if err != nil {
		t.Fatal(err)
	}
	defer obj.Body.Close()
	if obj.Size != resumeFileSize {
		t.Fatalf("the stored object is %d bytes, want %d", obj.Size, resumeFileSize)
	}
	// And a third run has nothing left to do, which is what proves the
	// index was told the file is up rather than merely the bucket having it.
	rep, err = backup.Run(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Uploaded != 0 {
		t.Errorf("a run after a completed resume uploaded %d file(s) again", rep.Uploaded)
	}
}
