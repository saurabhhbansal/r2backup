package engine

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/saurabhhbansal/r2backup/internal/pause"
)

// testGate is a pause.Gate a test opens and closes by hand.
type testGate struct {
	mu     sync.Mutex
	paused bool
	open   chan struct{}
}

func newTestGate(paused bool) *testGate {
	g := &testGate{paused: paused, open: make(chan struct{})}
	if !paused {
		close(g.open)
	}
	return g
}

func (g *testGate) Paused() bool { g.mu.Lock(); defer g.mu.Unlock(); return g.paused }

func (g *testGate) resume() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.paused {
		g.paused = false
		close(g.open)
	}
}

func (g *testGate) Wait(ctx context.Context) error {
	g.mu.Lock()
	ch := g.open
	g.mu.Unlock()
	select {
	case <-ch:
		return ctx.Err()
	case <-ctx.Done():
		return ctx.Err()
	}
}

// A paused run starts nothing new, and carries on to the end once resumed.
func TestRun_PausedRunHoldsBetweenFilesAndCarriesOn(t *testing.T) {
	fsys := newFakeFS()
	p := setupPlanWithFiles(t, fsys, 12)
	up := newFakeUploader()
	eng := New(Options{Uploader: up, FS: fsys, Workers: 4, MinWorkers: 4, MaxWorkers: 4})

	g := newTestGate(true)
	ctx := pause.NewContext(context.Background(), g)
	done := make(chan *Result, 1)
	go func() {
		res, err := eng.Run(ctx, p)
		if err != nil {
			t.Errorf("Run: %v", err)
		}
		done <- res
	}()

	time.Sleep(100 * time.Millisecond)
	if n := len(up.callSequence()); n != 0 {
		t.Fatalf("%d uploads started while paused", n)
	}
	g.resume()
	select {
	case res := <-done:
		if res.Uploaded != 12 {
			t.Fatalf("Uploaded = %d after resuming, want 12", res.Uploaded)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the run did not carry on after resuming")
	}
}

// Stopping a paused run ends it promptly, with nothing reported as done.
func TestRun_CancellingAPausedRunEndsIt(t *testing.T) {
	fsys := newFakeFS()
	p := setupPlanWithFiles(t, fsys, 5)
	up := newFakeUploader()
	eng := New(Options{Uploader: up, FS: fsys})

	ctx, cancel := context.WithCancel(pause.NewContext(context.Background(), newTestGate(true)))
	done := make(chan *Result, 1)
	go func() {
		res, _ := eng.Run(ctx, p)
		done <- res
	}()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case res := <-done:
		if res.Uploaded != 0 {
			t.Fatalf("Uploaded = %d from a run that never resumed", res.Uploaded)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a paused run did not end when cancelled")
	}
}
