package index

import (
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func openTestDB(t *testing.T) *DB {
	t.Helper()
	dir := t.TempDir()
	db, err := Open(filepath.Join(dir, "sub", "index.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	// bbolt fsyncs on every commit, and nothing in this package's tests
	// survives the process to care. On a Windows CI runner that fsync costs
	// ~40ms against ~2ms here, which is what put TestConcurrentAccess's 4,000
	// serialised write transactions past the 10-minute test timeout while the
	// same test finished in 8s on Linux. Turning it off removes the disk, not
	// the contention: the same transactions, locks and page writes still
	// happen, so what the tests actually assert -- and what -race sees -- is
	// unchanged.
	db.bolt.NoSync = true
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return db
}

func sampleRecord(key string) Record {
	return Record{
		Key:        key,
		Size:       1234,
		ModTime:    time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC).UnixNano(),
		ETag:       "etag-" + key,
		UploadedAt: time.Date(2026, 3, 1, 12, 5, 0, 0, time.UTC).UnixNano(),
		Kind:       KindFile,
	}
}

func TestPutGetRoundTrip(t *testing.T) {
	db := openTestDB(t)
	rec := sampleRecord("docs/a.txt")
	rec.Target = "" // regular file, no target

	if err := db.Put("photos", rec); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, err := db.Get("photos", rec.Key)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != rec {
		t.Errorf("round trip mismatch:\n got %+v\nwant %+v", got, rec)
	}
}

func TestPutGetRoundTripSymlink(t *testing.T) {
	db := openTestDB(t)
	rec := sampleRecord("bin/tool")
	rec.Kind = KindSymlink
	rec.Size = 0
	rec.Target = "../opt/tool-1.2/bin/tool"

	if err := db.Put("photos", rec); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, err := db.Get("photos", rec.Key)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != rec {
		t.Errorf("round trip mismatch:\n got %+v\nwant %+v", got, rec)
	}
}

func TestSetIsolation(t *testing.T) {
	db := openTestDB(t)
	recA := sampleRecord("shared/key.txt")
	recA.ETag = "from-set-a"
	recB := sampleRecord("shared/key.txt")
	recB.ETag = "from-set-b"

	if err := db.Put("set-a", recA); err != nil {
		t.Fatalf("Put set-a: %v", err)
	}
	if err := db.Put("set-b", recB); err != nil {
		t.Fatalf("Put set-b: %v", err)
	}

	gotA, err := db.Get("set-a", "shared/key.txt")
	if err != nil {
		t.Fatalf("Get set-a: %v", err)
	}
	gotB, err := db.Get("set-b", "shared/key.txt")
	if err != nil {
		t.Fatalf("Get set-b: %v", err)
	}
	if gotA.ETag != "from-set-a" {
		t.Errorf("set-a record leaked: got etag %q", gotA.ETag)
	}
	if gotB.ETag != "from-set-b" {
		t.Errorf("set-b record leaked: got etag %q", gotB.ETag)
	}

	countA, err := db.Count("set-a")
	if err != nil {
		t.Fatalf("Count set-a: %v", err)
	}
	if countA != 1 {
		t.Errorf("Count set-a = %d, want 1", countA)
	}
}

func TestChanged(t *testing.T) {
	base := time.Date(2026, 6, 15, 9, 0, 0, 0, time.UTC)
	rec := Record{Size: 100, ModTime: base.UnixNano()}

	cases := []struct {
		name  string
		size  int64
		delta time.Duration
		want  bool
	}{
		{"identical", 100, 0, false},
		{"size differs by one byte", 101, 0, true},
		{"size differs, smaller", 99, 0, true},
		{"mtime +1s", 100, 1 * time.Second, false},
		{"mtime +2s exact boundary", 100, 2 * time.Second, false},
		{"mtime +3s", 100, 3 * time.Second, true},
		{"mtime -2s exact boundary", 100, -2 * time.Second, false},
		{"mtime -3s file went backwards", 100, -3 * time.Second, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Changed(rec, tc.size, base.Add(tc.delta))
			if got != tc.want {
				t.Errorf("Changed(size=%d, delta=%v) = %v, want %v", tc.size, tc.delta, got, tc.want)
			}
		})
	}
}

func TestModTimeToleranceConstant(t *testing.T) {
	if ModTimeTolerance != 2*time.Second {
		t.Errorf("ModTimeTolerance = %v, want 2s", ModTimeTolerance)
	}
}

func TestPutManyBatch(t *testing.T) {
	db := openTestDB(t)
	const n = 5000
	recs := make([]Record, n)
	for i := 0; i < n; i++ {
		recs[i] = sampleRecord(fmt.Sprintf("file-%05d.bin", i))
	}
	if err := db.PutMany("bulk", recs); err != nil {
		t.Fatalf("PutMany: %v", err)
	}

	count, err := db.Count("bulk")
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if count != n {
		t.Fatalf("Count = %d, want %d", count, n)
	}

	// Spot check a handful, not just the count.
	for _, i := range []int{0, 1, n / 2, n - 1} {
		key := fmt.Sprintf("file-%05d.bin", i)
		got, err := db.Get("bulk", key)
		if err != nil {
			t.Fatalf("Get(%q): %v", key, err)
		}
		if got.Key != key {
			t.Errorf("Get(%q).Key = %q", key, got.Key)
		}
	}
}

func TestDeleteRemovesRecord(t *testing.T) {
	db := openTestDB(t)
	rec := sampleRecord("to-delete.txt")
	if err := db.Put("set", rec); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := db.Delete("set", rec.Key); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := db.Get("set", rec.Key); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get after Delete: err = %v, want ErrNotFound", err)
	}
}

func TestGetMissingKeyReportsNotFound(t *testing.T) {
	db := openTestDB(t)

	// Missing key in a set that has never been created.
	_, err := db.Get("never-touched", "nope.txt")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get in unknown set: err = %v, want ErrNotFound", err)
	}

	// Missing key in a set that does exist.
	if err := db.Put("real-set", sampleRecord("present.txt")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	rec, err := db.Get("real-set", "absent.txt")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get missing key: err = %v, want ErrNotFound", err)
	}
	if rec != (Record{}) {
		t.Errorf("Get missing key returned non-zero record %+v alongside the error", rec)
	}
}

func TestDeleteManyRemovesAll(t *testing.T) {
	db := openTestDB(t)
	keys := []string{"a.txt", "b.txt", "c.txt", "d.txt"}
	for _, k := range keys {
		if err := db.Put("set", sampleRecord(k)); err != nil {
			t.Fatalf("Put(%q): %v", k, err)
		}
	}
	if err := db.DeleteMany("set", keys[:3]); err != nil {
		t.Fatalf("DeleteMany: %v", err)
	}
	count, err := db.Count("set")
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if count != 1 {
		t.Fatalf("Count after DeleteMany = %d, want 1", count)
	}
	if _, err := db.Get("set", "d.txt"); err != nil {
		t.Errorf("survivor d.txt: %v", err)
	}
}

func TestAllIteratesEveryRecordExactlyOnce(t *testing.T) {
	db := openTestDB(t)
	want := map[string]bool{}
	for i := 0; i < 25; i++ {
		key := fmt.Sprintf("item-%02d.txt", i)
		if err := db.Put("iter-set", sampleRecord(key)); err != nil {
			t.Fatalf("Put(%q): %v", key, err)
		}
		want[key] = true
	}

	recs, err := db.All("iter-set")
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(recs) != len(want) {
		t.Fatalf("All returned %d records, want %d", len(recs), len(want))
	}
	seen := map[string]int{}
	for _, r := range recs {
		seen[r.Key]++
	}
	for k := range want {
		if seen[k] != 1 {
			t.Errorf("key %q seen %d times, want exactly 1", k, seen[k])
		}
	}
}

func TestAllOnUnknownSetIsEmptyNotError(t *testing.T) {
	db := openTestDB(t)
	recs, err := db.All("nothing-here")
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(recs) != 0 {
		t.Errorf("All on unknown set = %d records, want 0", len(recs))
	}
}

func TestDropSetRemovesOnlyThatSet(t *testing.T) {
	db := openTestDB(t)
	if err := db.Put("keep", sampleRecord("k.txt")); err != nil {
		t.Fatalf("Put keep: %v", err)
	}
	if err := db.Put("drop", sampleRecord("d.txt")); err != nil {
		t.Fatalf("Put drop: %v", err)
	}

	if err := db.DropSet("drop"); err != nil {
		t.Fatalf("DropSet: %v", err)
	}

	if _, err := db.Get("drop", "d.txt"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get in dropped set: err = %v, want ErrNotFound", err)
	}
	if _, err := db.Get("keep", "k.txt"); err != nil {
		t.Errorf("Get in surviving set: %v", err)
	}

	// Dropping an already-gone (or never-created) set is not an error.
	if err := db.DropSet("drop"); err != nil {
		t.Errorf("DropSet again: %v", err)
	}
	if err := db.DropSet("never-existed"); err != nil {
		t.Errorf("DropSet never-existed: %v", err)
	}
}

func TestOpsCounterAccumulates(t *testing.T) {
	db := openTestDB(t)
	frozen := time.Date(2026, 4, 10, 0, 0, 0, 0, time.UTC)
	db.Now = func() time.Time { return frozen }

	if err := db.AddOps(100); err != nil {
		t.Fatalf("AddOps: %v", err)
	}
	if err := db.AddOps(250); err != nil {
		t.Fatalf("AddOps: %v", err)
	}
	used, resetAt, err := db.OpsThisMonth()
	if err != nil {
		t.Fatalf("OpsThisMonth: %v", err)
	}
	if used != 350 {
		t.Errorf("used = %d, want 350", used)
	}
	wantReset := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	if !resetAt.Equal(wantReset) {
		t.Errorf("resetAt = %v, want %v", resetAt, wantReset)
	}
}

func TestOpsCounterResetsOnMonthRollover(t *testing.T) {
	db := openTestDB(t)
	april := time.Date(2026, 4, 20, 0, 0, 0, 0, time.UTC)
	db.Now = func() time.Time { return april }
	if err := db.AddOps(999_000); err != nil {
		t.Fatalf("AddOps: %v", err)
	}

	// Cross into May without any intervening AddOps call: a plain read
	// must still see the reset, not a stale total.
	may := time.Date(2026, 5, 1, 0, 0, 1, 0, time.UTC)
	db.Now = func() time.Time { return may }
	used, _, err := db.OpsThisMonth()
	if err != nil {
		t.Fatalf("OpsThisMonth: %v", err)
	}
	if used != 0 {
		t.Errorf("used after rollover (no AddOps yet) = %d, want 0", used)
	}

	// And AddOps itself starts the new month's count from zero, not from
	// wherever April left off.
	if err := db.AddOps(42); err != nil {
		t.Fatalf("AddOps: %v", err)
	}
	used, resetAt, err := db.OpsThisMonth()
	if err != nil {
		t.Fatalf("OpsThisMonth: %v", err)
	}
	if used != 42 {
		t.Errorf("used after rollover AddOps = %d, want 42", used)
	}
	wantReset := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	if !resetAt.Equal(wantReset) {
		t.Errorf("resetAt = %v, want %v", resetAt, wantReset)
	}
}

func TestFreeTierConstant(t *testing.T) {
	if FreeTierOpsPerMonth != 1_000_000 {
		t.Errorf("FreeTierOpsPerMonth = %d, want 1,000,000", FreeTierOpsPerMonth)
	}
}

// TestConcurrentAccess hammers the DB from many goroutines at once: distinct
// sets writing and reading concurrently, plus the shared op counter being
// bumped from every goroutine. Meaningful primarily under -race.
func TestConcurrentAccess(t *testing.T) {
	db := openTestDB(t)
	const goroutines = 20
	const perGoroutine = 100

	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			set := fmt.Sprintf("set-%d", g%4) // several goroutines share a set
			for i := 0; i < perGoroutine; i++ {
				key := fmt.Sprintf("g%d-item-%d.txt", g, i)
				rec := sampleRecord(key)
				if err := db.Put(set, rec); err != nil {
					t.Errorf("Put: %v", err)
					return
				}
				if _, err := db.Get(set, key); err != nil {
					t.Errorf("Get: %v", err)
					return
				}
				if err := db.AddOps(1); err != nil {
					t.Errorf("AddOps: %v", err)
					return
				}
				if _, err := db.Count(set); err != nil {
					t.Errorf("Count: %v", err)
					return
				}
			}
		}(g)
	}
	wg.Wait()

	used, _, err := db.OpsThisMonth()
	if err != nil {
		t.Fatalf("OpsThisMonth: %v", err)
	}
	if used != goroutines*perGoroutine {
		t.Errorf("ops used = %d, want %d", used, goroutines*perGoroutine)
	}

	for g := 0; g < 4; g++ {
		set := fmt.Sprintf("set-%d", g)
		recs, err := db.All(set)
		if err != nil {
			t.Fatalf("All(%q): %v", set, err)
		}
		if len(recs) != 5*perGoroutine { // 20 goroutines / 4 sets = 5 each
			t.Errorf("All(%q) = %d records, want %d", set, len(recs), 5*perGoroutine)
		}
	}
}

func TestRenameSetMovesEveryRecord(t *testing.T) {
	db := openTestDB(t)
	for i := 0; i < 25; i++ {
		if err := db.Put("photos", sampleRecord(fmt.Sprintf("a/%d.jpg", i))); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Put("music", sampleRecord("untouched.flac")); err != nil {
		t.Fatal(err)
	}

	if err := db.RenameSet("photos", "pictures"); err != nil {
		t.Fatalf("RenameSet: %v", err)
	}

	moved, err := db.All("pictures")
	if err != nil {
		t.Fatal(err)
	}
	if len(moved) != 25 {
		t.Errorf("pictures holds %d records, want 25", len(moved))
	}
	got, err := db.Get("pictures", "a/7.jpg")
	if err != nil {
		t.Fatalf("Get after rename: %v", err)
	}
	if got != sampleRecord("a/7.jpg") {
		t.Errorf("record did not survive the rename intact:\n got %+v\nwant %+v", got, sampleRecord("a/7.jpg"))
	}

	// The old name must be gone, not merely shadowed: leaving it behind is
	// what would let a later set of the same name inherit stale records.
	left, err := db.All("photos")
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 0 {
		t.Errorf("the old name still holds %d records", len(left))
	}

	// Every other set is untouched.
	if _, err := db.Get("music", "untouched.flac"); err != nil {
		t.Errorf("renaming one set disturbed another: %v", err)
	}
}

func TestRenameSetRefusesToMergeOntoAnExistingSet(t *testing.T) {
	db := openTestDB(t)
	if err := db.Put("photos", sampleRecord("a.jpg")); err != nil {
		t.Fatal(err)
	}
	if err := db.Put("pictures", sampleRecord("b.jpg")); err != nil {
		t.Fatal(err)
	}

	if err := db.RenameSet("photos", "pictures"); err == nil {
		t.Fatal("RenameSet merged two sets' records into one bucket instead of refusing")
	}

	// And it refused before touching either side, so neither set lost anything.
	if _, err := db.Get("photos", "a.jpg"); err != nil {
		t.Errorf("the source set lost its records to a refused rename: %v", err)
	}
	if _, err := db.Get("pictures", "b.jpg"); err != nil {
		t.Errorf("the target set lost its records to a refused rename: %v", err)
	}
}

func TestRenameSetOfANeverBackedUpSetIsNotAnError(t *testing.T) {
	db := openTestDB(t)
	// A set added but never run has no bucket at all, and renaming it is an
	// ordinary thing to do.
	if err := db.RenameSet("brand-new", "renamed"); err != nil {
		t.Errorf("RenameSet on a set with no records: %v", err)
	}
	if err := db.RenameSet("same", "same"); err != nil {
		t.Errorf("RenameSet onto the same name: %v", err)
	}
}
