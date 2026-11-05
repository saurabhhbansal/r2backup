package netwait

import (
	"context"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeClock is a clock the gate's sleeps advance, so a test can wait half an
// hour in a moment.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *fakeClock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}
func (c *fakeClock) sleep(ctx context.Context, d time.Duration) error {
	c.add(d)
	time.Sleep(time.Millisecond) // let the test look in between
	return ctx.Err()
}

func TestGate_OnlineWaitReturnsAtOnce(t *testing.T) {
	g := New(Options{})
	defer g.Close()
	if g.Offline() {
		t.Fatal("a new gate is offline")
	}
	if err := g.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestGate_HoldsUntilTheProbeSucceeds(t *testing.T) {
	var up atomic.Bool
	var probes atomic.Int64
	clk := &fakeClock{t: time.Unix(0, 0)}
	g := New(Options{
		Probe: func(context.Context) error {
			probes.Add(1)
			if up.Load() {
				return nil
			}
			return errors.New("unreachable")
		},
		Now: clk.now, Sleep: clk.sleep,
	})
	defer g.Close()

	g.Trip()
	if !g.Offline() {
		t.Fatal("tripped gate is not offline")
	}
	g.Trip() // a second trip starts no second prober
	res := make(chan error, 1)
	go func() { res <- g.Wait(context.Background()) }()
	time.Sleep(30 * time.Millisecond)
	select {
	case err := <-res:
		t.Fatalf("Wait returned %v while the network was down", err)
	default:
	}
	up.Store(true)
	select {
	case err := <-res:
		if err != nil {
			t.Fatalf("Wait = %v once the network was back", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Wait did not return once the network was back")
	}
	if g.Offline() {
		t.Fatal("gate still offline")
	}
}

// Backoff: 0, 2s, 4s, ... capped at 30s.
func TestGate_ProbeBackoff(t *testing.T) {
	clk := &fakeClock{t: time.Unix(0, 0)}
	var mu sync.Mutex
	var at []time.Duration
	start := clk.now()
	stop := make(chan struct{})
	g := New(Options{
		Probe: func(context.Context) error {
			mu.Lock()
			defer mu.Unlock()
			at = append(at, clk.now().Sub(start))
			if len(at) == 8 {
				close(stop)
			}
			if len(at) >= 8 {
				return nil
			}
			return errors.New("unreachable")
		},
		Now: clk.now, Sleep: clk.sleep,
	})
	defer g.Close()
	g.Trip()
	<-stop
	mu.Lock()
	defer mu.Unlock()
	want := []time.Duration{0, 2, 6, 14, 30, 60, 90, 120}
	for i, w := range want {
		if at[i] != w*time.Second {
			t.Fatalf("probe times %v, want %v (seconds)", at, want)
		}
	}
}

// Past MaxWait the gate stops holding, and says so; the network coming back
// later opens it again.
func TestGate_GivesUpAfterMaxWaitAndRecovers(t *testing.T) {
	var up atomic.Bool
	clk := &fakeClock{t: time.Unix(0, 0)}
	g := New(Options{
		Probe: func(context.Context) error {
			if up.Load() {
				return nil
			}
			return errors.New("unreachable")
		},
		MaxWait: 10 * time.Minute, Now: clk.now, Sleep: clk.sleep,
	})
	defer g.Close()
	g.Trip()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := g.Wait(ctx); !errors.Is(err, ErrNoNetwork) {
		t.Fatalf("Wait = %v, want ErrNoNetwork", err)
	}
	if elapsed := clk.now().Sub(time.Unix(0, 0)); elapsed < 10*time.Minute || elapsed > 12*time.Minute {
		t.Fatalf("gave up after %v, want about 10m", elapsed)
	}
	// Still given up: waiting returns at once.
	if err := g.Wait(ctx); !errors.Is(err, ErrNoNetwork) {
		t.Fatalf("second Wait = %v", err)
	}
	up.Store(true)
	deadline := time.Now().Add(5 * time.Second)
	for g.Offline() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if err := g.Wait(ctx); err != nil {
		t.Fatalf("Wait = %v after the network came back", err)
	}
}

// A computer asleep through a wait does not use up MaxWait: two hours on
// the clock between looks count as one wait, not two hours.
func TestGate_SleepDoesNotCountTowardMaxWait(t *testing.T) {
	clk := &fakeClock{t: time.Unix(0, 0)}
	var probes atomic.Int64
	var up atomic.Bool
	g := New(Options{
		Probe: func(context.Context) error {
			if probes.Add(1) == 3 {
				clk.add(2 * time.Hour) // the lid closed during this look
			}
			if up.Load() {
				return nil
			}
			return errors.New("unreachable")
		},
		MaxWait: 30 * time.Minute, Now: clk.now, Sleep: clk.sleep,
	})
	defer g.Close()
	g.Trip()
	for probes.Load() < 6 {
		time.Sleep(time.Millisecond)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := g.Wait(ctx); errors.Is(err, ErrNoNetwork) {
		t.Fatal("two hours asleep counted as waiting")
	}
	up.Store(true)
}

func TestGate_WaitEndsWithContext(t *testing.T) {
	g := New(Options{Probe: func(context.Context) error { return errors.New("down") }})
	defer g.Close()
	g.Trip()
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(20 * time.Millisecond); cancel() }()
	if err := g.Wait(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Wait = %v, want context.Canceled", err)
	}
}

func TestHold(t *testing.T) {
	ctx := context.Background()
	netErr := &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("x")}
	if Hold(ctx, netErr) == nil {
		t.Fatal("held without a gate")
	}
	g := New(Options{Probe: func(context.Context) error { return nil }})
	defer g.Close()
	ctx = NewContext(ctx, g)
	if Hold(ctx, errors.New("HTTP 500")) == nil {
		t.Fatal("held for an HTTP error")
	}
	if err := Hold(ctx, netErr); err != nil {
		t.Fatalf("Hold = %v with the network up", err)
	}
}

func TestForHosts(t *testing.T) {
	if ForHosts("", "not a url") != nil {
		t.Fatal("a gate with nothing to look at")
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	g := ForHosts("http://" + l.Addr().String() + "/api/")
	defer g.Close()
	if err := g.probe(context.Background()); err != nil {
		t.Fatalf("probe of a listening host: %v", err)
	}
	if got := probeAddr("https://app.drime.cloud/"); got != "app.drime.cloud:443" {
		t.Fatalf("probeAddr = %q", got)
	}
}
