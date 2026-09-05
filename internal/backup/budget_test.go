package backup_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/saurabhhbansal/r2backup/internal/backup"
	"github.com/saurabhhbansal/r2backup/internal/cost"
	"github.com/saurabhhbansal/r2backup/test/fixtures"
)

// overspend loads the op counter past a small budget. 2M Class A operations
// is 1M over the free tier, which at $4.50/million is $4.50 -- comfortably
// past the $1 limits used below.
func overspend(t *testing.T, h *harness) {
	t.Helper()
	if err := h.db.AddOps(2_000_000); err != nil {
		t.Fatalf("AddOps: %v", err)
	}
}

func runWithBudget(h *harness, b cost.Budget) (*backup.Report, error) {
	return backup.Run(context.Background(), backup.Options{
		Set:         h.set,
		Index:       h.db,
		Client:      h.client,
		DetectMoves: true,
		Budget:      b,
	})
}

func TestBudgetStopsUploadsWhenReached(t *testing.T) {
	h := setup(t, fixtures.Spec{SmallFiles: 4, SmallFileSize: 128, Seed: 3})
	overspend(t, h)

	rep, err := runWithBudget(h, cost.Budget{LimitUSD: 1})
	if !errors.Is(err, backup.ErrBudgetPaused) {
		t.Fatalf("err = %v, want ErrBudgetPaused", err)
	}
	if rep == nil {
		t.Fatal("no report returned; the dashboard needs one to explain the stop")
	}
	if rep.Uploaded != 0 {
		t.Errorf("uploaded %d objects despite the limit", rep.Uploaded)
	}
	// And nothing reached the bucket.
	if live := h.liveKeys(t); len(live) != 0 {
		t.Errorf("bucket holds %d objects, want none: %v", len(live), live)
	}
}

// The error has to name the figures, because "the limit was reached" without
// them leaves someone with no idea whether to raise it by a dollar or by ten.
func TestBudgetErrorNamesTheAmounts(t *testing.T) {
	h := setup(t, fixtures.Spec{SmallFiles: 2, SmallFileSize: 128, Seed: 7})
	overspend(t, h)

	_, err := runWithBudget(h, cost.Budget{LimitUSD: 1})
	if err == nil {
		t.Fatal("want an error")
	}
	msg := err.Error()
	for _, want := range []string{"$4.50", "$1.00"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q does not mention %s", msg, want)
		}
	}
}

func TestBudgetAllowsRunWhenUnderLimit(t *testing.T) {
	h := setup(t, fixtures.Spec{SmallFiles: 2, SmallFileSize: 128, Seed: 7})
	overspend(t, h) // about $4.50 spent

	rep, err := runWithBudget(h, cost.Budget{LimitUSD: 100})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.Uploaded == 0 {
		t.Error("nothing uploaded while well under the limit")
	}
}

// The zero Budget is off, and must not stop anything however much has been
// spent. This is the default every install runs with.
func TestNoBudgetNeverStops(t *testing.T) {
	h := setup(t, fixtures.Spec{SmallFiles: 2, SmallFileSize: 128, Seed: 7})
	overspend(t, h)

	rep, err := runWithBudget(h, cost.Budget{})
	if err != nil {
		t.Fatalf("Run with no budget: %v", err)
	}
	if rep.Uploaded == 0 {
		t.Error("nothing uploaded with no limit set")
	}
}

// A run with nothing to do costs nothing, so an exceeded limit has no
// business stopping it -- and it must not be reported as a budget failure.
func TestBudgetDoesNotStopAnUnchangedRun(t *testing.T) {
	h := setup(t, fixtures.Spec{SmallFiles: 2, SmallFileSize: 128, Seed: 7})

	if _, err := runWithBudget(h, cost.Budget{}); err != nil {
		t.Fatalf("first run: %v", err)
	}
	overspend(t, h)

	rep, err := runWithBudget(h, cost.Budget{LimitUSD: 1})
	if err != nil {
		t.Fatalf("unchanged run under an exceeded budget: %v", err)
	}
	if rep.Uploaded != 0 {
		t.Errorf("uploaded %d on an unchanged run", rep.Uploaded)
	}
}

// A finished run leaves a storage reading behind, which is what makes the
// month's estimate an average over time rather than a snapshot.
func TestRunRecordsAStorageSample(t *testing.T) {
	h := setup(t, fixtures.Spec{SmallFiles: 2, SmallFileSize: 256, Seed: 9})

	if before, err := h.db.StorageSamples(); err != nil {
		t.Fatalf("StorageSamples: %v", err)
	} else if len(before) != 0 {
		t.Fatalf("samples before any run = %v, want none", before)
	}

	h.run(t)

	samples, err := h.db.StorageSamples()
	if err != nil {
		t.Fatalf("StorageSamples: %v", err)
	}
	if len(samples) != 1 {
		t.Fatalf("len(samples) = %d, want 1 after a run", len(samples))
	}
	if samples[0].Bytes <= 0 {
		t.Errorf("sample bytes = %d, want the uploaded size", samples[0].Bytes)
	}
}
