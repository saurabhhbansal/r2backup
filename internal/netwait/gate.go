package netwait

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"
)

const (
	// MaxWait is how long one outage may hold a run, counting only time the
	// computer was awake: a laptop asleep for the night has not been
	// waiting. Past it the run stops waiting and the files still to go fail
	// with ErrNoNetwork, to be tried again next run. The prober keeps
	// looking, and if the network does come back the run goes on normally.
	MaxWait = 2 * time.Hour

	// FirstProbe is the delay before the second look; each look after that
	// waits twice as long, up to ProbeCap. The first look is immediate: an
	// error that was one connection's bad luck, with the network fine,
	// costs the run a few milliseconds.
	FirstProbe = 2 * time.Second
	ProbeCap   = 30 * time.Second

	// ProbeTimeout bounds one look: a DNS lookup and a TCP connection to the
	// storage's host.
	ProbeTimeout = 5 * time.Second
)

// Gate is one run's view of whether the network is there. The zero value is
// not usable; see New.
type Gate struct {
	probe   func(ctx context.Context) error
	maxWait time.Duration
	first   time.Duration
	cap     time.Duration
	now     func() time.Time
	sleep   func(ctx context.Context, d time.Duration) error

	mu      sync.Mutex
	offline bool
	gaveUp  bool
	back    chan struct{} // closed when the network is back; replaced on each outage
	given   chan struct{} // closed when the run stops waiting; replaced on each outage
	stop    chan struct{}
	stopped bool
	probing bool
}

// Options tunes a Gate. Zero values are the package defaults; tests shorten
// them.
type Options struct {
	// Probe reports whether the storage can be reached: nil when it can.
	Probe func(ctx context.Context) error
	// MaxWait, FirstProbe and ProbeCap override the constants of the same
	// names.
	MaxWait    time.Duration
	FirstProbe time.Duration
	ProbeCap   time.Duration
	// Now and Sleep stand in for the clock.
	Now   func() time.Time
	Sleep func(ctx context.Context, d time.Duration) error
}

// New builds a gate that looks for the network with opts.Probe.
func New(opts Options) *Gate {
	g := &Gate{
		probe: opts.Probe, maxWait: opts.MaxWait, first: opts.FirstProbe, cap: opts.ProbeCap,
		now: opts.Now, sleep: opts.Sleep,
		back: make(chan struct{}), given: make(chan struct{}), stop: make(chan struct{}),
	}
	close(g.back) // online
	if g.probe == nil {
		g.probe = func(context.Context) error { return nil }
	}
	if g.maxWait <= 0 {
		g.maxWait = MaxWait
	}
	if g.first <= 0 {
		g.first = FirstProbe
	}
	if g.cap <= 0 {
		g.cap = ProbeCap
	}
	if g.now == nil {
		g.now = time.Now
	}
	if g.sleep == nil {
		g.sleep = sleepCtx
	}
	return g
}

// ForHosts builds a gate whose probe connects to every one of urls' hosts
// (through the proxy the environment names for it, if any).
func ForHosts(urls ...string) *Gate {
	var addrs []string
	seen := map[string]bool{}
	for _, raw := range urls {
		a := probeAddr(raw)
		if a != "" && !seen[a] {
			seen[a] = true
			addrs = append(addrs, a)
		}
	}
	if len(addrs) == 0 {
		return nil
	}
	return New(Options{Probe: func(ctx context.Context) error { return dialAll(ctx, addrs) }})
}

// probeAddr is the host:port a look at raw connects to.
func probeAddr(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return ""
	}
	if req, err := http.NewRequest(http.MethodGet, raw, nil); err == nil {
		if p, err := http.ProxyFromEnvironment(req); err == nil && p != nil {
			u = p
		}
	}
	port := u.Port()
	if port == "" {
		port = "443"
		if u.Scheme == "http" {
			port = "80"
		}
	}
	return net.JoinHostPort(u.Hostname(), port)
}

func dialAll(ctx context.Context, addrs []string) error {
	for _, a := range addrs {
		ctx, cancel := context.WithTimeout(ctx, ProbeTimeout)
		c, err := (&net.Dialer{}).DialContext(ctx, "tcp", a)
		cancel()
		if err != nil {
			return err
		}
		c.Close()
	}
	return nil
}

// Offline reports whether the run is waiting for the network.
func (g *Gate) Offline() bool {
	if g == nil {
		return false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.offline
}

// Trip says a request has just failed for want of a network. The gate closes
// and a prober starts looking for the network; it reopens when the network is
// found. Tripping a gate that is already closed does nothing more.
func (g *Gate) Trip() {
	if g == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.stopped || g.offline {
		return
	}
	g.offline, g.gaveUp = true, false
	g.back = make(chan struct{})
	g.given = make(chan struct{})
	if !g.probing {
		g.probing = true
		go g.watch()
	}
}

// Wait returns nil once the network is there, at once if it already is. It
// returns ErrNoNetwork if the run has stopped waiting for it, and ctx's error
// if ctx ends first.
func (g *Gate) Wait(ctx context.Context) error {
	if g == nil {
		return ctx.Err()
	}
	g.mu.Lock()
	back, given, offline, gaveUp, stop := g.back, g.given, g.offline, g.gaveUp, g.stop
	g.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if !offline {
		return nil
	}
	if gaveUp {
		return ErrNoNetwork
	}
	select {
	case <-back:
		return ctx.Err()
	case <-given:
		if !g.Offline() {
			return ctx.Err()
		}
		return ErrNoNetwork
	case <-stop:
		if g.Offline() {
			return ErrNoNetwork
		}
		return ctx.Err()
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Close stops the prober. Anything still waiting is told there is no
// network.
func (g *Gate) Close() {
	if g == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.stopped {
		g.stopped = true
		close(g.stop)
	}
}

// watch looks for the network until it is back, or the gate is closed.
//
// The time an outage has held the run is the sum of the waits between looks
// and the looks themselves, each capped at what it should have taken. A
// computer that slept through one of them does not count the sleep: the
// clock says hours went by, but nobody was waiting.
func (g *Gate) watch() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		select {
		case <-g.stop:
			cancel()
		case <-ctx.Done():
		}
	}()

	var delay, waited time.Duration
	for {
		began := g.now()
		if delay > 0 {
			if g.sleep(ctx, delay) != nil {
				g.finishProbing()
				return
			}
		}
		pctx, pcancel := context.WithTimeout(ctx, ProbeTimeout+time.Second)
		err := g.probe(pctx)
		pcancel()
		if ctx.Err() != nil {
			g.finishProbing()
			return
		}
		spent := g.now().Sub(began)
		if most := delay + ProbeTimeout + 2*time.Second; spent > most || spent < 0 {
			spent = most
		}
		if err == nil {
			g.mu.Lock()
			g.offline, g.gaveUp = false, false
			close(g.back)
			g.probing = false
			g.mu.Unlock()
			return
		}
		waited += spent
		if waited >= g.maxWait {
			g.mu.Lock()
			if !g.gaveUp {
				g.gaveUp = true
				close(g.given)
			}
			g.mu.Unlock()
		}
		switch {
		case delay == 0:
			delay = g.first
		default:
			delay *= 2
		}
		if delay > g.cap {
			delay = g.cap
		}
	}
}

func (g *Gate) finishProbing() {
	g.mu.Lock()
	g.probing = false
	g.mu.Unlock()
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

type ctxKey struct{}

// NewContext carries g to everything a run calls.
func NewContext(ctx context.Context, g *Gate) context.Context {
	if g == nil {
		return ctx
	}
	return context.WithValue(ctx, ctxKey{}, g)
}

// FromContext returns the gate ctx carries, or nil.
func FromContext(ctx context.Context) *Gate {
	g, _ := ctx.Value(ctxKey{}).(*Gate)
	return g
}

// Ensure gives ctx a gate that looks for urls' hosts, unless it carries one
// already. The function returned stops the gate's prober; call it when the
// run is over. With no usable url ctx is returned as it is.
func Ensure(ctx context.Context, urls ...string) (context.Context, func()) {
	if FromContext(ctx) != nil {
		return ctx, func() {}
	}
	g := ForHosts(urls...)
	if g == nil {
		return ctx, func() {}
	}
	return NewContext(ctx, g), g.Close
}

// Hold is for a request that has just failed with err. If err is a network
// failure and ctx carries a gate, it trips the gate and waits for the network
// to come back: nil means try again. Otherwise -- a different kind of error,
// no gate, the run given up on or ended -- it returns a non-nil error, and the
// request should fail as it would have.
func Hold(ctx context.Context, err error) error {
	g := FromContext(ctx)
	if g == nil || !IsNetworkError(err) || ctx.Err() != nil {
		return errNotHeld
	}
	g.Trip()
	return g.Wait(ctx)
}

var errNotHeld = errors.New("netwait: not held")
