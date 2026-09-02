// Package engine executes a plan.Plan: it drains the fixed work list a
// planner produced, streaming bytes to remote storage with bounded, adaptive
// concurrency, and never lets one bad file stop the other sixty thousand.
//
// It depends only on interfaces it defines itself (Uploader, Reporter,
// FileSystem), not on the concrete remote or progress packages. Those are
// developed in parallel and their APIs are not settled; this package's tests
// run entirely against fakes, with no network and no real filesystem
// required for the logic under test. The parent package writes thin adapters
// from the real remote/progress implementations onto these interfaces.
package engine

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/saurabhhbansal/r2backup/internal/plan"
	"github.com/saurabhhbansal/r2backup/internal/scan"
)

// Uploader is the remote-storage operations a run needs. Put streams one
// object; onBytes is called as bytes leave the reader so a caller can show
// partial progress on a large file, and is expected to be cheap since it may
// be called many times per object. Copy performs a server-side copy (used for
// a detected move, so a renamed file costs one API call instead of a full
// re-upload). DeleteMany removes a batch of keys.
type Uploader interface {
	Put(ctx context.Context, key string, r io.Reader, size int64, meta map[string]string, onBytes func(int64)) (etag string, err error)
	Copy(ctx context.Context, from, to string) error
	DeleteMany(ctx context.Context, keys []string) error
}

// Reporter is how a run surfaces progress. AddBytes reports partial progress
// within a file (streamed as it uploads); CompleteFile reports one whole file
// finished, for counting completed-of-total independently of byte progress.
type Reporter interface {
	AddBytes(n int64)
	CompleteFile(size int64)
}

// nopReporter is used when Options.Reporter is nil, so call sites never have
// to nil-check it.
type nopReporter struct{}

func (nopReporter) AddBytes(int64)     {}
func (nopReporter) CompleteFile(int64) {}

// Failure records one item the run could not complete. A locked file, a
// permission error, or a remote error all land here; none of them abort the
// run.
type Failure struct {
	Key string
	Err error
}

// Result is the outcome of one Run: what got done, what didn't, and how long
// it took. Only keys that actually succeeded are reflected in Uploaded/Moved
// -- the caller should persist exactly these as done and leave everything
// else for the next run to retry.
type Result struct {
	Uploaded      int
	UploadedBytes int64
	Moved         int
	Deleted       int
	Failed        []Failure
	Elapsed       time.Duration

	// moveSources are the "From" keys of successfully-copied moves, staged
	// here so they can be appended to the delete list. Deleting them earlier
	// -- before the copy is confirmed -- would leave a window where a killed
	// run has the data in neither the old key nor the new one.
	moveSources []string
}

// Options configures an Engine.
type Options struct {
	// Root is the local directory scan.Entry.Key values are relative to.
	Root string

	// Uploader and Reporter are the required and optional collaborators;
	// FileSystem defaults to the real filesystem.
	Uploader Uploader
	Reporter Reporter
	FS       FileSystem

	// Workers is the initial worker count. Default 32.
	Workers int
	// MinWorkers is the floor adaptive concurrency backs off to. Default 4:
	// low enough to recover from sustained throttling, never zero so a run
	// can't stall itself out entirely.
	MinWorkers int
	// MaxWorkers is the ceiling adaptive concurrency grows to. Default 128.
	MaxWorkers int

	// AdaptiveInterval is how often throughput is sampled and the worker
	// count reconsidered. Default 10s. Ignored if Ticker is set.
	AdaptiveInterval time.Duration
	// Ticker overrides the adaptive-concurrency clock. Tests use
	// NewManualTicker so the policy can be driven tick-by-tick instead of
	// waiting on a real timer.
	Ticker Ticker

	// IsThrottle classifies an Uploader error as a rate-limit/503-shaped
	// backoff signal. The parent wires R2's actual error shapes here; nil
	// means "never throttle," which simply disables backoff.
	IsThrottle func(error) bool

	// Metadata, if set, computes the object metadata passed to Put for one
	// entry (e.g. original mtime, mode). Nil sends no metadata.
	Metadata func(scan.Entry) map[string]string

	// Now overrides the clock used for Result.Elapsed. Nil uses time.Now.
	Now func() time.Time

	// OnUploaded is called once for every object that genuinely landed, in
	// the order results are drained (single-threaded, so it needs no locking
	// of its own). This is how the caller records only what succeeded: a run
	// that fails halfway must not mark the failures done, and must not
	// discard the successes either.
	OnUploaded func(entry scan.Entry, etag string)

	// OnMoved is the same for a server-side copy, giving the old key, the new
	// key, and the size.
	OnMoved func(from, to string, size int64)
}

// Engine executes plans built from Options. It holds no per-run state, so a
// single Engine is safe to reuse across concurrent Run calls.
type Engine struct {
	opts Options
}

// New builds an Engine, filling in defaults for anything left zero.
func New(opts Options) *Engine {
	if opts.Workers <= 0 {
		opts.Workers = 32
	}
	if opts.MinWorkers <= 0 {
		opts.MinWorkers = 4
	}
	if opts.MaxWorkers <= 0 {
		opts.MaxWorkers = 128
	}
	if opts.MinWorkers > opts.MaxWorkers {
		opts.MinWorkers = opts.MaxWorkers
	}
	if opts.Workers < opts.MinWorkers {
		opts.Workers = opts.MinWorkers
	}
	if opts.Workers > opts.MaxWorkers {
		opts.Workers = opts.MaxWorkers
	}
	if opts.AdaptiveInterval <= 0 {
		opts.AdaptiveInterval = 10 * time.Second
	}
	if opts.FS == nil {
		opts.FS = osFS{}
	}
	if opts.Reporter == nil {
		opts.Reporter = nopReporter{}
	}
	if opts.IsThrottle == nil {
		opts.IsThrottle = func(error) bool { return false }
	}
	return &Engine{opts: opts}
}

// Run drains p to completion: uploads and moves first, deletes last (see
// jobKind below for why), reporting every failure without letting it stop
// the rest of the run. Cancelling ctx stops the pool promptly; Result then
// reflects exactly what had genuinely succeeded at that point, nothing more
// -- and the returned error is ctx.Err(), so a cancelled run is never
// mistaken for one that finished cleanly. A non-cancelled run that failed
// for some other reason still reports success here: per-item failures live
// in Result.Failed, not in this error, which is reserved for "the run itself
// did not run to completion."
func (e *Engine) Run(ctx context.Context, p *plan.Plan) (*Result, error) {
	if p == nil {
		return &Result{}, nil
	}
	if e.opts.Uploader == nil {
		return nil, fmt.Errorf("engine: Options.Uploader is nil")
	}

	started := e.now()
	r := &runner{opts: e.opts}
	result, err := r.execute(ctx, p)
	if result != nil {
		result.Elapsed = e.now().Sub(started)
	}
	return result, err
}

func (e *Engine) now() time.Time {
	if e.opts.Now != nil {
		return e.opts.Now()
	}
	return time.Now()
}

// jobKind distinguishes the two phases of work that share the worker pool.
// Deletes are deliberately not a jobKind: they are only ever issued after
// every upload and move job has finished (see runner.execute), so that a
// killed run never deletes an old object before its replacement is
// confirmed written.
type jobKind uint8

const (
	jobUpload jobKind = iota
	jobMove
)

type job struct {
	kind  jobKind
	entry scan.Entry
	move  plan.Move
}

type outcome struct {
	key     string
	from    string // move source, set only when isMove && success
	bytes   int64
	success bool
	isMove  bool
	err     error

	// entry and etag describe what actually landed, so the caller can record
	// it in the index. Counts alone are not enough: after a partly-failed run
	// the caller has to know precisely which keys succeeded, or it either
	// records failures as done (and never retries them) or records nothing
	// (and re-uploads everything next time).
	entry scan.Entry
	etag  string
}

// runner holds the per-Run mutable state (throughput counters that the
// adaptive-concurrency governor samples) that must not be shared across
// concurrent Run calls on the same Engine.
type runner struct {
	opts         Options
	windowBytes  int64
	throttleHits int64
}

func (r *runner) execute(ctx context.Context, p *plan.Plan) (*Result, error) {
	result := &Result{}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	total := len(p.Uploads) + len(p.Moves)
	jobs := make(chan job, total)
	for _, u := range p.Uploads {
		jobs <- job{kind: jobUpload, entry: u}
	}
	for _, m := range p.Moves {
		jobs <- job{kind: jobMove, move: m}
	}
	close(jobs)

	// Buffered to the exact job count: every worker can hand back its result
	// without ever blocking on a reader, so the pool doesn't need a select
	// against ctx just to publish an outcome.
	results := make(chan outcome, total)

	lim := newLimiter(r.opts.Workers, r.opts.MinWorkers, r.opts.MaxWorkers)

	ticker := r.opts.Ticker
	if ticker == nil {
		ticker = &intervalTicker{d: r.opts.AdaptiveInterval}
	}
	// The governor only makes sense while phase 1 is actively uploading; stop
	// it before the delete phase so it isn't still resizing a pool that has
	// no more work to hand out.
	govCtx, govCancel := context.WithCancel(runCtx)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		r.governor(govCtx, lim, ticker)
	}()

	workerCount := r.opts.MaxWorkers
	if total > 0 && workerCount > total {
		// No point standing up more idle goroutines than there is work to
		// ever hand out; the adaptive limiter still gates how many of them
		// run at once, so this only trims goroutine count, not concurrency.
		workerCount = total
	}
	if workerCount < 1 {
		workerCount = 1
	}
	var uploadWG sync.WaitGroup
	for i := 0; i < workerCount; i++ {
		uploadWG.Add(1)
		go func() {
			defer uploadWG.Done()
			r.worker(runCtx, jobs, lim, results)
		}()
	}
	uploadWG.Wait() // phase 1 (uploads + moves) fully drained, or abandoned by cancellation
	govCancel()
	wg.Wait() // governor joined before we touch its state again
	close(results)

	for out := range results {
		switch {
		case out.success && out.isMove:
			result.Moved++
			result.moveSources = append(result.moveSources, out.from)
			if r.opts.OnMoved != nil {
				r.opts.OnMoved(out.from, out.key, out.bytes)
			}
		case out.success:
			result.Uploaded++
			result.UploadedBytes += out.bytes
			if r.opts.OnUploaded != nil {
				r.opts.OnUploaded(out.entry, out.etag)
			}
		default:
			result.Failed = append(result.Failed, Failure{Key: out.key, Err: out.err})
		}
	}

	// Deletes run last, and are skipped entirely on a cancelled run rather
	// than issued against a possibly-incomplete set of replacement uploads.
	// A delete that outran its replacement's upload would leave the file in
	// neither place; skipping it here just leaves the stale object for the
	// next run to clean up once the upload has actually landed.
	if runCtx.Err() == nil {
		deleteKeys := make([]string, 0, len(p.Deletes)+len(result.moveSources))
		deleteKeys = append(deleteKeys, p.Deletes...)
		deleteKeys = append(deleteKeys, result.moveSources...)
		if len(deleteKeys) > 0 {
			if err := r.opts.Uploader.DeleteMany(runCtx, deleteKeys); err != nil {
				for _, k := range deleteKeys {
					result.Failed = append(result.Failed, Failure{Key: k, Err: fmt.Errorf("delete %s: %w", k, err)})
				}
			} else {
				result.Deleted = len(deleteKeys)
			}
		}
	}

	result.moveSources = nil
	// runCtx.Err() is nil on an ordinary completion and non-nil -- almost
	// always context.Canceled, since nothing here sets a deadline -- when the
	// caller cancelled ctx mid-run. Reporting that as success would be a lie
	// backup.Run has no way to catch: everything above already made a
	// cancelled run behave correctly (drain what's in flight, skip deletes,
	// keep only what genuinely landed), but "correct partial work, reported
	// as a clean finish" is exactly the shape of the bug this return fixes.
	// The caller decides what a cancelled run means for history and billing;
	// this is only responsible for not hiding that it happened.
	return result, runCtx.Err()
}

func (r *runner) worker(ctx context.Context, jobs <-chan job, lim *limiter, results chan<- outcome) {
	for j := range jobs {
		if !lim.acquire(ctx) {
			// Run ended while this job was still queued. Leave it
			// unreported rather than guessing at success or failure: the
			// caller only persists what Result says succeeded, so an
			// unprocessed key is simply retried whole on the next run.
			continue
		}
		var out outcome
		switch j.kind {
		case jobUpload:
			out = r.processUpload(ctx, j.entry)
		case jobMove:
			out = r.processMove(ctx, j.move)
		}
		lim.release()
		results <- out
	}
}

func (r *runner) addBytes(n int64) {
	atomic.AddInt64(&r.windowBytes, n)
	r.opts.Reporter.AddBytes(n)
}

func (r *runner) metadataFor(entry scan.Entry) map[string]string {
	if r.opts.Metadata == nil {
		return nil
	}
	return r.opts.Metadata(entry)
}

func (r *runner) noteIfThrottle(err error) {
	if err != nil && r.opts.IsThrottle(err) {
		atomic.AddInt64(&r.throttleHits, 1)
	}
}

func (r *runner) processUpload(ctx context.Context, entry scan.Entry) outcome {
	switch entry.Kind {
	case scan.KindSymlink:
		// A symlink is stored as its target string, never followed -- there
		// is nothing on disk to stream, so it bypasses FileSystem entirely.
		return r.uploadBytes(ctx, entry, []byte(entry.Target))
	case scan.KindEmptyDir:
		// Object storage has no directories; an empty one needs a zero-byte
		// marker object or it simply disappears on restore.
		return r.uploadBytes(ctx, entry, nil)
	default:
		return r.uploadFile(ctx, entry, false)
	}
}

func (r *runner) uploadBytes(ctx context.Context, entry scan.Entry, data []byte) outcome {
	etag, err := r.opts.Uploader.Put(ctx, entry.Key, bytes.NewReader(data), int64(len(data)), r.metadataFor(entry), r.addBytes)
	if err != nil {
		r.noteIfThrottle(err)
		return outcome{key: entry.Key, err: fmt.Errorf("upload %s: %w", entry.Key, err)}
	}
	r.opts.Reporter.CompleteFile(int64(len(data)))
	return outcome{key: entry.Key, bytes: int64(len(data)), success: true, entry: entry, etag: etag}
}

// uploadFile streams one regular file. retried is true on the one allowed
// re-attempt after a mid-upload change was detected.
func (r *runner) uploadFile(ctx context.Context, entry scan.Entry, retried bool) outcome {
	path := filepath.Join(r.opts.Root, filepath.FromSlash(entry.Key))

	pre, err := r.opts.FS.Stat(path)
	if err != nil {
		return outcome{key: entry.Key, err: fmt.Errorf("stat %s: %w", entry.Key, err)}
	}
	// entry carries whatever scan.Walk saw when the plan was built, which on
	// a retry is stale by definition -- the whole reason this is attempt two
	// is that the file has since changed. Refreshing Size/ModTime from pre,
	// taken immediately before this attempt's own read, is what keeps the
	// metadata Put uploads (and the record OnUploaded hands the index)
	// describing the bytes this attempt actually captured, rather than the
	// file's state before whichever save triggered the retry. Without this,
	// a retried upload lands with the right content under the wrong mtime,
	// which both restores incorrectly and makes the next run see a false
	// mismatch against its own freshly-corrected copy on disk.
	entry.Size = pre.Size()
	entry.ModTime = pre.ModTime()
	f, err := r.opts.FS.Open(path)
	if err != nil {
		// Locked by another process, permission denied, deleted since the
		// scan -- none of these are worth retrying here; they either persist
		// or will be picked up fresh on the next scan.
		return outcome{key: entry.Key, err: fmt.Errorf("open %s: %w", entry.Key, err)}
	}

	etag, err := r.opts.Uploader.Put(ctx, entry.Key, f, pre.Size(), r.metadataFor(entry), r.addBytes)
	closeErr := f.Close()
	if err != nil {
		r.noteIfThrottle(err)
		return outcome{key: entry.Key, err: fmt.Errorf("upload %s: %w", entry.Key, err)}
	}
	if closeErr != nil {
		return outcome{key: entry.Key, err: fmt.Errorf("close %s after upload: %w", entry.Key, closeErr)}
	}

	// Stat again now that the transfer is done. If size or mtime moved while
	// we were reading, the bytes Put just sent were some mix of old and new
	// content -- a torn object that must never be treated as a successful
	// upload.
	post, err := r.opts.FS.Stat(path)
	if err != nil {
		return outcome{key: entry.Key, err: fmt.Errorf("post-upload stat %s: %w", entry.Key, err)}
	}
	if post.Size() != pre.Size() || !post.ModTime().Equal(pre.ModTime()) {
		// Put has already succeeded from the remote's point of view: the
		// (possibly torn) bytes it just streamed are sitting live at
		// entry.Key right now, whatever we decide to report. Deciding to
		// retry or fail without removing them would leave exactly the
		// object this check exists to prevent -- reachable by any restore
		// run this instant, not just "eventually corrected by a future
		// backup" -- so it is deleted before either outcome below is ever
		// returned, on the first attempt as much as the last.
		if delErr := r.opts.Uploader.DeleteMany(ctx, []string{entry.Key}); delErr != nil {
			return outcome{key: entry.Key, err: fmt.Errorf(
				"upload %s: file changed during upload, and the resulting object could not be removed: %w",
				entry.Key, delErr)}
		}
		if retried {
			return outcome{key: entry.Key, err: fmt.Errorf("upload %s: file changed again during upload; giving up after one retry", entry.Key)}
		}
		// One retry handles the common case -- a save that finishes a moment
		// later -- without looping forever against a file under constant,
		// unrelated churn.
		return r.uploadFile(ctx, entry, true)
	}

	r.opts.Reporter.CompleteFile(pre.Size())
	return outcome{key: entry.Key, bytes: pre.Size(), success: true, entry: entry, etag: etag}
}

func (r *runner) processMove(ctx context.Context, mv plan.Move) outcome {
	if err := r.opts.Uploader.Copy(ctx, mv.From, mv.To); err != nil {
		r.noteIfThrottle(err)
		return outcome{key: mv.To, err: fmt.Errorf("copy %s -> %s: %w", mv.From, mv.To, err), isMove: true}
	}
	r.opts.Reporter.CompleteFile(mv.Size)
	return outcome{key: mv.To, from: mv.From, bytes: mv.Size, success: true, isMove: true}
}
