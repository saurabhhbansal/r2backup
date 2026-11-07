package drime

import (
	"context"
	"math/rand/v2"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// throttle is one pause shared by every request a Client sends to Drime's
// API. Drime caps how fast one account may call it and answers a burst past
// that with 429, mostly with no Retry-After. Backing off each request on its
// own lets the other thirty carry on into the same wall; one 429 pausing all
// of them for a moment lets the cap reset and the run go on at full speed.
//
// It also counts the 429s and 503s it sees, so the engine can send fewer
// files at once when Drime keeps asking it to slow down (see Throttles).
type throttle struct {
	mu      sync.Mutex
	until   time.Time // no API request starts before this
	streak  int       // 429s in a row with no success started after them
	lastHit time.Time
	hits    atomic.Int64
}

// Pauses are variables so tests can shorten them.
var (
	throttleBase = time.Second
	throttleMax  = 30 * time.Second
	// throttleSpread staggers requests leaving a pause, so they do not all
	// arrive at once and trip the cap again.
	throttleSpread = 300 * time.Millisecond
)

// maxThrottled is how many 429s one request may wait out before it gives up.
// They do not count against maxAttempts: a 429 says nothing is wrong with the
// request, only that it came too soon. With pauses up to throttleMax this is
// several minutes of Drime saying no before a file fails.
const maxThrottled = 16

// isThrottleStatus reports whether a status means "slow down": 429 from
// Drime, 503 from Drime or its storage when overloaded.
func isThrottleStatus(status int) bool {
	return status == http.StatusTooManyRequests || status == http.StatusServiceUnavailable
}

// wait holds until the shared pause is over, plus a little jitter if there
// was one.
func (t *throttle) wait(ctx context.Context) error {
	t.mu.Lock()
	d := time.Until(t.until)
	t.mu.Unlock()
	if d <= 0 {
		return nil
	}
	if throttleSpread > 0 {
		d += time.Duration(rand.Int64N(int64(throttleSpread)))
	}
	return sleepCtx(ctx, d)
}

// hit records a slow-down answer and pauses every API request: for as long
// as Retry-After says, or else for longer the more 429s come in a row.
func (t *throttle) hit(resp *http.Response) {
	t.hits.Add(1)
	d := time.Duration(0)
	if resp != nil {
		if s := resp.Header.Get("Retry-After"); s != "" {
			if secs, err := strconv.Atoi(s); err == nil && secs >= 0 {
				d = min(time.Duration(secs)*time.Second, throttleMax)
			}
		}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	now := time.Now()
	// Many requests in flight get the same burst of 429s at once. Only the
	// first, or one after the pause is over, lengthens the next pause.
	if !now.Before(t.until) {
		t.streak++
	}
	t.lastHit = now
	if d == 0 {
		d = min(throttleBase<<min(t.streak-1, 5), throttleMax)
	}
	if until := now.Add(d); until.After(t.until) {
		t.until = until
	}
}

// ok records a success. Only a request that started after the last 429 ends
// a streak: ones already under way when it came prove nothing.
func (t *throttle) ok(began time.Time) {
	t.mu.Lock()
	if began.After(t.lastHit) {
		t.streak = 0
	}
	t.mu.Unlock()
}

// Throttles is how many times Drime has said to slow down since the Client
// was made. The engine reads it to send fewer files at once.
func (c *Client) Throttles() int64 { return c.api.thr.hits.Load() }
