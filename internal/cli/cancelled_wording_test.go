package cli

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/saurabhhbansal/r2backup/internal/backup"
	"github.com/saurabhhbansal/r2backup/internal/runstate"
	"github.com/saurabhhbansal/r2backup/internal/sets"
)

// TestStatusReadsACancelledRunAsCancelledNotFailed is the wording follow-up
// to the L1 finding: a run someone stopped on purpose used to come back from
// `status` as
//
//	last run 2 minutes ago — failed: run cancelled: context canceled
//
// which called a deliberate stop a failure and printed Go's raw
// "context canceled" text at a person. runOne and recordRun now set
// runstate.Past.Cancelled by testing the error backup.Run returns with
// errors.Is(err, backup.ErrCancelled) -- this test skips straight to that
// recorded state (rather than re-driving an actual cancelled backup, which
// cancel_test.go and TestACancelledRunReportsAsCancelledNotClean already
// cover) to check what printStatus does with it.
func TestStatusReadsACancelledRunAsCancelledNotFailed(t *testing.T) {
	t.Setenv("R2BACKUP_DATA_DIR", t.TempDir())
	a, err := openApp()
	if err != nil {
		t.Fatal(err)
	}
	defer a.close()
	if err := a.sets.Add(sets.Set{Name: "Documents", Root: t.TempDir(), Machine: "testpc"}); err != nil {
		t.Fatal(err)
	}

	histPath, err := historyPath()
	if err != nil {
		t.Fatal(err)
	}
	// backup.ErrCancelled.Error() is exactly what runOne and recordRun store
	// in Error for a run stopped mid-flight -- see backup.go's ctx.Err()
	// branch, which returns ErrCancelled itself rather than a wrap that
	// would carry the engine's raw "context canceled" text along with it.
	if err := runstate.Record(histPath, runstate.Past{
		Set: "Documents", FinishedAt: time.Now().Add(-2 * time.Minute),
		Error: backup.ErrCancelled.Error(), Cancelled: true,
	}); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := printStatus(a, &Options{Out: &out}); err != nil {
		t.Fatalf("printStatus: %v", err)
	}
	got := out.String()

	if strings.Contains(got, "context canceled") {
		t.Errorf("status leaked Go's raw error text, got:\n%s", got)
	}
	if strings.Contains(got, "failed") {
		t.Errorf("status called a deliberate stop a failure, got:\n%s", got)
	}
	if !strings.Contains(got, "cancelled") {
		t.Errorf("status should say the run was cancelled, got:\n%s", got)
	}
}

// TestStatusStillReadsAGenuineFailureAsFailed guards the other side of the
// same change: Cancelled is a field on runstate.Past, not something inferred
// from whether Error is merely non-empty, so a run that broke for a real
// reason -- Cancelled left false -- must still read as "failed" with its
// actual error message, exactly as before.
func TestStatusStillReadsAGenuineFailureAsFailed(t *testing.T) {
	t.Setenv("R2BACKUP_DATA_DIR", t.TempDir())
	a, err := openApp()
	if err != nil {
		t.Fatal(err)
	}
	defer a.close()
	if err := a.sets.Add(sets.Set{Name: "Documents", Root: t.TempDir(), Machine: "testpc"}); err != nil {
		t.Fatal(err)
	}

	histPath, err := historyPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := runstate.Record(histPath, runstate.Past{
		Set: "Documents", FinishedAt: time.Now().Add(-2 * time.Minute),
		Error: errors.New("put object: 403 Forbidden").Error(), Cancelled: false,
	}); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := printStatus(a, &Options{Out: &out}); err != nil {
		t.Fatalf("printStatus: %v", err)
	}
	got := out.String()

	if !strings.Contains(got, "failed: put object: 403 Forbidden") {
		t.Errorf("a genuine failure should still read as failed with its error, got:\n%s", got)
	}
	if strings.Contains(got, "— cancelled") {
		t.Errorf("a genuine failure was read as cancelled, got:\n%s", got)
	}
}
