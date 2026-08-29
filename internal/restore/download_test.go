package restore

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/saurabhhbansal/r2backup/internal/remote"
)

func TestClassify(t *testing.T) {
	cases := []struct {
		name string
		meta remote.Metadata
		want itemKind
	}{
		{"symlink wins regardless of size", remote.Metadata{Symlink: "x", Size: 100}, kindSymlink},
		{"zero size with owner-execute is a directory marker", remote.Metadata{Size: 0, Mode: 0o755}, kindEmptyDir},
		{"zero size without execute is a file", remote.Metadata{Size: 0, Mode: 0o644}, kindFile},
		{"non-zero size is always a file", remote.Metadata{Size: 5, Mode: 0o755}, kindFile},
	}
	for _, tc := range cases {
		if got := classify(tc.meta); got != tc.want {
			t.Errorf("%s: classify(%+v) = %v, want %v", tc.name, tc.meta, got, tc.want)
		}
	}
}

func TestModeAndModTimeArePreservedWithinTolerance(t *testing.T) {
	backend := newFakeBackend()
	mtime := time.Date(2023, 5, 6, 7, 8, 9, 0, time.UTC)
	backend.put("proj/current/a.txt", []byte("content"), remote.Metadata{ModTime: mtime, Mode: 0o640})

	target := t.TempDir()
	if _, err := Run(context.Background(), Options{
		Set: testSet("proj", target), Client: backend, Target: target,
	}); err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(filepath.Join(target, "a.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o640 {
		t.Errorf("mode = %v, want 0640", info.Mode().Perm())
	}
	if d := info.ModTime().Sub(mtime); d > 2*time.Second || d < -2*time.Second {
		t.Errorf("mtime differs by %s, want within 2s", d)
	}
}

func TestVerifyPassesOnACleanRestore(t *testing.T) {
	backend := newFakeBackend()
	backend.put("proj/current/a.txt", []byte("verify me please"), fileMeta())

	target := t.TempDir()
	rep, err := Run(context.Background(), Options{
		Set: testSet("proj", target), Client: backend, Target: target, Verify: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Succeeded() {
		t.Fatalf("Failures: %v, VerifyMismatches: %v", rep.Failures, rep.VerifyMismatches)
	}
	if rep.Verified != 1 {
		t.Errorf("Verified = %d, want 1", rep.Verified)
	}
}

func TestAPerObjectFailureDoesNotStopTheRun(t *testing.T) {
	backend := newFakeBackend()
	backend.put("proj/current/good-a.txt", []byte("a"), fileMeta())
	backend.put("proj/current/good-b.txt", []byte("b"), fileMeta())
	// No object exists at this key; List will still return it if we seed
	// it via a raw map entry that Get can't satisfy. Simulate a
	// disappeared object by removing it from the backend after listing
	// would have seen it -- easiest done by pointing Only at a key that
	// buildPlan's safety check rejects instead, which is a genuine,
	// deterministic per-object failure path.
	backend.put("proj/current/../escape.txt", []byte("nope"), fileMeta())

	target := t.TempDir()
	rep, err := Run(context.Background(), Options{
		Set: testSet("proj", target), Client: backend, Target: target,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.Downloaded != 2 {
		t.Errorf("Downloaded = %d, want 2 -- the two good files must still land", rep.Downloaded)
	}
	if len(rep.Failures) != 1 {
		t.Errorf("Failures = %v, want exactly one", rep.Failures)
	}
	if rep.Succeeded() {
		t.Error("Succeeded() should be false when a failure was recorded")
	}
}

// TestContextCancellationStopsPromptly proves a run does not wait for every
// in-flight download to finish once its context is cancelled: every Get
// call here would hang forever on its own, so a slow return means workers
// are not respecting ctx.
func TestContextCancellationStopsPromptly(t *testing.T) {
	backend := newFakeBackend()
	backend.blockGet = make(chan struct{}) // never closed: every Get hangs until ctx is done
	for i := 0; i < 50; i++ {
		backend.put(fmt.Sprintf("proj/current/file%02d.txt", i), []byte("x"), fileMeta())
	}

	target := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		defer close(done)
		Run(ctx, Options{Set: testSet("proj", target), Client: backend, Target: target})
	}()

	// Give the pool a moment to actually start downloads and block on
	// blockGet, then cancel mid-flight.
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return promptly after context cancellation")
	}
}

// TestNoTargetAndCancelledContextStillReturnsPromptly guards the other
// order: a context already cancelled before Run does anything at all.
func TestAlreadyCancelledContextReturnsPromptly(t *testing.T) {
	backend := newFakeBackend()
	backend.put("proj/current/a.txt", []byte("a"), fileMeta())
	target := t.TempDir()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		Run(ctx, Options{Set: testSet("proj", target), Client: backend, Target: target})
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return promptly for an already-cancelled context")
	}
}
