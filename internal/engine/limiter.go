package engine

import "context"

// limiter is an adjustable concurrency gate: at most `limit` acquires may be
// outstanding at once, and `limit` can change while goroutines are waiting on
// it. A plain buffered channel can't do the second part -- its capacity is
// fixed at creation -- so this is a small hand-rolled semaphore instead.
//
// Workers are not grown or shut down as concurrency adapts; a fixed pool of
// goroutines (sized to MaxWorkers) all contend for this gate, and raising or
// lowering `limit` is what actually changes how many run at once. That keeps
// adaptive concurrency to one mutex-protected integer instead of spinning
// goroutines up and down under contention.
type limiter struct {
	mu     chan struct{} // 1-buffered mutex-substitute; see lock/unlock
	cond   chan struct{} // closed and replaced on every state change, to wake waiters
	limit  int
	active int
	min    int
	max    int
}

func newLimiter(initial, min, max int) *limiter {
	l := &limiter{
		mu:    make(chan struct{}, 1),
		cond:  make(chan struct{}),
		limit: initial,
		min:   min,
		max:   max,
	}
	l.mu <- struct{}{}
	return l
}

func (l *limiter) lock()   { <-l.mu }
func (l *limiter) unlock() { l.mu <- struct{}{} }

// wake releases every goroutine currently parked in acquire. Must be called
// with the lock held.
func (l *limiter) wakeLocked() {
	close(l.cond)
	l.cond = make(chan struct{})
}

// acquire blocks until a slot is free or ctx is done, returning false in the
// latter case. Callers always pass the run's own context, so a cancelled run
// wakes every waiter promptly instead of leaving it parked.
func (l *limiter) acquire(ctx context.Context) bool {
	for {
		l.lock()
		if ctx.Err() != nil {
			l.unlock()
			return false
		}
		if l.active < l.limit {
			l.active++
			l.unlock()
			return true
		}
		wait := l.cond
		l.unlock()
		select {
		case <-wait:
		case <-ctx.Done():
			return false
		}
	}
}

func (l *limiter) release() {
	l.lock()
	l.active--
	l.wakeLocked()
	l.unlock()
}

// setLimit changes the number of permitted concurrent acquires, clamped to
// [min, max]. Waiters are woken so a raised limit takes effect immediately
// rather than waiting for the next release.
func (l *limiter) setLimit(n int) {
	if n < l.min {
		n = l.min
	}
	if n > l.max {
		n = l.max
	}
	l.lock()
	l.limit = n
	l.wakeLocked()
	l.unlock()
}

func (l *limiter) currentLimit() int {
	l.lock()
	defer l.unlock()
	return l.limit
}
