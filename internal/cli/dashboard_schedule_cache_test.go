package cli

import (
	"context"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/saurabhhbansal/r2backup/internal/schedule"
)

// TestLoadCachesTheSchedulerRead is finding M4: Load runs on the dashboard's
// once-a-second UI tick, and schedule.Current shells out to the OS scheduler
// -- systemctl --user show on Linux, two schtasks /query calls on Windows,
// launchctl list on macOS. Calling that once a second for as long as a
// window is open spawns a process (two, on Windows) every second, for no
// reason: the schedule tab does not need to be that fresh.
//
// This fails before the cache existed -- ten Load calls meant ten calls to
// scheduleCurrent -- and passes once Load answers from the cache described
// on the dashboard struct instead.
func TestLoadCachesTheSchedulerRead(t *testing.T) {
	origCurrent := scheduleCurrent
	t.Cleanup(func() { scheduleCurrent = origCurrent })

	var calls int32
	scheduleCurrent = func(string) (schedule.Status, error) {
		atomic.AddInt32(&calls, 1)
		return schedule.Status{Registered: true, Interval: 30 * time.Minute}, nil
	}

	t.Setenv("R2BACKUP_DATA_DIR", t.TempDir())
	d, err := openDashboard(&Options{Out: os.Stderr, Err: os.Stderr})
	if err != nil {
		t.Fatal(err)
	}
	defer d.close()

	now := time.Now()
	d.now = func() time.Time { return now }

	ctx := context.Background()
	for i := 0; i < 10; i++ {
		if _, _, err := d.Load(ctx); err != nil {
			t.Fatalf("Load #%d: %v", i, err)
		}
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("scheduleCurrent was called %d times across 10 Loads within the cache window, want 1", got)
	}

	// Advance the injected clock past scheduleCacheTTL: the next Load must
	// ask the OS again rather than going on repeating the first answer
	// forever.
	now = now.Add(scheduleCacheTTL + time.Second)
	if _, _, err := d.Load(ctx); err != nil {
		t.Fatalf("Load after the cache window expired: %v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Errorf("scheduleCurrent was called %d times after the cache window expired, want 2", got)
	}
}

// TestSchedulingThroughTheDashboardInvalidatesTheCache covers the other half
// of M4: a cache with no invalidation would make the Schedule tab lie for up
// to scheduleCacheTTL right after the user presses the toggle -- the one
// moment the tab most needs to be right. Schedule (on) is exercised here;
// TestTurningTheScheduleOffInvalidatesTheCache below covers off.
func TestSchedulingThroughTheDashboardInvalidatesTheCache(t *testing.T) {
	origCurrent, origInstall := scheduleCurrent, scheduleInstall
	t.Cleanup(func() { scheduleCurrent, scheduleInstall = origCurrent, origInstall })

	var registered atomic.Bool
	scheduleCurrent = func(string) (schedule.Status, error) {
		return schedule.Status{Registered: registered.Load(), Interval: 30 * time.Minute}, nil
	}
	scheduleInstall = func(schedule.Entry) error {
		registered.Store(true)
		return nil
	}

	t.Setenv("R2BACKUP_DATA_DIR", t.TempDir())
	d, err := openDashboard(&Options{Out: os.Stderr, Err: os.Stderr})
	if err != nil {
		t.Fatal(err)
	}
	defer d.close()

	now := time.Now()
	d.now = func() time.Time { return now }

	ctx := context.Background()
	_, ov, err := d.Load(ctx)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if ov.Scheduled {
		t.Fatal("Scheduled = true before anything was registered")
	}

	// Well inside the cache window -- if this were still cached, the next
	// Load would go on reporting Scheduled = false.
	if err := d.Schedule(ctx, 30, false); err != nil {
		t.Fatalf("Schedule: %v", err)
	}
	_, ov, err = d.Load(ctx)
	if err != nil {
		t.Fatalf("Load after Schedule: %v", err)
	}
	if !ov.Scheduled {
		t.Error("Scheduled = false right after Schedule registered an entry, want true -- the cache was not invalidated")
	}
}

// TestTurningTheScheduleOffInvalidatesTheCache is the off half of the same
// requirement: a user who presses the toggle to stop scheduled backups must
// see the Schedule tab agree immediately, not up to scheduleCacheTTL later.
func TestTurningTheScheduleOffInvalidatesTheCache(t *testing.T) {
	origCurrent, origRemove := scheduleCurrent, scheduleRemove
	t.Cleanup(func() { scheduleCurrent, scheduleRemove = origCurrent, origRemove })

	registered := true
	scheduleCurrent = func(string) (schedule.Status, error) {
		return schedule.Status{Registered: registered, Interval: 30 * time.Minute}, nil
	}
	scheduleRemove = func(string) error {
		registered = false
		return nil
	}

	t.Setenv("R2BACKUP_DATA_DIR", t.TempDir())
	d, err := openDashboard(&Options{Out: os.Stderr, Err: os.Stderr})
	if err != nil {
		t.Fatal(err)
	}
	defer d.close()

	now := time.Now()
	d.now = func() time.Time { return now }

	ctx := context.Background()
	_, ov, err := d.Load(ctx)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !ov.Scheduled {
		t.Fatal("Scheduled = false before anything was removed")
	}

	if err := d.Schedule(ctx, 0, true); err != nil {
		t.Fatalf("Schedule (off): %v", err)
	}
	_, ov, err = d.Load(ctx)
	if err != nil {
		t.Fatalf("Load after Schedule (off): %v", err)
	}
	if ov.Scheduled {
		t.Error("Scheduled = true right after Schedule removed the entry, want false -- the cache was not invalidated")
	}
}
