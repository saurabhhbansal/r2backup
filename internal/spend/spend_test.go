package spend

import (
	"math"
	"path/filepath"
	"testing"
	"time"

	"github.com/saurabhhbansal/r2backup/internal/cost"
	"github.com/saurabhhbansal/r2backup/internal/index"
)

func openDB(t *testing.T, now time.Time) *index.DB {
	t.Helper()
	db, err := index.Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	db.Now = func() time.Time { return now }
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func put(t *testing.T, db *index.DB, set, key string, size int64) {
	t.Helper()
	err := db.Put(set, index.Record{
		Key:  key,
		Size: size,
		Kind: index.KindFile,
	})
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
}

func TestReadOnEmptyIndexIsFreeAndOff(t *testing.T) {
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	db := openDB(t, now)

	s, err := Read(db, cost.Budget{}, now)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if s.StoredBytes != 0 || s.Objects != 0 {
		t.Errorf("stored = %d bytes / %d objects, want zero", s.StoredBytes, s.Objects)
	}
	if !s.Cost.WithinFreeTier {
		t.Error("an empty index is not within the free tier")
	}
	if s.EstimatedUSD() != 0 {
		t.Errorf("EstimatedUSD = %v, want 0", s.EstimatedUSD())
	}
	if s.Verdict != cost.Off {
		t.Errorf("Verdict = %v, want Off with no budget set", s.Verdict)
	}
}

func TestReadCarriesFreeTierLimits(t *testing.T) {
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	db := openDB(t, now)
	s, err := Read(db, cost.Budget{}, now)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if s.OpsLimit != cost.FreeClassAOps {
		t.Errorf("OpsLimit = %d, want %d", s.OpsLimit, cost.FreeClassAOps)
	}
	if s.ClassBLimit != cost.FreeClassBOps {
		t.Errorf("ClassBLimit = %d, want %d", s.ClassBLimit, cost.FreeClassBOps)
	}
}

func TestReadCountsBothOperationClasses(t *testing.T) {
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	db := openDB(t, now)
	if err := db.AddOps(1200); err != nil {
		t.Fatalf("AddOps: %v", err)
	}
	if err := db.AddClassBOps(7); err != nil {
		t.Fatalf("AddClassBOps: %v", err)
	}

	s, err := Read(db, cost.Budget{}, now)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if s.ClassAOps != 1200 {
		t.Errorf("ClassAOps = %d, want 1200", s.ClassAOps)
	}
	if s.ClassBOps != 7 {
		t.Errorf("ClassBOps = %d, want 7", s.ClassBOps)
	}
	if s.Usage.ClassAOps != 1200 || s.Usage.ClassBOps != 7 {
		t.Errorf("Usage ops = %d/%d, want 1200/7", s.Usage.ClassAOps, s.Usage.ClassBOps)
	}
}

// With no samples yet, storage must be charged for today alone. Assuming the
// current size held all month would overstate a fresh install's very first
// reading by up to thirtyfold -- and that figure is what a limit reads.
func TestReadWithoutSamplesChargesTodayOnly(t *testing.T) {
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC) // half a day in
	db := openDB(t, now)
	put(t, db, "docs", "big", 100_000_000_000) // 100 GB

	s, err := Read(db, cost.Budget{}, now)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	// 100 GB for half a day out of a 30-day month.
	want := 100 * 0.5 / 30
	if math.Abs(s.Usage.StorageGBMonths-want) > 0.01 {
		t.Errorf("StorageGBMonths = %.3f, want about %.3f", s.Usage.StorageGBMonths, want)
	}
	if s.Usage.StorageGBMonths > 10 {
		t.Errorf("charged %.2f GB-months on day one; naive pricing would say 100",
			s.Usage.StorageGBMonths)
	}
}

// Once samples exist they are what counts, so storage is priced over the time
// it was actually held.
func TestReadUsesSamplesWhenPresent(t *testing.T) {
	start := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	// The 30th, not the 31st: September has 30 days, and a Date past a
	// month's end normalizes into the next one -- which would put this read
	// in October, where September's samples correctly do not exist.
	now := time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC)
	db := openDB(t, start)

	// 30 GB held for the first 15 days...
	db.Now = func() time.Time { return start }
	if err := db.RecordStorageSample(30_000_000_000); err != nil {
		t.Fatalf("RecordStorageSample: %v", err)
	}
	// ...then 90 GB for the next 14, up to `now`.
	// 30*(15/30) + 90*(14/30) = 15 + 42 = 57 GB-months.
	mid := start.Add(15 * 24 * time.Hour)
	db.Now = func() time.Time { return mid }
	if err := db.RecordStorageSample(90_000_000_000); err != nil {
		t.Fatalf("RecordStorageSample: %v", err)
	}
	db.Now = func() time.Time { return now }

	s, err := Read(db, cost.Budget{}, now)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if math.Abs(s.Usage.StorageGBMonths-57) > 0.5 {
		t.Errorf("StorageGBMonths = %.2f, want about 57", s.Usage.StorageGBMonths)
	}
}

func TestReadAppliesBudgetVerdict(t *testing.T) {
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	db := openDB(t, now)
	// 5M Class A ops is 4M over the free tier, at $4.50/M = $18.
	if err := db.AddOps(5_000_000); err != nil {
		t.Fatalf("AddOps: %v", err)
	}

	s, err := Read(db, cost.Budget{LimitUSD: 10}, now)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if math.Abs(s.EstimatedUSD()-18) > 0.01 {
		t.Fatalf("EstimatedUSD = %.2f, want 18.00", s.EstimatedUSD())
	}
	if s.Verdict != cost.Paused {
		t.Errorf("Verdict = %v, want Paused", s.Verdict)
	}
	if s.Budget.AllowsBackup(s.EstimatedUSD(), now) {
		t.Error("backups still allowed past the limit")
	}
	// And never a restore.
	if !s.Budget.AllowsRestore() {
		t.Error("an exceeded budget blocked a restore")
	}
}

// The limit is enforced against what has been spent, not against the
// projection -- otherwise a quiet month would be stopped early over money
// that has not been spent and might never be.
func TestProjectionIsSeparateFromTheEnforcedFigure(t *testing.T) {
	now := time.Date(2026, 9, 16, 0, 0, 0, 0, time.UTC) // half the month
	db := openDB(t, now)
	if err := db.AddOps(3_000_000); err != nil { // 2M over = $9.00
		t.Fatalf("AddOps: %v", err)
	}

	s, err := Read(db, cost.Budget{LimitUSD: 12}, now)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if math.Abs(s.EstimatedUSD()-9) > 0.01 {
		t.Fatalf("EstimatedUSD = %.2f, want 9.00", s.EstimatedUSD())
	}
	// Projected is about $18 -- over the $12 limit -- but spending so far
	// is under it, so backups continue.
	if s.Projected <= s.EstimatedUSD() {
		t.Errorf("Projected = %.2f, want more than the %.2f spent so far",
			s.Projected, s.EstimatedUSD())
	}
	if s.Verdict == cost.Paused {
		t.Error("the projection stopped backups; only spending so far may do that")
	}
	if !s.Budget.AllowsBackup(s.EstimatedUSD(), now) {
		t.Error("backups blocked while still under the limit")
	}
}

func TestReadWithoutIndexErrors(t *testing.T) {
	if _, err := Read(nil, cost.Budget{}, time.Now()); err == nil {
		t.Error("want an error with no index")
	}
}
