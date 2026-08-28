package progress

import (
	"testing"
	"time"
)

// runToCheckpoints drives a simulation and captures a Snapshot the first time
// each requested percentage is reached, so one run can be examined before and
// after a rate change.
func runToCheckpoints(t *testing.T, totalBytes, totalFiles int64, events []simEvent, at []float64) (snaps []Snapshot, total time.Duration) {
	t.Helper()
	origin := time.Unix(0, 0)
	clock := newFakeClock(origin)
	tr := New(totalBytes, totalFiles, clock.now)

	snaps = make([]Snapshot, len(at))
	taken := make([]bool, len(at))

	for _, ev := range events {
		clock.advance(ev.at - clock.now().Sub(origin))
		switch ev.kind {
		case simComplete:
			tr.Complete(ev.bytes)
		case simAddBytes:
			tr.AddBytes(ev.bytes)
		}
		snap := tr.Snapshot()
		for i, pct := range at {
			if !taken[i] && snap.Percent >= pct {
				snaps[i], taken[i] = snap, true
			}
		}
	}
	for i, pct := range at {
		if !taken[i] {
			t.Fatalf("simulation never reached %.0f%%", pct)
		}
	}
	return snaps, clock.now().Sub(origin)
}

// TestETAFollowsTheNetworkDown is the case the user actually described: the
// connection genuinely slows partway through, and the estimate must follow it.
//
// The companion test in simulation_test.go deliberately places its slowdown
// early so a 25% checkpoint can see it -- asking a causal estimator to predict
// a rate change before it happens would be testing precognition. This test
// asks the opposite and harder question: when the drop lands at the literal
// halfway point, does the estimate ADAPT, or does it stay anchored to the fast
// rate it learned first?
//
// A stale estimate is exactly the failure mode being designed against: a bar
// that says two hours, keeps saying two hours while the network halves, and
// then takes four. Here the bar is allowed to have been wrong at 25%; what it
// may not do is still be wrong at 75%.
func TestETAFollowsTheNetworkDown(t *testing.T) {
	const totalBytes = int64(4) * 1024 * 1024 * 1024
	const partSize = int64(4) * 1024 * 1024
	const fastInterval = 50 * time.Millisecond // ~80 MB/s

	// Bandwidth halves at the true midpoint of the transfer.
	events := genBandwidthBound(totalBytes, partSize, fastInterval, 0, totalBytes/2)

	snaps, total := runToCheckpoints(t, totalBytes, 1, events, []float64{25, 75})
	before, after := snaps[0], snaps[1]

	if !before.ETAKnown || !after.ETAKnown {
		t.Fatalf("ETA unknown at a checkpoint (25%%: %v, 75%%: %v)", before.ETAKnown, after.ETAKnown)
	}

	// 1. The observed rate must actually have come down. Allowing a generous
	//    band because the EWMA is still catching up at 75%.
	if after.ByteRate >= before.ByteRate*0.75 {
		t.Errorf("byte rate did not fall after the slowdown: %.0f B/s at 25%% -> %.0f B/s at 75%%.\n"+
			"The estimate is anchored to a rate that no longer exists.",
			before.ByteRate, after.ByteRate)
	}

	// 2. Having seen the slowdown, the estimate must now be right about what
	//    is left. This is the promise: the number on screen reflects the rate
	//    right now.
	remaining := total - after.Elapsed
	relErr := (float64(after.ETA) - float64(remaining)) / float64(remaining)
	t.Logf("after the slowdown: elapsed=%v predicted=%v actual=%v relErr=%.1f%%",
		after.Elapsed, after.ETA, remaining, relErr*100)

	if relErr < -0.10 {
		t.Errorf("ETA under-predicted by %.1f%% after the slowdown (said %v, really %v).\n"+
			"Under-prediction is the failure that matters: the user shuts the machine down too early.",
			-relErr*100, after.ETA, remaining)
	}
	if relErr > 0.15 {
		t.Errorf("ETA over-predicted by %.1f%% after the slowdown (said %v, really %v)",
			relErr*100, after.ETA, remaining)
	}
}

// TestETANeverGoesInfiniteOrNegative guards the arithmetic itself. An Inf or
// NaN reaching the renderer would print something meaningless at exactly the
// moment the user is relying on it.
func TestETANeverGoesInfiniteOrNegative(t *testing.T) {
	origin := time.Unix(0, 0)
	clock := newFakeClock(origin)
	tr := New(1024*1024, 10, clock.now)

	for i := 0; i < 10; i++ {
		clock.advance(500 * time.Millisecond)
		tr.Complete(1024 * 1024 / 10)
		s := tr.Snapshot()
		if s.ETA < 0 {
			t.Fatalf("negative ETA at step %d: %v", i, s.ETA)
		}
		if s.ETA > 365*24*time.Hour {
			t.Fatalf("absurd ETA at step %d: %v -- almost certainly an overflow or a divide by a near-zero rate", i, s.ETA)
		}
	}
	// Past 100%: over-completion must not produce nonsense either.
	clock.advance(time.Second)
	tr.Complete(1 << 20)
	if s := tr.Snapshot(); s.ETA < 0 {
		t.Fatalf("negative ETA after over-completion: %v", s.ETA)
	}
}
