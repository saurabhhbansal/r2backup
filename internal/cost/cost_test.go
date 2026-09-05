package cost

import (
	"math"
	"testing"
	"time"
)

func closeTo(t *testing.T, got, want float64, what string) {
	t.Helper()
	// A cent of tolerance: these are estimates built from fractional
	// GB-months, and asserting tighter than the arithmetic's own precision
	// would make the tests fail for reasons that are not bugs.
	if math.Abs(got-want) > 0.005 {
		t.Errorf("%s = %.4f, want %.4f", what, got, want)
	}
}

// The free tier is the common case for this product -- most people backing up
// a documents folder never leave it -- so it is the first thing that has to
// be right.
func TestPriceWithinFreeTierCostsNothing(t *testing.T) {
	b := Price(Usage{StorageGBMonths: 9.5, ClassAOps: 900_000, ClassBOps: 5_000_000})
	if !b.WithinFreeTier {
		t.Error("WithinFreeTier = false, want true")
	}
	closeTo(t, b.TotalUSD, 0, "TotalUSD")
	if b.BillableGBMonths != 0 || b.BillableClassA != 0 || b.BillableClassB != 0 {
		t.Errorf("billable amounts = %v/%d/%d, want all zero",
			b.BillableGBMonths, b.BillableClassA, b.BillableClassB)
	}
}

// Exactly at the allowance is still free; only what is over it is charged.
func TestPriceAtExactAllowanceIsFree(t *testing.T) {
	b := Price(Usage{
		StorageGBMonths: FreeStorageGBMonths,
		ClassAOps:       FreeClassAOps,
		ClassBOps:       FreeClassBOps,
	})
	closeTo(t, b.TotalUSD, 0, "TotalUSD")
	if !b.WithinFreeTier {
		t.Error("WithinFreeTier = false at exactly the allowance")
	}
}

func TestPriceChargesOnlyTheExcess(t *testing.T) {
	// 110 GB-months is 100 over the allowance, at $0.015 = $1.50.
	// 3M Class A is 2M over, at $4.50/M = $9.00.
	// 12M Class B is 2M over, at $0.36/M = $0.72.
	b := Price(Usage{StorageGBMonths: 110, ClassAOps: 3_000_000, ClassBOps: 12_000_000})
	closeTo(t, b.StorageUSD, 1.50, "StorageUSD")
	closeTo(t, b.ClassAUSD, 9.00, "ClassAUSD")
	closeTo(t, b.ClassBUSD, 0.72, "ClassBUSD")
	closeTo(t, b.TotalUSD, 11.22, "TotalUSD")
	if b.WithinFreeTier {
		t.Error("WithinFreeTier = true despite a bill")
	}
}

// R2 prices in decimal GB. Using gibibytes would under-report by ~7%, which
// is exactly the kind of quiet optimism an estimate must not have.
func TestGBMonthsUsesDecimalGigabytes(t *testing.T) {
	got := GBMonths(1_000_000_000, 30*24*time.Hour)
	closeTo(t, got, 1.0, "one decimal GB for one month")
}

func TestGBMonthsScalesWithTimeHeld(t *testing.T) {
	full := GBMonths(100_000_000_000, 30*24*time.Hour)
	half := GBMonths(100_000_000_000, 15*24*time.Hour)
	closeTo(t, full, 100, "100 GB for a full month")
	closeTo(t, half, 50, "100 GB for half a month")
}

func TestGBMonthsZeroForEmptyOrNoTime(t *testing.T) {
	if got := GBMonths(0, 30*24*time.Hour); got != 0 {
		t.Errorf("no bytes = %v, want 0", got)
	}
	if got := GBMonths(1_000_000_000, 0); got != 0 {
		t.Errorf("no time = %v, want 0", got)
	}
	if got := GBMonths(-5, time.Hour); got != 0 {
		t.Errorf("negative bytes = %v, want 0", got)
	}
}

// The reason AccrueGBMonths exists: data added late in the month must not be
// priced as though it had been there since the first.
func TestAccrueChargesLateDataOnlyForTimeHeld(t *testing.T) {
	start := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	now := start.Add(30 * 24 * time.Hour)

	// Empty until the 28th, then 100 GB for the last two days.
	samples := []Sample{
		{At: start, Bytes: 0},
		{At: start.Add(28 * 24 * time.Hour), Bytes: 100_000_000_000},
	}
	got := AccrueGBMonths(samples, now)

	// 100 GB for 2 of 30 days = 6.67 GB-months, not 100.
	closeTo(t, got, 100*2.0/30.0, "late-added data")
	if got > 10 {
		t.Errorf("accrued %.2f GB-months; naive current-size pricing would say 100", got)
	}
}

func TestAccrueHoldsEachSampleUntilTheNext(t *testing.T) {
	start := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	now := start.Add(30 * 24 * time.Hour)
	// 30 GB for the first half, 90 GB for the second: mean 60 GB-months.
	samples := []Sample{
		{At: start, Bytes: 30_000_000_000},
		{At: start.Add(15 * 24 * time.Hour), Bytes: 90_000_000_000},
	}
	closeTo(t, AccrueGBMonths(samples, now), 60, "two-step accrual")
}

func TestAccrueSortsAndIgnoresTheFuture(t *testing.T) {
	start := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	now := start.Add(30 * 24 * time.Hour)
	unsorted := []Sample{
		{At: start.Add(15 * 24 * time.Hour), Bytes: 90_000_000_000},
		{At: now.Add(24 * time.Hour), Bytes: 999_000_000_000}, // after upTo
		{At: start, Bytes: 30_000_000_000},
	}
	closeTo(t, AccrueGBMonths(unsorted, now), 60, "unsorted with a future sample")
}

func TestAccrueEmptyIsZero(t *testing.T) {
	if got := AccrueGBMonths(nil, time.Now()); got != 0 {
		t.Errorf("no samples = %v, want 0", got)
	}
}

func TestMonthElapsed(t *testing.T) {
	// September has 30 days; the 16th at midnight is 15 days in.
	mid := time.Date(2026, 9, 16, 0, 0, 0, 0, time.UTC)
	closeTo(t, MonthElapsed(mid), 0.5, "half of September")

	first := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	if got := MonthElapsed(first); got != 0 {
		t.Errorf("start of month = %v, want 0", got)
	}
	// February 2026 has 28 days, so the 15th is halfway. This is the case
	// that would break if month length were hardcoded here.
	feb := time.Date(2026, 2, 15, 0, 0, 0, 0, time.UTC)
	closeTo(t, MonthElapsed(feb), 14.0/28.0, "mid-February")
}

func TestProjectExtrapolatesToMonthEnd(t *testing.T) {
	mid := time.Date(2026, 9, 16, 0, 0, 0, 0, time.UTC)
	// $5 spent in half a month projects to $10.
	closeTo(t, Project(5, mid), 10, "projection at half a month")
}

func TestProjectAtMonthStartDoesNotExplode(t *testing.T) {
	first := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	got := Project(5, first)
	if math.IsInf(got, 0) || math.IsNaN(got) {
		t.Fatalf("projection at month start = %v", got)
	}
	closeTo(t, got, 5, "projection with no elapsed time")
}
