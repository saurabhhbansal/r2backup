package progress

import (
	"math"
	"math/rand"
	"sort"
	"testing"
	"time"
)

// poolFile is one file in a synthetic plan.
type poolFile struct{ size int64 }

// simulatePool runs files through a pool of workers the way the engine does:
// each worker takes the next file as soon as it is free, spends a fixed
// overhead on it (the request, the round trip) and then streams it at a
// per-worker rate, reporting a large file part by part. It returns the event
// timeline, when the last file finished, and where the tail begins: the
// pool starts to drain once the first worker runs out of files, and before
// that, the last stretch shorter than the longest single file may be that one
// file running on its own at one worker's speed.
func simulatePool(files []poolFile, workers int, overhead time.Duration, perWorker float64) ([]simEvent, time.Duration, time.Duration) {
	const partSize = 8 << 20
	free := make([]time.Duration, workers)
	var events []simEvent
	var end, drain, longest time.Duration
	for _, f := range files {
		// The worker that frees up first takes the next file.
		w := 0
		for i := range free {
			if free[i] < free[w] {
				w = i
			}
		}
		t := free[w] + overhead
		if f.size > partSize {
			for sent := int64(0); sent < f.size; {
				n := min(int64(partSize), f.size-sent)
				t += time.Duration(float64(n) / perWorker * float64(time.Second))
				sent += n
				events = append(events, simEvent{at: t, kind: simAddBytes, bytes: n})
			}
		} else {
			t += time.Duration(float64(f.size) / perWorker * float64(time.Second))
			events = append(events, simEvent{at: t, kind: simAddBytes, bytes: f.size})
		}
		events = append(events, simEvent{at: t, kind: simComplete})
		longest = max(longest, t-free[w])
		free[w] = t
		if t > end {
			end = t
		}
	}
	drain = end - longest
	for _, f := range free {
		if f < drain {
			drain = f
		}
	}
	sort.SliceStable(events, func(i, j int) bool { return events[i].at < events[j].at })
	return events, end, drain
}

func tinyFiles(r *rand.Rand, n int) []poolFile {
	out := make([]poolFile, n)
	for i := range out {
		out[i] = poolFile{size: 1<<10 + r.Int63n(63<<10)} // 1-64 KB
	}
	return out
}

func bigFiles(r *rand.Rand, n int) []poolFile {
	out := make([]poolFile, n)
	for i := range out {
		out[i] = poolFile{size: 50<<20 + r.Int63n(150<<20)} // 50-200 MB
	}
	return out
}

// estimateRun drives a Tracker through the timeline, snapshotting once a
// second as the backup does, and returns every snapshot with the true
// remaining time at that moment.
type estimatePoint struct {
	at        time.Duration
	snap      Snapshot
	remaining time.Duration
}

func estimateRun(events []simEvent, end time.Duration, files []poolFile) []estimatePoint {
	var totalBytes int64
	for _, f := range files {
		totalBytes += f.size
	}
	origin := time.Unix(0, 0)
	clock := newFakeClock(origin)
	tr := New(totalBytes, int64(len(files)), clock.now)
	var out []estimatePoint
	next := time.Second
	i := 0
	for next < end {
		for i < len(events) && events[i].at <= next {
			clock.advance(events[i].at - clock.now().Sub(origin))
			if events[i].kind == simComplete {
				tr.Complete(events[i].bytes)
			} else {
				tr.AddBytes(events[i].bytes)
			}
			i++
		}
		clock.advance(next - clock.now().Sub(origin))
		out = append(out, estimatePoint{at: next, snap: tr.Snapshot(), remaining: end - next})
		next += time.Second
	}
	return out
}

// checkEstimates is the promise, for any workload: once the estimate is shown
// (after the warm-up a person would see), it stays within a sane band of the
// real time left, and it never jumps by more than double from one second to
// the next.
//
// It stops where the tail begins. The model is aggregate cost for a busy
// pool; once there are fewer files left than workers the last few run on
// their own at one worker's speed, and that tail is the one thing it does not
// try to predict.
func checkEstimates(t *testing.T, name string, points []estimatePoint, end, drain time.Duration) {
	t.Helper()
	var worstLow, worstHigh = 1.0, 1.0
	var prev time.Duration
	for _, p := range points {
		s := p.snap
		// The warm-up the panel uses before it shows anything.
		if !s.ETAKnown || p.at < 30*time.Second || (s.Percent < 2 && float64(s.FilesDone) < 0.02*float64(s.FilesTotal)) {
			continue
		}
		// The last stretch is a few seconds of a few files: ratios of tiny
		// numbers say nothing about the estimator.
		if p.at >= drain || p.remaining < 20*time.Second {
			break
		}
		ratio := float64(s.ETA) / float64(p.remaining)
		worstLow, worstHigh = math.Min(worstLow, ratio), math.Max(worstHigh, ratio)
		if ratio < 1.0/3 || ratio > 3 {
			t.Errorf("[%s] at %v: estimate %v, really %v (x%.2f) -- outside the sane band",
				name, p.at, s.ETA.Round(time.Second), p.remaining, ratio)
		}
		if prev > 0 {
			jump := float64(s.ETA) / float64(prev)
			if jump > 2 || jump < 0.5 {
				t.Errorf("[%s] at %v: estimate jumped from %v to %v in one second", name, p.at, prev, s.ETA)
			}
		}
		prev = s.ETA
	}
	t.Logf("[%s] run %v (drains from %v); estimate stayed between x%.2f and x%.2f of the truth", name, end.Round(time.Second), drain.Round(time.Second), worstLow, worstHigh)
}

func TestEstimateTinyFilesThenBigFiles(t *testing.T) {
	r := rand.New(rand.NewSource(1))
	files := append(tinyFiles(r, 40000), bigFiles(r, 48)...)
	events, end, drain := simulatePool(files, 16, 60*time.Millisecond, 2<<20)
	checkEstimates(t, "tiny then big", estimateRun(events, end, files), end, drain)
}

func TestEstimateBigFilesThenTinyFiles(t *testing.T) {
	r := rand.New(rand.NewSource(2))
	files := append(bigFiles(r, 48), tinyFiles(r, 40000)...)
	events, end, drain := simulatePool(files, 16, 60*time.Millisecond, 2<<20)
	checkEstimates(t, "big then tiny", estimateRun(events, end, files), end, drain)
}

func TestEstimateSteadyMix(t *testing.T) {
	r := rand.New(rand.NewSource(3))
	files := append(tinyFiles(r, 40000), bigFiles(r, 48)...)
	r.Shuffle(len(files), func(i, j int) { files[i], files[j] = files[j], files[i] })
	events, end, drain := simulatePool(files, 16, 60*time.Millisecond, 2<<20)
	checkEstimates(t, "steady mix", estimateRun(events, end, files), end, drain)
}

// TestEstimateStandsStillWhilePaused: a paused run's estimate is the one it
// had when it was paused, however long the pause, and elapsed time stops.
func TestEstimateStandsStillWhilePaused(t *testing.T) {
	clock := newFakeClock(time.Unix(0, 0))
	tr := New(100<<20, 1000, clock.now)
	paused := false
	tr.FollowPause(func() bool { return paused })
	for i := 0; i < 200; i++ {
		tr.AddBytes(50 << 10)
		tr.Complete(0)
		clock.advance(200 * time.Millisecond)
	}
	before := tr.Snapshot()
	if !before.ETAKnown {
		t.Fatal("no estimate after 40s")
	}
	paused = true
	tr.Snapshot()
	clock.advance(2 * time.Hour)
	during := tr.Snapshot()
	if !during.Paused {
		t.Error("snapshot does not say paused")
	}
	if during.Elapsed != before.Elapsed {
		t.Errorf("elapsed moved while paused: %v -> %v", before.Elapsed, during.Elapsed)
	}
	if during.ETA != before.ETA {
		t.Errorf("estimate moved while paused: %v -> %v", before.ETA, during.ETA)
	}
	paused = false
	for i := 0; i < 20; i++ {
		tr.AddBytes(50 << 10)
		tr.Complete(0)
		clock.advance(200 * time.Millisecond)
	}
	after := tr.Snapshot()
	if after.Paused {
		t.Error("still paused after resuming")
	}
	if r := float64(after.ETA) / float64(before.ETA); r > 1.2 || r < 0.6 {
		t.Errorf("the pause leaked into the estimate: %v before, %v after resuming at the same speed", before.ETA, after.ETA)
	}
}
