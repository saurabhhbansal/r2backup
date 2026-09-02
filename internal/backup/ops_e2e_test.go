package backup_test

import (
	"context"
	"crypto/rand"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/saurabhhbansal/r2backup/internal/backup"
	"github.com/saurabhhbansal/r2backup/internal/index"
	"github.com/saurabhhbansal/r2backup/internal/remote"
	"github.com/saurabhhbansal/r2backup/internal/sets"
	tminio "github.com/saurabhhbansal/r2backup/test/minio"
)

// failNamedPutTransport fails a single-shot PUT -- a whole-object upload, not
// a multipart part (those carry ?uploadId=) and not a server-side copy (those
// carry X-Amz-Copy-Source) -- whose path contains needle, while armed. Modeled
// on move_e2e_test.go's failCopyTransport, aimed at a plain upload instead of
// a copy.
type failNamedPutTransport struct {
	next   http.RoundTripper
	needle string
	armed  int32
}

func (f *failNamedPutTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Method == http.MethodPut &&
		r.URL.Query().Get("uploadId") == "" &&
		r.Header.Get("X-Amz-Copy-Source") == "" &&
		strings.Contains(r.URL.Path, f.needle) &&
		atomic.LoadInt32(&f.armed) != 0 {
		return nil, errSimulatedPutFailure
	}
	return f.next.RoundTrip(r)
}

type simulatedPutFailure struct{}

func (simulatedPutFailure) Error() string { return "simulated PUT failure" }

var errSimulatedPutFailure = simulatedPutFailure{}

// TestAFailedUploadChargesFewerOperationsThanThePlanPredicted is the
// regression test for finding L6: rep.Operations used to come from
// p.Operations(...), which counts what the plan intended to send, not what
// the engine actually got onto the wire. A run where one upload fails must
// charge the free-tier monthly total for only the uploads that landed.
func TestAFailedUploadChargesFewerOperationsThanThePlanPredicted(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"ok-one.bin", "ok-two.bin", "bad.bin"} {
		content := make([]byte, 4096)
		if _, err := rand.Read(content); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, name), content, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	db, err := index.Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	breaker := &failNamedPutTransport{next: http.DefaultTransport, needle: "bad.bin", armed: 1}
	client, cleanup := tminio.StartWithConfig(t, func(c *remote.Config) {
		c.HTTPClient = &http.Client{Transport: breaker}
		c.MaxRetryAttempts = 1
	})
	t.Cleanup(cleanup)

	// RetentionDays left at zero -- trash off -- and Options.Trash left nil,
	// so this test isolates the upload/move half of the operation count from
	// the trash-copy half, which is unaffected by this bug (see TrashOps's
	// comment in internal/plan/plan.go for why).
	set := sets.Set{
		Name: "Docs", Prefix: "machines/test-pc/Docs",
		Root: root, Machine: "test-pc", RetentionDays: sets.RetentionDisabled,
	}
	opts := backup.Options{Set: set, Index: db, Client: client, DetectMoves: true}

	rep, err := backup.Run(context.Background(), opts)
	if err != nil {
		t.Fatalf("backup.Run: %v", err)
	}

	if rep.Uploaded != 2 {
		t.Fatalf("Uploaded = %d, want 2 -- two of the three files should have landed", rep.Uploaded)
	}
	if len(rep.Failures) != 1 {
		t.Fatalf("Failures = %d, want 1 -- bad.bin's PUT was broken on purpose", len(rep.Failures))
	}

	planned := rep.Planned.Operations(set.TrashEnabled())
	if planned != 3 {
		t.Fatalf("the plan predicted %d operations, want 3 (sanity check on the fixture)", planned)
	}

	// This is the bug: rep.Operations must reflect what actually landed (2
	// uploads), not what the plan predicted (3), or a failed upload is
	// billed against the free tier as if it had succeeded.
	if rep.Operations != 2 {
		t.Errorf("Operations = %d, want 2 -- charged for the plan's 3 predicted uploads instead of the 2 that actually landed", rep.Operations)
	}
	if rep.Operations >= planned {
		t.Errorf("Operations = %d did not charge fewer than the plan's %d -- the failed upload was still billed", rep.Operations, planned)
	}

	used, _, err := db.OpsThisMonth()
	if err != nil {
		t.Fatal(err)
	}
	if used != 2 {
		t.Errorf("the index's monthly total is %d, want 2 -- it must match what actually landed, not the plan's %d", used, planned)
	}
}

// TestAFullySuccessfulRunChargesTheSamePlanAndActualOperations is the
// no-regression companion: when nothing fails, the plan's prediction and the
// engine's actual counts agree, and rep.Operations must still equal that
// number after the fix, exactly as it did before it.
func TestAFullySuccessfulRunChargesTheSamePlanAndActualOperations(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"one.bin", "two.bin", "three.bin"} {
		content := make([]byte, 4096)
		if _, err := rand.Read(content); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, name), content, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	db, err := index.Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	client, cleanup := tminio.Start(t)
	t.Cleanup(cleanup)

	set := sets.Set{
		Name: "Docs", Prefix: "machines/test-pc/Docs",
		Root: root, Machine: "test-pc", RetentionDays: sets.RetentionDisabled,
	}
	opts := backup.Options{Set: set, Index: db, Client: client, DetectMoves: true}

	rep, err := backup.Run(context.Background(), opts)
	if err != nil {
		t.Fatalf("backup.Run: %v", err)
	}
	if len(rep.Failures) != 0 {
		t.Fatalf("unexpected failures: %v", rep.Failures)
	}
	if rep.Uploaded != 3 {
		t.Fatalf("Uploaded = %d, want 3", rep.Uploaded)
	}

	planned := rep.Planned.Operations(set.TrashEnabled())
	if rep.Operations != planned {
		t.Errorf("Operations = %d, want %d (the plan's prediction) -- a clean run must cost exactly what it always did", rep.Operations, planned)
	}
	if rep.Operations != 3 {
		t.Errorf("Operations = %d, want 3", rep.Operations)
	}

	used, _, err := db.OpsThisMonth()
	if err != nil {
		t.Fatal(err)
	}
	if used != 3 {
		t.Errorf("the index's monthly total is %d, want 3", used)
	}
}
