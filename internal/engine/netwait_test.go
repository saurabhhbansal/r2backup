package engine

import (
	"context"
	"errors"
	"io"
	"net"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/saurabhhbansal/r2backup/internal/netwait"
)

// downUploader fails every Put with the error a lookup gives on a machine
// with no network, for as long as down is set.
type downUploader struct {
	*fakeUploader
	down  *atomic.Bool
	tries atomic.Int64
}

func lookupFailure() error {
	return &url.Error{Op: "Post", URL: "https://app.drime.cloud/api/v1/uploads", Err: &net.OpError{
		Op: "dial", Net: "tcp",
		Err: &net.DNSError{Err: "no such host", Name: "app.drime.cloud", IsNotFound: true},
	}}
}

func (d *downUploader) Put(ctx context.Context, key string, r io.Reader, size int64, meta map[string]string, onBytes func(int64)) (string, error) {
	d.tries.Add(1)
	if d.down.Load() {
		return "", lookupFailure()
	}
	return d.fakeUploader.Put(ctx, key, r, size, meta, onBytes)
}

// A network that goes away mid-run holds the run instead of failing files;
// when it is back every file goes up and none is reported failed.
func TestRun_NetworkOutageHoldsAndCarriesOn(t *testing.T) {
	fsys := newFakeFS()
	p := setupPlanWithFiles(t, fsys, 20)
	var down atomic.Bool
	down.Store(true)
	up := &downUploader{fakeUploader: newFakeUploader(), down: &down}
	eng := New(Options{Uploader: up, FS: fsys, Workers: 4, MinWorkers: 4, MaxWorkers: 4})

	g := netwait.New(netwait.Options{
		Probe: func(context.Context) error {
			if down.Load() {
				return errors.New("unreachable")
			}
			return nil
		},
		FirstProbe: 5 * time.Millisecond, ProbeCap: 10 * time.Millisecond,
	})
	defer g.Close()
	ctx := netwait.NewContext(context.Background(), g)

	done := make(chan *Result, 1)
	go func() {
		res, err := eng.Run(ctx, p)
		if err != nil {
			t.Errorf("Run: %v", err)
		}
		done <- res
	}()

	time.Sleep(150 * time.Millisecond)
	if !g.Offline() {
		t.Fatal("the gate did not close on a lookup failure")
	}
	// Workers hold rather than spin through the plan failing files.
	if n := up.tries.Load(); n > 8 {
		t.Fatalf("%d attempts while offline; the run should be holding", n)
	}
	down.Store(false)

	select {
	case res := <-done:
		if len(res.Failed) != 0 {
			t.Fatalf("%d files failed across the outage: %v", len(res.Failed), res.Failed[0].Err)
		}
		if res.Uploaded != 20 {
			t.Fatalf("Uploaded = %d, want 20", res.Uploaded)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the run did not carry on once the network was back")
	}
}

// A network that never comes back ends the run once it has waited long
// enough, failing what is left with a message that says why.
func TestRun_NetworkGoneForGoodFailsWithNoNetwork(t *testing.T) {
	fsys := newFakeFS()
	p := setupPlanWithFiles(t, fsys, 10)
	var down atomic.Bool
	down.Store(true)
	up := &downUploader{fakeUploader: newFakeUploader(), down: &down}
	eng := New(Options{Uploader: up, FS: fsys, Workers: 2, MinWorkers: 2, MaxWorkers: 2})

	g := netwait.New(netwait.Options{
		Probe:   func(context.Context) error { return errors.New("unreachable") },
		MaxWait: 20 * time.Millisecond, FirstProbe: 5 * time.Millisecond, ProbeCap: 5 * time.Millisecond,
	})
	defer g.Close()
	ctx := netwait.NewContext(context.Background(), g)

	done := make(chan *Result, 1)
	go func() {
		res, _ := eng.Run(ctx, p)
		done <- res
	}()
	select {
	case res := <-done:
		if res.Uploaded != 0 || len(res.Failed) != 10 {
			t.Fatalf("Uploaded = %d, Failed = %d; want 0 and 10", res.Uploaded, len(res.Failed))
		}
		for _, f := range res.Failed {
			if !errors.Is(f.Err, netwait.ErrNoNetwork) {
				t.Fatalf("%s failed with %v, want ErrNoNetwork", f.Key, f.Err)
			}
		}
		// Files after the first few are failed without being tried.
		if n := up.tries.Load(); n > 4 {
			t.Fatalf("%d attempts on a network known to be gone", n)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a run with no network never ended")
	}
}

// An error from the storage itself is not an outage: the file fails as
// before, and the run does not hold.
func TestRun_StorageErrorIsNotHeld(t *testing.T) {
	fsys := newFakeFS()
	p := setupPlanWithFiles(t, fsys, 3)
	up := newFakeUploader()
	up.putErr = map[string]error{p.Uploads[0].Key: errors.New("drime: Forbidden (HTTP 403)")}
	eng := New(Options{Uploader: up, FS: fsys})

	g := netwait.New(netwait.Options{Probe: func(context.Context) error { return nil }})
	defer g.Close()
	res, err := eng.Run(netwait.NewContext(context.Background(), g), p)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Failed) != 1 || res.Uploaded != 2 {
		t.Fatalf("Failed = %d, Uploaded = %d; want 1 and 2", len(res.Failed), res.Uploaded)
	}
	if up.putCount(p.Uploads[0].Key) != 1 {
		t.Fatalf("a storage error was retried %d times", up.putCount(p.Uploads[0].Key))
	}
}
