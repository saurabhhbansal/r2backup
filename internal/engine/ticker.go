package engine

import (
	"context"
	"sync/atomic"
	"time"
)

// Ticker abstracts the periodic wakeups that drive adaptive concurrency, so
// tests can advance the adaptive-concurrency loop deterministically instead
// of sleeping for real wall-clock seconds.
type Ticker interface {
	// Wait blocks until the next tick, or returns false if ctx ends first.
	Wait(ctx context.Context) bool
}

// intervalTicker is the default Ticker: a fixed real-time interval.
type intervalTicker struct {
	d time.Duration
}

func (t *intervalTicker) Wait(ctx context.Context) bool {
	timer := time.NewTimer(t.d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// ManualTicker is a Ticker a test drives by hand: each call to Tick releases
// exactly one pending Wait, so a test can force an adaptive-concurrency
// window to close at an exact, repeatable point instead of racing a timer.
type ManualTicker struct {
	c chan struct{}
}

// NewManualTicker returns a Ticker with no scheduled ticks; call Tick to fire
// one.
func NewManualTicker() *ManualTicker {
	return &ManualTicker{c: make(chan struct{})}
}

func (m *ManualTicker) Wait(ctx context.Context) bool {
	select {
	case <-m.c:
		return true
	case <-ctx.Done():
		return false
	}
}

// Tick fires one tick, blocking until a Wait call consumes it or ctx ends.
// Tests should call it from a goroutine, or only once a Wait is known to be
// pending, to avoid blocking forever against a governor that already exited.
func (m *ManualTicker) Tick(ctx context.Context) bool {
	select {
	case m.c <- struct{}{}:
		return true
	case <-ctx.Done():
		return false
	}
}

// adjustLimit is the whole adaptive-concurrency policy, expressed as a pure
// function so it can be unit tested exactly rather than through goroutines,
// timers, and a fake network.
//
// Throttling wins over everything else and is not gradual: a 429/503 means
// the far end is actively rejecting work right now, and a policy that eased
// off slowly would keep hammering it for several more windows. Absent that,
// concurrency only climbs while throughput is still improving -- once adding
// workers stops buying more bytes/window, the bottleneck has moved off the
// worker count (to the disk, the link, or the far end) and adding more just
// adds contention. bytesPrevWindow is meaningless on the very first window
// (there is nothing to compare against), so firstWindow suppresses growth
// until a real baseline exists.
func adjustLimit(current int, bytesThisWindow, bytesPrevWindow int64, throttled, firstWindow bool, min, max int) int {
	switch {
	case throttled:
		next := current / 2
		if next < min {
			next = min
		}
		return next
	case !firstWindow && bytesThisWindow > bytesPrevWindow:
		next := current + current/4
		if next <= current { // current/4 rounds to 0 below 4 workers
			next = current + 1
		}
		if next > max {
			next = max
		}
		return next
	default:
		return current
	}
}

// governor runs the adaptive-concurrency loop until ctx ends. It is a method
// on runner only so it can read opts; all decision logic lives in
// adjustLimit above.
func (r *runner) governor(ctx context.Context, lim *limiter, ticker Ticker) {
	var prevBytes int64
	first := true
	for ticker.Wait(ctx) {
		bytes := atomic.SwapInt64(&r.windowBytes, 0)
		throttled := atomic.SwapInt64(&r.throttleHits, 0) > 0
		next := adjustLimit(lim.currentLimit(), bytes, prevBytes, throttled, first, r.opts.MinWorkers, r.opts.MaxWorkers)
		lim.setLimit(next)
		prevBytes = bytes
		first = false
	}
}
