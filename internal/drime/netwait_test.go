package drime

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/saurabhhbansal/r2backup/internal/netwait"
)

// flakyTransport fails the first n requests the way a machine with no
// network does, then answers.
type flakyTransport struct {
	n     int64
	calls atomic.Int64
}

func (f *flakyTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if f.calls.Add(1) <= f.n {
		return nil, &net.OpError{Op: "dial", Net: "tcp",
			Err: &net.DNSError{Err: "no such host", Name: r.URL.Hostname(), IsNotFound: true}}
	}
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{}`)), Header: http.Header{}, Request: r}, nil
}

// A request that fails for want of a network waits for it and tries again,
// without the failures counting against its attempts.
func TestSend_NetworkFailureWaitsWithoutSpendingAttempts(t *testing.T) {
	tr := &flakyTransport{n: 3}
	a := &api{base: "https://app.drime.cloud/", hc: &http.Client{Transport: tr}, maxAttempts: 1}
	g := netwait.New(netwait.Options{Probe: func(context.Context) error { return nil }})
	defer g.Close()
	ctx := netwait.NewContext(context.Background(), g)
	if err := a.call(ctx, http.MethodGet, "/x", nil, nil, nil, nil); err != nil {
		t.Fatalf("call = %v", err)
	}
	if n := tr.calls.Load(); n != 4 {
		t.Fatalf("%d requests, want 4", n)
	}
}

// Without a gate it behaves as it always did: one attempt, one failure.
func TestSend_NoGateNoWait(t *testing.T) {
	tr := &flakyTransport{n: 3}
	a := &api{base: "https://app.drime.cloud/", hc: &http.Client{Transport: tr}, maxAttempts: 1}
	err := a.call(context.Background(), http.MethodGet, "/x", nil, nil, nil, nil)
	if err == nil || !netwait.IsNetworkError(err) {
		t.Fatalf("call = %v, want the network error", err)
	}
}

// Once the run has given up on the network, the request fails saying so.
func TestSend_GivenUpFailsWithNoNetwork(t *testing.T) {
	tr := &flakyTransport{n: 100}
	a := &api{base: "https://app.drime.cloud/", hc: &http.Client{Transport: tr}, maxAttempts: 2}
	g := netwait.New(netwait.Options{
		Probe:   func(context.Context) error { return errors.New("down") },
		MaxWait: 1, FirstProbe: 1, ProbeCap: 1,
	})
	defer g.Close()
	err := a.call(netwait.NewContext(context.Background(), g), http.MethodGet, "/x", nil, nil, nil, nil)
	if !errors.Is(err, netwait.ErrNoNetwork) {
		t.Fatalf("call = %v, want ErrNoNetwork", err)
	}
}
