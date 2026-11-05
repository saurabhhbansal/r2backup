package progress

import "math"

// The remaining-time model.
//
// A backup's time goes on two things: a cost for every file (a request, a
// round trip, an index write) and a cost for every byte (the link). Which one
// dominates changes as the run moves through the folder -- thousands of tiny
// source files, then a few large archives -- and an estimator that only knows
// "bytes per second" and "files per second" has to guess which of the two to
// believe. The one this replaces took the more pessimistic of the two, over
// short windows, and on exactly that kind of folder it said sixty hours, then
// thirty, then one.
//
// So the model is the physics, stated once:
//
//	time left = filesLeft × perFile + bytesLeft × perByte
//
// with perFile and perByte learned from the run itself. Work is measured as a
// fraction of the plan (files done ÷ files planned, bytes done ÷ bytes
// planned), so the two coefficients are "seconds for all the files' overhead"
// and "seconds for all the bytes" and are on the same scale.
//
// They are fitted by least squares over the run's history, cut into slices of
// about a second: each slice says "this much time went on this many files and
// this many bytes". The workers run in parallel, so these are aggregate costs
// for the whole pool, not per-file durations -- which is exactly what the
// remaining work will be done by.
//
// The fit decides only the split between the two costs. How much time the
// work actually takes is then calibrated against the clock: the model is
// scaled so it accounts for all of the elapsed time, stalls included. That
// keeps the estimate honest when the split is uncertain (a run so far made of
// one kind of file says little about the other) and makes a stall raise the
// estimate the way it should.
//
// Two fits are kept: one over the whole run, which is steady, and one over the
// recent past (about two minutes on a long run, shorter on a short one), which
// sees a real change of speed. They are averaged, except that when the recent
// past says things have got slower it is believed sooner -- running out of
// time is worse than finishing early.

// estimator is guarded by the Tracker's mutex. Times are seconds of active
// (unpaused) time since the run started.
type estimator struct {
	totalFiles, totalBytes float64

	// The slice being filled.
	openStart float64
	openFiles float64
	openBytes float64

	doneFiles float64
	doneBytes float64
	samples   []sample
	cum       sums
}

type sample struct {
	end          float64 // when the slice closed
	files, bytes float64 // finished in the slice
	dt           float64 // how long the slice took
}

// sums are what the fit needs, for any per-file cost k, in closed form.
type sums struct {
	w, ff, bb, fb, ft, bt, tt, f, b, t float64
}

func (s *sums) add(f, b, dt, w float64) {
	s.w += w
	s.ff += w * f * f
	s.bb += w * b * b
	s.fb += w * f * b
	s.ft += w * f * dt
	s.bt += w * b * dt
	s.tt += w * dt * dt
	s.f += w * f
	s.b += w * b
	s.t += w * dt
}

const (
	// sliceSeconds is how much time a sample covers at least. A slice with
	// no progress at all is never closed: its time is carried into the next
	// slice that does make progress, so a gap between completions is
	// charged to the work that ended it rather than lost.
	sliceSeconds = 4.0
	// recentMin and recentMax bound the recent fit's time constant, which
	// is a quarter of the elapsed time in between: a ten-minute run looks
	// at the last two and a half minutes, an hour-long run the last two.
	recentMin = 10.0
	recentMax = 120.0
	// keepSamples bounds memory on a run lasting days. Older samples stay
	// in the whole-run sums; they only leave the recent window, where their
	// weight had long since gone to nothing.
	keepSamples = 4096

	// priorFileCost is what one file is assumed to cost, in bytes-worth of
	// transfer, until the run shows otherwise: a request's round trip
	// costs about as long as sending this much. It is only a starting
	// point. It decides the estimate while the run has seen one kind of
	// file -- tiny files say nothing about the link's speed, big ones
	// nothing about per-file overhead -- and is replaced by what the run
	// measures as soon as it has seen both.
	priorFileCost = 128 << 10
	// priorWeight is how strongly the fit holds to that starting point,
	// against the share of the run's timing it explains. Small: a cost the
	// data clearly supports wins; noise does not.
	priorWeight = 0.002
)

// fileCosts is the grid of per-file costs the fit chooses from: 1 KB to
// 64 MB, a quarter-power of two apart. A grid, not an equation, because
// it cannot go negative, cannot divide by a near-zero determinant, and
// costs a few hundred multiplications a second.
var fileCosts = func() []float64 {
	var out []float64
	for e := 10.0; e <= 26.0001; e += 0.25 {
		out = append(out, math.Pow(2, e))
	}
	return out
}()

func newEstimator(totalFiles, totalBytes int64) *estimator {
	return &estimator{totalFiles: float64(totalFiles), totalBytes: float64(totalBytes)}
}

// add records progress at time now.
func (e *estimator) add(now float64, files, bytes int64) {
	e.roll(now)
	e.openFiles += float64(files)
	e.openBytes += float64(bytes)
	e.doneFiles += float64(files)
	e.doneBytes += float64(bytes)
}

// roll closes the open slice if it has run long enough and holds progress.
func (e *estimator) roll(now float64) {
	if now-e.openStart < sliceSeconds || (e.openFiles == 0 && e.openBytes == 0) {
		return
	}
	s := sample{end: now, files: e.openFiles, bytes: e.openBytes, dt: now - e.openStart}
	e.cum.add(s.files, s.bytes, s.dt, 1)
	e.samples = append(e.samples, s)
	if len(e.samples) > keepSamples {
		e.samples = append(e.samples[:0], e.samples[len(e.samples)-keepSamples:]...)
	}
	e.openStart, e.openFiles, e.openBytes = now, 0, 0
}

// fit picks the per-file cost k that best explains the slices' timing,
// held toward prior. Each k implies a rate (all the work done ÷ all the time
// taken), and so a predicted duration for every slice; the best k is the one
// whose predictions are closest, measured as a share of the timing there is
// to explain.
func fit(s sums, prior float64) float64 {
	if s.w == 0 || s.tt == 0 {
		return prior
	}
	best, bestErr := prior, math.Inf(1)
	lp := math.Log(prior)
	// Few slices are few chances for noise to look like a pattern: the
	// prior holds hardest at the start and gives way as slices accumulate.
	pw := priorWeight + 2/(s.w+1)
	for _, k := range fileCosts {
		u := s.b + k*s.f // work, in bytes-worth
		if u <= 0 {
			continue
		}
		r := s.t / u // seconds per byte-worth
		// Σ (dt - r·(b + k·f))², expanded.
		uu := s.bb + 2*k*s.fb + k*k*s.ff
		ut := s.bt + k*s.ft
		sq := s.tt - 2*r*ut + r*r*uu
		d := math.Log(k) - lp
		e := sq/s.tt + pw*d*d
		if e < bestErr {
			best, bestErr = k, e
		}
	}
	return best
}

// remaining estimates the seconds left. ok is false when there is nothing
// yet to estimate from.
func (e *estimator) remaining(now float64) (float64, bool) {
	e.roll(now)
	if (e.doneFiles <= 0 && e.doneBytes <= 0) || now <= 0 {
		return 0, false
	}
	filesLeft := math.Max(e.totalFiles-e.doneFiles, 0)
	bytesLeft := math.Max(e.totalBytes-e.doneBytes, 0)
	if filesLeft == 0 && bytesLeft == 0 {
		return 0, true
	}

	// The whole run: the per-file cost from the fit, the speed from the
	// clock -- everything done so far, against all the time it took.
	k := fit(e.cum, priorFileCost)
	whole := now * (bytesLeft + k*filesLeft) / (e.doneBytes + k*e.doneFiles)

	// The recent past, the same way, held toward what the whole run found.
	tau := math.Max(recentMin, math.Min(recentMax, now/4))
	var rs sums
	for i := len(e.samples) - 1; i >= 0; i-- {
		s := e.samples[i]
		age := now - s.end
		if age > 6*tau {
			break
		}
		rs.add(s.files, s.bytes, s.dt, math.Exp(-age/tau))
	}
	// The slice still open counts in full: its time is the most recent
	// there is, and during a stall it is the only thing growing.
	open := now - e.openStart
	rk := fit(rs, k)
	recent := whole
	if work := rs.b + e.openBytes + rk*(rs.f+e.openFiles); work > 0 {
		recent = (rs.t + open) * (bytesLeft + rk*filesLeft) / work
	} else if open > 0 {
		// Nothing has finished for a long while: the whole-run view,
		// stretched by how long it has been since anything did.
		recent = whole * (1 + open/tau)
	}

	w := 0.5
	if whole > 0 && recent > whole {
		w = math.Min(1, 0.5+0.5*(recent/whole-1))
	}
	return safeFloat(w*recent + (1-w)*whole), true
}
