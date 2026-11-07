package drime

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func fastThrottle(t *testing.T) {
	t.Helper()
	b, m, s := throttleBase, throttleMax, throttleSpread
	throttleBase, throttleMax, throttleSpread = time.Millisecond, 20*time.Millisecond, 0
	t.Cleanup(func() { throttleBase, throttleMax, throttleSpread = b, m, s })
}

// tooManyServer answers 429 to the first n requests and 200 {} after.
func tooManyServer(t *testing.T, n int64) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) <= n {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"message":"Too Many Attempts."}`))
			return
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

// A 429 is not the request's fault, so it must not use up the attempts
// meant for real failures: a request Drime turns away five times still goes
// through with only two attempts allowed.
func TestTooManyRequestsDoNotUseUpAttempts(t *testing.T) {
	fastThrottle(t)
	srv, calls := tooManyServer(t, 5)
	a := &api{base: srv.URL + "/", hc: srv.Client(), maxAttempts: 2}
	if err := a.call(context.Background(), http.MethodGet, "/x", nil, nil, nil, nil); err != nil {
		t.Fatalf("call after 5 x 429: %v", err)
	}
	if got := calls.Load(); got != 6 {
		t.Fatalf("requests = %d, want 6", got)
	}
	if got := a.thr.hits.Load(); got != 5 {
		t.Fatalf("throttle hits = %d, want 5", got)
	}
}

// A request that Drime never lets through still fails in the end.
func TestTooManyRequestsGiveUpEventually(t *testing.T) {
	fastThrottle(t)
	srv, calls := tooManyServer(t, 1<<30)
	a := &api{base: srv.URL + "/", hc: srv.Client(), maxAttempts: 2}
	err := a.call(context.Background(), http.MethodGet, "/x", nil, nil, nil, nil)
	var ae *apiError
	if !errors.As(err, &ae) || ae.Status != http.StatusTooManyRequests {
		t.Fatalf("err = %v, want a 429", err)
	}
	if got := calls.Load(); got != maxThrottled+1 {
		t.Fatalf("requests = %d, want %d", got, maxThrottled+1)
	}
}

// One 429 pauses every API request, not just the one that got it.
func TestThrottlePausesEveryRequest(t *testing.T) {
	b, m, s := throttleBase, throttleMax, throttleSpread
	throttleBase, throttleMax, throttleSpread = 80*time.Millisecond, time.Second, 0
	t.Cleanup(func() { throttleBase, throttleMax, throttleSpread = b, m, s })

	var th throttle
	th.hit(nil)
	began := time.Now()
	if err := th.wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(began); d < 60*time.Millisecond {
		t.Fatalf("waited %s after a 429, want about %s", d, throttleBase)
	}
	// Once the pause is over, nobody waits.
	began = time.Now()
	_ = th.wait(context.Background())
	if d := time.Since(began); d > 20*time.Millisecond {
		t.Fatalf("waited %s with no pause on", d)
	}
}

// Pauses grow while 429s keep coming, and a success started after the last
// one ends the streak. Successes of requests already under way do not.
func TestThrottleStreak(t *testing.T) {
	fastThrottle(t)
	var th throttle
	before := time.Now().Add(-time.Second)
	th.hit(nil)
	th.hit(nil) // same burst, during the pause: counts once
	if th.streak != 1 {
		t.Fatalf("streak = %d after one burst, want 1", th.streak)
	}
	time.Sleep(time.Until(th.until) + time.Millisecond)
	th.hit(nil)   // after the pause: the streak grows
	th.ok(before) // started before the 429s: proves nothing
	if th.streak != 2 {
		t.Fatalf("streak = %d, want 2", th.streak)
	}
	th.ok(time.Now().Add(time.Millisecond))
	if th.streak != 0 {
		t.Fatalf("streak = %d after a fresh success, want 0", th.streak)
	}
}

// Retry-After, when Drime sends it, sets the pause.
func TestThrottleHonoursRetryAfter(t *testing.T) {
	fastThrottle(t)
	var th throttle
	resp := &http.Response{Header: http.Header{"Retry-After": {"0"}}}
	th.hit(resp)
	// Retry-After 0 gives no pause at all.
	if d := time.Until(th.until); d > 5*time.Millisecond {
		t.Fatalf("pause = %s, want none", d)
	}
}

// The Client exposes its count for the engine.
func TestClientThrottles(t *testing.T) {
	f := newFake(t)
	c := newClient(t, f, nil)
	if c.Throttles() != 0 {
		t.Fatalf("Throttles = %d on a fresh client", c.Throttles())
	}
	c.api.thr.hit(nil)
	if c.Throttles() != 1 {
		t.Fatalf("Throttles = %d, want 1", c.Throttles())
	}
}
