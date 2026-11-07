package progress

import (
	"fmt"
	"math"
	"math/rand/v2"
	"testing"
	"time"
)

// A parallel, Drime-shaped run: a pool of workers, a cap on how many
// requests start a second, a link shared by whatever is sending, and a round
// trip after each upload. It checks the estimate the way a person sees it:
// what the panel shows, against how long the run really had left.
type simFile struct{ size int64 }

type active struct {
	left  int64
	ready float64 // when the request finishes once bytes are sent
	rtt   float64
}

func runSim(t *testing.T, name string, files []simFile, workers int, reqPerSec, bw, rtt float64) map[float64]float64 {
	return runSimRate(t, name, files, workers, func(float64) float64 { return reqPerSec }, bw, rtt)
}

func runSimRate(t *testing.T, name string, files []simFile, workers int, rate func(float64) float64, bw, rtt float64) map[float64]float64 {
	t.Helper()
	var clock time.Time
	base := time.Unix(0, 0)
	clock = base
	var totalBytes int64
	for _, f := range files {
		totalBytes += f.size
	}
	tr := New(totalBytes, int64(len(files)), func() time.Time { return clock })
	var p Presenter
	type obs struct{ at, eta, shown float64 }
	var obsv []obs
	next := 0
	var act []*active
	tokens := 0.0
	dt := 0.05
	now := 0.0
	lastSnap := -1.0
	for next < len(files) || len(act) > 0 {
		rps := rate(now)
		tokens = math.Min(tokens+rps*dt, math.Max(rps, 1))
		for len(act) < workers && next < len(files) && tokens >= 1 {
			tokens--
			act = append(act, &active{left: files[next].size, rtt: rtt})
			next++
		}
		sending := 0
		for _, a := range act {
			if a.left > 0 {
				sending++
			}
		}
		share := 0.0
		if sending > 0 {
			share = bw * dt / float64(sending)
		}
		keep := act[:0]
		for _, a := range act {
			if a.left > 0 {
				n := int64(math.Min(float64(a.left), share))
				if n < 1 {
					n = 1
				}
				a.left -= n
				tr.AddBytes(n)
				if a.left == 0 {
					a.ready = now + a.rtt
				}
				keep = append(keep, a)
				continue
			}
			if now >= a.ready {
				tr.Complete(0)
				continue
			}
			keep = append(keep, a)
		}
		act = keep
		now += dt
		clock = base.Add(time.Duration(now * float64(time.Second)))
		if now-lastSnap >= 1 {
			lastSnap = now
			s := tr.Snapshot()
			d, ok := p.Present(s)
			if ok {
				obsv = append(obsv, obs{now, s.ETA.Seconds(), d.Seconds()})
			}
		}
	}
	total := now
	out := name + fmt.Sprintf(" total %.0fs:", total)
	ratios := map[float64]float64{}
	for _, frac := range []float64{0.05, 0.1, 0.25, 0.5, 0.75} {
		for _, o := range obsv {
			if o.at >= frac*total {
				ratios[frac] = o.shown / (total - o.at)
				out += fmt.Sprintf("  @%.0f%% shown %.2fx raw %.2fx", frac*100, ratios[frac], o.eta/(total-o.at))
				break
			}
		}
	}
	t.Log(out)
	return ratios
}

// within fails the test when what was shown at a point of the run was more
// than tol off, as a ratio, from the time really left.
func within(t *testing.T, name string, r map[float64]float64, frac, tol float64) {
	t.Helper()
	got, ok := r[frac]
	if !ok {
		t.Fatalf("%s: nothing shown by %.0f%% of the run", name, frac*100)
	}
	if got > 1+tol || got < 1/(1+tol) {
		t.Errorf("%s: at %.0f%% of the run the estimate was %.2fx the time left, want within %.0f%%", name, frac*100, got, tol*100)
	}
}

func codeFiles(n int, seed uint64, bigEvery int) []simFile {
	r := rand.New(rand.NewPCG(seed, 1))
	out := make([]simFile, n)
	for i := range out {
		sz := int64(math.Exp(r.NormFloat64()*1.5 + math.Log(4096)))
		if bigEvery > 0 && r.IntN(bigEvery) == 0 {
			sz = int64(1+r.IntN(20)) << 20
		}
		out[i] = simFile{sz}
	}
	return out
}

// A steady run is right from the moment an estimate is shown.
func TestDrimeShapedSteadyRuns(t *testing.T) {
	r := runSim(t, "tiny", codeFiles(10000, 1, 0), 32, 20, 4e6, 0.4)
	within(t, "tiny", r, 0.1, 0.1)
	within(t, "tiny", r, 0.5, 0.1)
	r = runSim(t, "code, 1% big", codeFiles(10000, 2, 100), 32, 20, 4e6, 0.4)
	within(t, "code, 1% big", r, 0.25, 0.3)
	within(t, "code, 1% big", r, 0.5, 0.15)
	r = runSim(t, "big first", append(codeFiles(200, 5, 1), codeFiles(8000, 6, 0)...), 32, 20, 4e6, 0.4)
	within(t, "big first", r, 0.5, 0.3)
}

// A run that starts slow -- Drime making folders, workers ramping up, a
// burst of "slow down" -- and then speeds up: once it has, the estimate
// follows within a few minutes instead of staying at twice the time left.
func TestDrimeShapedSlowStartThenFast(t *testing.T) {
	slow := func(until, lo, hi float64) func(float64) float64 {
		return func(n float64) float64 {
			if n < until {
				return lo
			}
			return hi
		}
	}
	r := runSimRate(t, "3 min slow", codeFiles(20000, 9, 0), 32, slow(180, 5, 25), 4e6, 0.4)
	within(t, "3 min slow", r, 0.5, 0.15)
	r = runSimRate(t, "10 min slow", codeFiles(40000, 10, 100), 32, slow(600, 8, 25), 4e6, 0.4)
	within(t, "10 min slow", r, 0.5, 0.15)
	storm := func(n float64) float64 {
		if n < 300 && math.Mod(n, 60) < 10 {
			return 0
		}
		return 25
	}
	r = runSimRate(t, "429 storms", codeFiles(20000, 11, 0), 32, storm, 4e6, 0.4)
	within(t, "429 storms", r, 0.5, 0.15)
}
