// Package progress turns observed transfer completions into an honest
// remaining-time estimate for a plan whose total size (bytes and file
// count) is known in full before the first byte moves.
//
// This is deliberately unlike a progress bar built on top of rclone, whose
// total keeps moving because file discovery is interleaved with transfer,
// and whose wall-clock time is dominated by a comparison phase the bar
// knows nothing about. Here the plan is fixed, so every second of elapsed
// time can be spent turning "how fast have we actually been going" into
// "how much longer, honestly" -- the whole point being that if the bar
// says two hours, two hours later the job is done.
package progress

import (
	"math"
	"sync"
	"time"
)

// rateWindow is the EWMA time constant for both rate estimators. It is not
// a hard window (an EWMA has no edge), but choosing a 30s decay constant
// means an event's influence on the reported rate fades to ~5% after about
// 90s -- long enough to smooth over the burstiness of individual request
// completions, short enough that a real slowdown (a throttled connection,
// a stalled worker) shows up in well under a minute instead of being
// diluted by minutes of stale history.
const rateWindow = 30 * time.Second

// fastRateWindow is a second, shorter time constant tracked alongside
// rateWindow. The reported rate is the LOWER of the two.
//
// This makes the estimator asymmetric on purpose: it believes bad news
// quickly and good news slowly. When throughput drops, the fast accumulator
// follows it down within seconds and the minimum picks that up, so the ETA
// rises almost immediately. When throughput improves, the slow accumulator
// holds the estimate back until the improvement has actually persisted.
//
// The asymmetry is the whole requirement restated. Over-predicting means
// finishing early, which costs nothing. Under-predicting means the number on
// screen said two hours, the user shut the machine down, and the backup was
// not done. Those two errors are not equally bad, so the estimator does not
// treat them equally. In steady state both accumulators converge on the same
// value and the minimum costs nothing.
const fastRateWindow = 10 * time.Second

// etaSmoothingWindow damps the *reported* ETA, separately from damping the
// underlying rates. Rates feed the ETA multiplicatively (remaining/rate),
// so even a smoothed rate produces an ETA that swings whenever a large
// file happens to land right before a Snapshot call. Smoothing the ETA
// itself, with its own (shorter) time constant, is what keeps the number
// on screen from visibly jittering second to second while still tracking
// real trend changes within well under a minute.
const etaSmoothingWindow = 6 * time.Second

// minElapsedForETA and minPercentForETA gate when an ETA is trustworthy
// enough to show. Both must hold before ETAKnown is true.
const (
	minElapsedForETA = 5 * time.Second
	minPercentForETA = 1.0 // percent
)

// Snapshot is a point-in-time read of a Tracker's progress and estimate.
// ETA and ETAKnown are only meaningful together: when ETAKnown is false,
// ETA is not a prediction (it is either zero or based on too little data)
// and must not be displayed as one.
type Snapshot struct {
	BytesDone  int64
	BytesTotal int64
	FilesDone  int64
	FilesTotal int64

	// ByteRate and FileRate are the current EWMA estimates, in units per
	// second. They are always finite and non-negative.
	ByteRate float64
	FileRate float64

	// Percent is overall plan progress, 0..100. It follows bytes when the
	// plan has any bytes (the common case), and falls back to file count
	// only for an all-zero-byte plan, so it never divides by zero.
	Percent float64

	Elapsed time.Duration

	// ETA is the estimated remaining time, smoothed. Ignore it unless
	// ETAKnown is true.
	ETA      time.Duration
	ETAKnown bool
}

// Tracker accumulates completions for a plan of known total size and turns
// them into rate estimates and a damped ETA. It is safe for concurrent use:
// uploads normally complete from multiple worker goroutines at once.
type Tracker struct {
	mu sync.Mutex

	now   func() time.Time
	start time.Time

	totalBytes int64
	totalFiles int64

	doneBytes int64
	doneFiles int64

	// byteRate and fileRate are the EWMA accumulators. They are updated by
	// a "decay then add" step: first decay for the elapsed wall-clock time
	// since the last update, then (for an event) add that event's
	// contribution. See decayLocked and the doc comments on Complete and
	// AddBytes for why the contribution is amount/rateWindow.
	byteRate float64
	fileRate float64
	lastRate time.Time

	// byteRateFast and fileRateFast are the same estimators over
	// fastRateWindow. See that constant for why both exist.
	byteRateFast float64
	fileRateFast float64

	// byteRateStart/fileRateStart are the moment each accumulator first
	// received a contribution (zero value until then). An accumulator
	// that starts at zero and charges up via decay+impulse behaves like an
	// RC circuit: fed a truly constant rate R starting at t0, its raw
	// value at time t is R*(1-exp(-(t-t0)/tau)), not R -- understating the
	// real rate for the first couple of time constants after activity
	// begins. Snapshot divides the raw accumulator by that same
	// (1-exp(-(t-t0)/tau)) factor to cancel the startup bias, the same
	// debiasing trick Adam uses for its own EWMA moment estimates. Without
	// it, a plan whose 25% checkpoint arrives well inside one rateWindow
	// of the first completion -- exactly the case for a fast, steady
	// workload -- reports a rate several times lower than what is actually
	// happening, which turns into a wildly over-estimated ETA.
	byteRateStart time.Time
	fileRateStart time.Time

	// etaSmoothed is the damped ETA in seconds, or -1 before it has ever
	// been seeded (i.e. before ETAKnown has ever been true).
	etaSmoothed float64
	lastETA     time.Time
}

// New creates a Tracker for a plan with the given total bytes and total
// file count. now supplies the current time on every call; pass nil to use
// time.Now. Tests pass a fake clock so rate decay and ETA damping are
// driven by simulated time rather than real wall-clock delay -- deriving
// clock-dependent tests from real sleeps would make them slow and flaky.
func New(totalBytes, totalFiles int64, now func() time.Time) *Tracker {
	if now == nil {
		now = time.Now
	}
	if totalBytes < 0 {
		totalBytes = 0
	}
	if totalFiles < 0 {
		totalFiles = 0
	}
	start := now()
	return &Tracker{
		now:         now,
		start:       start,
		totalBytes:  totalBytes,
		totalFiles:  totalFiles,
		lastRate:    start,
		etaSmoothed: -1,
		lastETA:     start,
	}
}

// decayLocked advances both rate accumulators to now, applying exponential
// decay for the elapsed time. Called before every mutation and before every
// read, so a Snapshot taken during a stall reflects the stall: with no new
// completions the rate keeps decaying every time it is observed, which is
// exactly what makes the ETA rise instead of freezing on stale numbers.
// mu must be held.
func (t *Tracker) decayLocked(now time.Time) {
	dt := now.Sub(t.lastRate)
	if dt <= 0 {
		return
	}
	factor := math.Exp(-dt.Seconds() / rateWindow.Seconds())
	t.byteRate *= factor
	t.fileRate *= factor

	fastFactor := math.Exp(-dt.Seconds() / fastRateWindow.Seconds())
	t.byteRateFast *= fastFactor
	t.fileRateFast *= fastFactor

	t.lastRate = now
}

// Complete records that one whole file has finished transferring. bytes is
// the amount to credit for this call: pass the file's full size for a file
// that was never reported incrementally, or the remainder not already
// reported via AddBytes for a multipart file (0 if every byte was already
// acknowledged part by part). Complete feeds both rate estimators, since
// finishing a file is itself an acknowledged transfer of bytes as well as
// of one file.
func (t *Tracker) Complete(bytes int64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	t.decayLocked(now)

	if bytes > 0 {
		// The impulse added per event is amount/rateWindow, not amount/dt.
		// Combined with the exponential decay above, this is the
		// continuous-time analogue of an EWMA: a steady stream of events
		// converges the accumulator to the true average rate, while a
		// single isolated event contributes a bounded, fading impulse
		// rather than one huge instantaneous "rate" spike.
		if t.byteRateStart.IsZero() {
			t.byteRateStart = now
		}
		t.byteRate += float64(bytes) / rateWindow.Seconds()
		t.byteRateFast += float64(bytes) / fastRateWindow.Seconds()
		t.doneBytes += bytes
	}
	if t.fileRateStart.IsZero() {
		t.fileRateStart = now
	}
	t.fileRate += 1.0 / rateWindow.Seconds()
	t.fileRateFast += 1.0 / fastRateWindow.Seconds()
	t.doneFiles++
}

// AddBytes records bytes acknowledged for a transfer still in progress --
// e.g. one part of a multipart upload coming back successful. This is what
// lets the bar move smoothly during a single large file: without it, a
// single 200MB upload would report a completely flat byte rate (and thus a
// frozen ETA) until the one Complete call at the very end.
//
// AddBytes feeds the byte rate, because a part acknowledgement is a real,
// confirmed transfer of that many bytes -- not a raw "bytes written to a
// socket buffer" estimate that could stall or roll back. It does NOT
// increment the file count or feed the file rate: the file this chunk
// belongs to is not finished until Complete is called for it.
func (t *Tracker) AddBytes(delta int64) {
	if delta <= 0 {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	t.decayLocked(now)
	if t.byteRateStart.IsZero() {
		t.byteRateStart = now
	}
	t.byteRate += float64(delta) / rateWindow.Seconds()
	t.byteRateFast += float64(delta) / fastRateWindow.Seconds()
	t.doneBytes += delta
}

// clampPercent keeps a done/total ratio in [0, 100] even if a caller's
// Complete/AddBytes calls overshoot the declared total (e.g. a size
// reported by the source turned out to be an estimate) -- the bar should
// pin at 100%, not overflow past it.
func clampPercent(done, total int64) float64 {
	if total <= 0 {
		return 0
	}
	p := 100 * float64(done) / float64(total)
	if p > 100 {
		return 100
	}
	if p < 0 {
		return 0
	}
	return p
}

// computePercent is overall plan progress. It is byte-based whenever the
// plan has any bytes, which is the normal case and matches what the bar
// visually fills with. It falls back to file count only when the plan has
// zero total bytes (e.g. a plan made entirely of empty files), and reports
// a completed plan as 100% rather than dividing by zero when there is
// nothing to do at all.
func computePercent(bytesDone, bytesTotal, filesDone, filesTotal int64) float64 {
	switch {
	case bytesTotal > 0:
		return clampPercent(bytesDone, bytesTotal)
	case filesTotal > 0:
		return clampPercent(filesDone, filesTotal)
	default:
		return 100
	}
}

// safeFloat guards against NaN/Inf ever reaching a Snapshot. Every input to
// the rate and ETA math is already finite by construction (decay factors
// are in (0,1], impulses are finite), but this is cheap insurance against a
// future change introducing a division or a pathological duration -- the
// contract with callers is that these numbers are always safe to format
// and display without a NaN/Inf check of their own.
func safeFloat(v float64) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
		return 0
	}
	return v
}

// correctedRate undoes the zero-start charge-up bias described on
// byteRateStart/fileRateStart above. startedAt is the zero Time until the
// accumulator has ever received a contribution, in which case there is
// nothing to correct -- the rate really is zero. A near-zero elapsed time
// since the first contribution makes the correction factor itself near
// zero; dividing by it would amplify noise into a huge or infinite number,
// so that case falls back to the raw (small, honest, uncorrected) value
// rather than guessing.
func correctedRate(raw float64, startedAt, now time.Time, window time.Duration) float64 {
	if startedAt.IsZero() {
		return 0
	}
	dt := now.Sub(startedAt).Seconds()
	if dt <= 0 {
		return raw
	}
	factor := 1 - math.Exp(-dt/window.Seconds())
	const minFactor = 1e-3
	if factor < minFactor {
		return raw
	}
	return raw / factor
}

// Snapshot reads current progress and produces a damped ETA.
func (t *Tracker) Snapshot() Snapshot {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	t.decayLocked(now)

	bytesDone := t.doneBytes
	if bytesDone > t.totalBytes {
		bytesDone = t.totalBytes
	}
	filesDone := t.doneFiles
	if filesDone > t.totalFiles {
		filesDone = t.totalFiles
	}
	bytesRemaining := t.totalBytes - bytesDone
	filesRemaining := t.totalFiles - filesDone

	// The lower of the two windows wins. See fastRateWindow: a slowdown must
	// reach the estimate immediately, a speed-up only once it has held.
	byteRate := safeFloat(math.Min(
		correctedRate(t.byteRate, t.byteRateStart, now, rateWindow),
		correctedRate(t.byteRateFast, t.byteRateStart, now, fastRateWindow),
	))
	fileRate := safeFloat(math.Min(
		correctedRate(t.fileRate, t.fileRateStart, now, rateWindow),
		correctedRate(t.fileRateFast, t.fileRateStart, now, fastRateWindow),
	))

	// Each dimension only contributes an ETA when it has both a positive
	// rate and remaining work; a rate of exactly zero must NOT turn into
	// an infinite ETA (dividing by it), it must simply not participate.
	// This matters for e.g. a single large file, where the file rate is
	// structurally zero until the very last instant -- if a zero file rate
	// forced its ETA contribution to infinity, max() would report an
	// infinite ETA for the entire transfer of a single file, which is
	// exactly backwards.
	var byteETA, fileETA float64
	if byteRate > 0 && bytesRemaining > 0 {
		byteETA = float64(bytesRemaining) / byteRate
	}
	if fileRate > 0 && filesRemaining > 0 {
		fileETA = float64(filesRemaining) / fileRate
	}
	// The max is the point: request-bound and bandwidth-bound workloads
	// are different physics, and whichever constraint actually binds is
	// whichever dimension predicts the longer remaining time.
	rawETA := math.Max(byteETA, fileETA)

	elapsed := now.Sub(t.start)
	percent := computePercent(bytesDone, t.totalBytes, filesDone, t.totalFiles)

	known := elapsed >= minElapsedForETA && percent >= minPercentForETA && rawETA > 0

	if known {
		if t.etaSmoothed < 0 {
			// First trustworthy estimate: seed the filter directly rather
			// than smoothing from an unset/zero value, which would make
			// the first displayed ETA understate the real one.
			t.etaSmoothed = rawETA
			t.lastETA = now
		} else if dt := now.Sub(t.lastETA); dt > 0 {
			alpha := 1 - math.Exp(-dt.Seconds()/etaSmoothingWindow.Seconds())
			t.etaSmoothed += alpha * (rawETA - t.etaSmoothed)
			t.lastETA = now
		}
	}

	etaOut := safeFloat(t.etaSmoothed)

	return Snapshot{
		BytesDone:  bytesDone,
		BytesTotal: t.totalBytes,
		FilesDone:  filesDone,
		FilesTotal: t.totalFiles,
		ByteRate:   byteRate,
		FileRate:   fileRate,
		Percent:    percent,
		Elapsed:    elapsed,
		ETA:        time.Duration(etaOut * float64(time.Second)),
		ETAKnown:   known,
	}
}
