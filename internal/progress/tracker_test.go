package progress

import (
	"math"
	"sync"
	"testing"
	"time"
)

// fakeClock lets tests drive Tracker with simulated time instead of real
// sleeps, so the 30s EWMA window and the 5s ETA gate can be exercised in
// milliseconds of real test runtime and with exact, reproducible timing.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock(start time.Time) *fakeClock {
	return &fakeClock{t: start}
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func TestNoETABeforeGate(t *testing.T) {
	clock := newFakeClock(time.Unix(0, 0))
	tr := New(1000, 10, clock.now)

	// Well past 1% but before 5s: gate must hold on elapsed time.
	tr.Complete(500) // 50% of bytes, way over the 1% threshold
	clock.advance(1 * time.Second)
	snap := tr.Snapshot()
	if snap.ETAKnown {
		t.Fatalf("ETAKnown true after only 1s elapsed; want false before the 5s gate")
	}

	// Past 5s but under 1% done: gate must hold on percent.
	clock2 := newFakeClock(time.Unix(0, 0))
	tr2 := New(1_000_000, 1000, clock2.now)
	tr2.Complete(1) // far under 1%
	clock2.advance(10 * time.Second)
	snap2 := tr2.Snapshot()
	if snap2.ETAKnown {
		t.Fatalf("ETAKnown true at <1%% done; want false before the 1%% gate")
	}

	// Both satisfied: gate must open.
	clock3 := newFakeClock(time.Unix(0, 0))
	tr3 := New(1000, 10, clock3.now)
	tr3.Complete(100) // 10%, over 1%
	clock3.advance(6 * time.Second)
	snap3 := tr3.Snapshot()
	if !snap3.ETAKnown {
		t.Fatalf("ETAKnown false after 6s and 10%% done; want true once both gates clear")
	}
}

func TestETARisesWhenStalled(t *testing.T) {
	clock := newFakeClock(time.Unix(0, 0))
	tr := New(1_000_000, 100, clock.now)

	// Establish a real rate so the gate opens and we have a baseline ETA.
	for i := 0; i < 20; i++ {
		tr.Complete(10_000)
		clock.advance(500 * time.Millisecond)
	}
	base := tr.Snapshot()
	if !base.ETAKnown {
		t.Fatalf("expected ETAKnown after warmup, got false")
	}

	// Now stall: no more completions, only time passing. The ETA must
	// rise (the remaining work hasn't shrunk, but the tracker should stop
	// trusting a rate that hasn't produced anything in a while) rather
	// than stay frozen at the pre-stall estimate.
	var last = base.ETA
	rose := false
	for i := 0; i < 20; i++ {
		clock.advance(3 * time.Second)
		snap := tr.Snapshot()
		if snap.ETA > last {
			rose = true
		}
		if snap.ETA < last {
			t.Fatalf("ETA decreased during a stall: %v -> %v", last, snap.ETA)
		}
		last = snap.ETA
	}
	if !rose {
		t.Fatalf("ETA never rose across a 60s stall; want a rising ETA, not a frozen one")
	}
}

func TestRatesAndETAAreZeroSafe(t *testing.T) {
	clock := newFakeClock(time.Unix(0, 0))
	tr := New(0, 0, clock.now)
	snap := tr.Snapshot()
	assertFinite(t, "ByteRate", snap.ByteRate)
	assertFinite(t, "FileRate", snap.FileRate)
	assertFinite(t, "Percent", snap.Percent)
	assertFinite(t, "ETA seconds", snap.ETA.Seconds())

	// Zero total bytes, nonzero files.
	clock2 := newFakeClock(time.Unix(0, 0))
	tr2 := New(0, 5, clock2.now)
	tr2.Complete(0)
	clock2.advance(10 * time.Second)
	snap2 := tr2.Snapshot()
	assertFinite(t, "ByteRate (0 bytes total)", snap2.ByteRate)
	assertFinite(t, "ETA seconds (0 bytes total)", snap2.ETA.Seconds())
	if math.IsNaN(snap2.Percent) || math.IsInf(snap2.Percent, 0) {
		t.Fatalf("Percent not finite: %v", snap2.Percent)
	}

	// Zero total files, nonzero bytes.
	clock3 := newFakeClock(time.Unix(0, 0))
	tr3 := New(5000, 0, clock3.now)
	tr3.AddBytes(1000)
	clock3.advance(10 * time.Second)
	snap3 := tr3.Snapshot()
	assertFinite(t, "FileRate (0 files total)", snap3.FileRate)
	assertFinite(t, "ETA seconds (0 files total)", snap3.ETA.Seconds())
}

func assertFinite(t *testing.T, label string, v float64) {
	t.Helper()
	if math.IsNaN(v) {
		t.Fatalf("%s is NaN", label)
	}
	if math.IsInf(v, 0) {
		t.Fatalf("%s is Inf", label)
	}
	if v < 0 {
		t.Fatalf("%s is negative: %v", label, v)
	}
}

func TestZeroPlanDoesNotPanic(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("panicked on a zero plan: %v", r)
		}
	}()
	clock := newFakeClock(time.Unix(0, 0))
	tr := New(0, 0, clock.now)
	clock.advance(10 * time.Second)
	snap := tr.Snapshot()
	if snap.Percent != 100 {
		t.Fatalf("zero-everything plan: want Percent=100 (nothing to do), got %v", snap.Percent)
	}
	if snap.BytesTotal != 0 || snap.FilesTotal != 0 {
		t.Fatalf("unexpected totals: %+v", snap)
	}
}

func TestCompleteAndAddBytesAccumulateDoneBytes(t *testing.T) {
	clock := newFakeClock(time.Unix(0, 0))
	tr := New(1000, 2, clock.now)

	tr.AddBytes(300) // partial progress on file 1
	clock.advance(time.Second)
	tr.AddBytes(200) // more partial progress on file 1
	clock.advance(time.Second)
	tr.Complete(0) // file 1 finishes; all its bytes were already reported
	clock.advance(time.Second)
	tr.Complete(500) // file 2 finishes in one shot

	snap := tr.Snapshot()
	if snap.BytesDone != 1000 {
		t.Fatalf("BytesDone = %d, want 1000", snap.BytesDone)
	}
	if snap.FilesDone != 2 {
		t.Fatalf("FilesDone = %d, want 2", snap.FilesDone)
	}
}

func TestDoneClampsToTotal(t *testing.T) {
	clock := newFakeClock(time.Unix(0, 0))
	tr := New(100, 1, clock.now)
	tr.Complete(150) // overshoot, e.g. a source size estimate was wrong
	snap := tr.Snapshot()
	if snap.BytesDone != 100 {
		t.Fatalf("BytesDone = %d, want clamped to 100", snap.BytesDone)
	}
	if snap.Percent != 100 {
		t.Fatalf("Percent = %v, want 100", snap.Percent)
	}
}

func TestSingleLargeFileByteRateFeedsFromAddBytes(t *testing.T) {
	// A single big file must not show a flat/zero byte rate for its whole
	// duration just because Complete() is only called once at the end.
	clock := newFakeClock(time.Unix(0, 0))
	tr := New(1_000_000_000, 1, clock.now)

	for i := 0; i < 50; i++ {
		tr.AddBytes(10_000_000)
		clock.advance(time.Second)
	}
	snap := tr.Snapshot()
	if snap.ByteRate <= 0 {
		t.Fatalf("ByteRate = %v after sustained AddBytes calls, want > 0", snap.ByteRate)
	}
	if !snap.ETAKnown {
		t.Fatalf("ETAKnown = false after 50s of steady partial progress, want true")
	}
}

func TestConcurrentUseIsRace_Free(t *testing.T) {
	clock := newFakeClock(time.Unix(0, 0))
	tr := New(1_000_000, 1000, clock.now)

	var wg sync.WaitGroup
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				tr.Complete(100)
				_ = tr.Snapshot()
			}
		}()
	}
	wg.Wait()

	snap := tr.Snapshot()
	if snap.FilesDone != 800 {
		t.Fatalf("FilesDone = %d, want 800", snap.FilesDone)
	}
}
