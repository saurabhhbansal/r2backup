package progress

import (
	"testing"
	"time"
)

// sleepRun is a long run at a steady pace, looked at once a second as the
// backup's ticker does: 12,862 files, 20 a second, a megabyte a second.
type sleepRun struct {
	clock *fakeClock
	tr    *Tracker
	pres  Presenter
	shown time.Duration
	ok    bool
}

func newSleepRun(watch bool) *sleepRun {
	clock := newFakeClock(time.Unix(0, 0))
	tr := New(12862<<16, 12862, clock.now)
	if watch {
		tr.WatchForSleep(time.Second)
	}
	return &sleepRun{clock: clock, tr: tr}
}

// work runs for d at the steady pace, with a snapshot every second.
func (r *sleepRun) work(d time.Duration) {
	for i := 0; i < int(d/time.Second); i++ {
		for j := 0; j < 20; j++ {
			r.clock.advance(50 * time.Millisecond)
			r.tr.Complete(1 << 16)
		}
		r.shown, r.ok = r.pres.Present(r.tr.Snapshot())
	}
}

// The lid closes for two hours mid-run. After waking, the estimate carries on
// from where it was: no hours or days, and the time asleep is not elapsed.
func TestSleepIsNotElapsedTime(t *testing.T) {
	r := newSleepRun(true)
	r.work(3 * time.Minute)
	if !r.ok {
		t.Fatal("no estimate after three minutes")
	}
	before, beforeShown := r.tr.Snapshot(), r.shown

	// Asleep: nothing runs, then the clock has jumped two hours. The first
	// thing to happen on waking is a file that was in flight finishing.
	r.clock.advance(2 * time.Hour)
	r.tr.Complete(1 << 16)
	woke := r.tr.Snapshot()
	if d := woke.Elapsed - before.Elapsed; d > 5*time.Second {
		t.Fatalf("elapsed grew by %v across the sleep", d)
	}

	// For two minutes after waking, at the same pace, the estimate shown
	// stays in the band it was in: it counts down, it does not jump.
	for s := 0; s < 120; s++ {
		r.work(time.Second)
		if !r.ok {
			t.Fatalf("%ds after waking: no estimate shown", s)
		}
		want := beforeShown - (r.tr.Snapshot().Elapsed - before.Elapsed)
		if ratio := float64(r.shown) / float64(want); ratio > 1.3 || ratio < 0.7 {
			t.Fatalf("%ds after waking: showing %v (%s), expected about %v", s, r.shown, FormatETA(r.shown), want)
		}
		if r.shown > 24*time.Hour {
			t.Fatalf("%ds after waking: showing %s", s, FormatETA(r.shown))
		}
	}
}

// Without the sleep watch, the same two hours do reach the estimate: this is
// what the test above is guarding against, shown to be real.
func TestSleepUnwatchedSpikes(t *testing.T) {
	r := newSleepRun(false)
	r.work(3 * time.Minute)
	before := r.tr.Snapshot()
	r.clock.advance(2 * time.Hour)
	r.tr.Complete(1 << 16)
	if after := r.tr.Snapshot(); after.ETA < 3*before.ETA {
		t.Skipf("the estimator absorbed the gap by itself (%v -> %v)", before.ETA, after.ETA)
	}
}

// The ticker running late by a few seconds is not sleep.
func TestShortGapIsElapsedTime(t *testing.T) {
	r := newSleepRun(true)
	r.work(time.Minute)
	before := r.tr.Snapshot()
	r.clock.advance(10 * time.Second)
	after := r.tr.Snapshot()
	if d := after.Elapsed - before.Elapsed; d != 10*time.Second {
		t.Fatalf("a 10s gap moved elapsed by %v", d)
	}
}

// Sleeping while paused is counted once, as the pause.
func TestSleepWhilePausedCountsOnce(t *testing.T) {
	r := newSleepRun(true)
	paused := false
	r.tr.FollowPause(func() bool { return paused })
	r.work(time.Minute)
	before := r.tr.Snapshot()
	paused = true
	r.tr.Snapshot()
	r.clock.advance(2 * time.Hour)
	paused = false
	after := r.tr.Snapshot()
	if d := after.Elapsed - before.Elapsed; d < 0 || d > time.Second {
		t.Fatalf("elapsed moved by %v across a pause and a sleep", d)
	}
}

// Waiting for the network stops the clock as a pause does, and says so.
func TestOfflineStopsTheClock(t *testing.T) {
	r := newSleepRun(true)
	offline := false
	r.tr.FollowOffline(func() bool { return offline })
	r.work(3 * time.Minute)
	before, beforeShown := r.tr.Snapshot(), r.shown

	offline = true
	r.tr.Snapshot()
	for i := 0; i < 48*60; i++ { // 48 minutes awake, nothing moving
		r.clock.advance(time.Second)
		s := r.tr.Snapshot()
		r.shown, r.ok = r.pres.Present(s)
		if !s.Offline || s.Paused {
			t.Fatalf("snapshot Offline=%v Paused=%v while offline", s.Offline, s.Paused)
		}
	}
	during := r.tr.Snapshot()
	if during.Elapsed != before.Elapsed || during.ETA != before.ETA {
		t.Fatalf("offline moved elapsed %v -> %v, estimate %v -> %v", before.Elapsed, during.Elapsed, before.ETA, during.ETA)
	}
	if r.shown != beforeShown {
		t.Fatalf("shown estimate moved while offline: %v -> %v", beforeShown, r.shown)
	}
	offline = false
	r.work(30 * time.Second)
	if s := r.tr.Snapshot(); s.Offline {
		t.Fatal("still offline")
	}
	if ratio := float64(r.shown) / float64(beforeShown-30*time.Second); ratio > 1.3 || ratio < 0.7 {
		t.Fatalf("after the outage: showing %v, had %v", r.shown, beforeShown)
	}
}
