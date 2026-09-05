package index

import (
	"testing"
	"time"
)

// atClock pins the DB's clock so month rollovers and daily samples can be
// driven without waiting for a calendar.
func atClock(db *DB, t *time.Time) {
	db.Now = func() time.Time { return *t }
}

func TestClassBOpsCountAndReset(t *testing.T) {
	db := openTestDB(t)
	now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	atClock(db, &now)

	if err := db.AddClassBOps(3); err != nil {
		t.Fatalf("AddClassBOps: %v", err)
	}
	if err := db.AddClassBOps(4); err != nil {
		t.Fatalf("AddClassBOps: %v", err)
	}
	used, resetAt, err := db.ClassBOpsThisMonth()
	if err != nil {
		t.Fatalf("ClassBOpsThisMonth: %v", err)
	}
	if used != 7 {
		t.Errorf("used = %d, want 7", used)
	}
	if want := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC); !resetAt.Equal(want) {
		t.Errorf("resetAt = %v, want %v", resetAt, want)
	}

	// Next month the count starts again, without a write to trigger it.
	now = time.Date(2026, 10, 1, 0, 0, 1, 0, time.UTC)
	used, _, err = db.ClassBOpsThisMonth()
	if err != nil {
		t.Fatalf("ClassBOpsThisMonth: %v", err)
	}
	if used != 0 {
		t.Errorf("used after rollover = %d, want 0", used)
	}
}

// The two counters must not share storage, or a restore would eat into the
// Class A allowance the dashboard warns against.
func TestClassAAndClassBCountSeparately(t *testing.T) {
	db := openTestDB(t)
	if err := db.AddOps(5); err != nil {
		t.Fatalf("AddOps: %v", err)
	}
	if err := db.AddClassBOps(9); err != nil {
		t.Fatalf("AddClassBOps: %v", err)
	}
	a, _, err := db.OpsThisMonth()
	if err != nil {
		t.Fatalf("OpsThisMonth: %v", err)
	}
	b, _, err := db.ClassBOpsThisMonth()
	if err != nil {
		t.Fatalf("ClassBOpsThisMonth: %v", err)
	}
	if a != 5 {
		t.Errorf("Class A = %d, want 5", a)
	}
	if b != 9 {
		t.Errorf("Class B = %d, want 9", b)
	}
}

// An index written before this counter existed has no key for it, and must
// read back as zero rather than failing.
func TestClassBOpsOnFreshIndexIsZero(t *testing.T) {
	db := openTestDB(t)
	used, _, err := db.ClassBOpsThisMonth()
	if err != nil {
		t.Fatalf("ClassBOpsThisMonth on a fresh index: %v", err)
	}
	if used != 0 {
		t.Errorf("used = %d, want 0", used)
	}
}

func TestStoredBytesSumsAcrossSets(t *testing.T) {
	db := openTestDB(t)
	put := func(set, key string, size int64, kind Kind) {
		t.Helper()
		rec := sampleRecord(key)
		rec.Size = size
		rec.Kind = kind
		if err := db.Put(set, rec); err != nil {
			t.Fatalf("Put: %v", err)
		}
	}
	put("docs", "a.txt", 1000, KindFile)
	put("docs", "b.txt", 2000, KindFile)
	put("photos", "c.jpg", 5000, KindFile)

	bytes, objects, err := db.StoredBytes()
	if err != nil {
		t.Fatalf("StoredBytes: %v", err)
	}
	if bytes != 8000 {
		t.Errorf("bytes = %d, want 8000", bytes)
	}
	if objects != 3 {
		t.Errorf("objects = %d, want 3", objects)
	}
}

// Symlinks and empty directories are stored as bodiless objects: they cost an
// operation but no storage, so counting their Size would overstate the bill.
func TestStoredBytesIgnoresBodilessObjects(t *testing.T) {
	db := openTestDB(t)
	file := sampleRecord("real.txt")
	file.Size = 4000
	file.Kind = KindFile
	link := sampleRecord("link")
	link.Size = 9999 // whatever is recorded, nothing is stored
	link.Kind = KindSymlink
	dir := sampleRecord("empty/")
	dir.Size = 8888
	dir.Kind = KindEmptyDir
	for _, rec := range []Record{file, link, dir} {
		if err := db.Put("docs", rec); err != nil {
			t.Fatalf("Put: %v", err)
		}
	}

	bytes, objects, err := db.StoredBytes()
	if err != nil {
		t.Fatalf("StoredBytes: %v", err)
	}
	if bytes != 4000 {
		t.Errorf("bytes = %d, want 4000 (only the file has a body)", bytes)
	}
	// They still exist as objects, and still cost operations to write.
	if objects != 3 {
		t.Errorf("objects = %d, want 3", objects)
	}
}

func TestStoredBytesOnFreshIndexIsZero(t *testing.T) {
	db := openTestDB(t)
	bytes, objects, err := db.StoredBytes()
	if err != nil {
		t.Fatalf("StoredBytes: %v", err)
	}
	if bytes != 0 || objects != 0 {
		t.Errorf("got %d bytes / %d objects, want 0 / 0", bytes, objects)
	}
}

func TestStorageSamplesOnePerDay(t *testing.T) {
	db := openTestDB(t)
	now := time.Date(2026, 9, 10, 9, 0, 0, 0, time.UTC)
	atClock(db, &now)

	if err := db.RecordStorageSample(1000); err != nil {
		t.Fatalf("RecordStorageSample: %v", err)
	}
	// A second reading the same day replaces the first rather than adding
	// a second entry.
	now = time.Date(2026, 9, 10, 21, 0, 0, 0, time.UTC)
	if err := db.RecordStorageSample(2000); err != nil {
		t.Fatalf("RecordStorageSample: %v", err)
	}
	samples, err := db.StorageSamples()
	if err != nil {
		t.Fatalf("StorageSamples: %v", err)
	}
	if len(samples) != 1 {
		t.Fatalf("len(samples) = %d, want 1", len(samples))
	}
	if samples[0].Bytes != 2000 {
		t.Errorf("bytes = %d, want the later reading 2000", samples[0].Bytes)
	}
	// Timestamped at the start of the UTC day, not the moment observed.
	if want := time.Date(2026, 9, 10, 0, 0, 0, 0, time.UTC); !samples[0].At.Equal(want) {
		t.Errorf("At = %v, want the day's start %v", samples[0].At, want)
	}

	now = time.Date(2026, 9, 11, 9, 0, 0, 0, time.UTC)
	if err := db.RecordStorageSample(3000); err != nil {
		t.Fatalf("RecordStorageSample: %v", err)
	}
	samples, err = db.StorageSamples()
	if err != nil {
		t.Fatalf("StorageSamples: %v", err)
	}
	if len(samples) != 2 {
		t.Fatalf("len(samples) = %d, want 2 after a second day", len(samples))
	}
}

// The series is per calendar month, because the free tier and the bill are.
func TestStorageSamplesResetEachMonth(t *testing.T) {
	db := openTestDB(t)
	now := time.Date(2026, 9, 28, 9, 0, 0, 0, time.UTC)
	atClock(db, &now)
	if err := db.RecordStorageSample(1000); err != nil {
		t.Fatalf("RecordStorageSample: %v", err)
	}

	now = time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	samples, err := db.StorageSamples()
	if err != nil {
		t.Fatalf("StorageSamples: %v", err)
	}
	if len(samples) != 0 {
		t.Errorf("last month's samples survived the rollover: %v", samples)
	}

	if err := db.RecordStorageSample(5000); err != nil {
		t.Fatalf("RecordStorageSample: %v", err)
	}
	samples, err = db.StorageSamples()
	if err != nil {
		t.Fatalf("StorageSamples: %v", err)
	}
	if len(samples) != 1 || samples[0].Bytes != 5000 {
		t.Errorf("samples = %v, want just the new month's 5000", samples)
	}
}

func TestStorageSamplesOnFreshIndexIsEmpty(t *testing.T) {
	db := openTestDB(t)
	samples, err := db.StorageSamples()
	if err != nil {
		t.Fatalf("StorageSamples: %v", err)
	}
	if len(samples) != 0 {
		t.Errorf("samples = %v, want none", samples)
	}
}

func TestRecordStorageSampleClampsNegative(t *testing.T) {
	db := openTestDB(t)
	if err := db.RecordStorageSample(-5); err != nil {
		t.Fatalf("RecordStorageSample: %v", err)
	}
	samples, err := db.StorageSamples()
	if err != nil {
		t.Fatalf("StorageSamples: %v", err)
	}
	if len(samples) != 1 || samples[0].Bytes != 0 {
		t.Errorf("samples = %v, want a single zero", samples)
	}
}
