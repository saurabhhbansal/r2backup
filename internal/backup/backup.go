// Package backup runs one backup: scan, plan, transfer, record.
//
// The ordering is the product. Scanning and planning finish before a single
// byte moves, so the progress bar starts with a total that never changes. The
// tool this replaces discovered files while transferring them, and its estimate
// was worthless as a result.
package backup

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/saurabhhbansal/r2backup/internal/engine"
	"github.com/saurabhhbansal/r2backup/internal/index"
	"github.com/saurabhhbansal/r2backup/internal/plan"
	"github.com/saurabhhbansal/r2backup/internal/progress"
	"github.com/saurabhhbansal/r2backup/internal/remote"
	"github.com/saurabhhbansal/r2backup/internal/scan"
	"github.com/saurabhhbansal/r2backup/internal/sets"
)

// Phase is which of the three stages a run is in.
type Phase string

const (
	PhaseScanning  Phase = "scanning"
	PhasePlanning  Phase = "planning"
	PhaseUploading Phase = "uploading"
	PhaseDone      Phase = "done"
)

// Report is what a run tells the caller happened.
type Report struct {
	Set string

	ScannedFiles int64
	ScannedBytes int64

	Planned   *plan.Plan
	Uploaded  int
	Moved     int
	Deleted   int
	Bytes     int64
	Unchanged int

	// Problems are files that could not be read. A run completes despite
	// them: one locked .pst must not stop 60,000 other files.
	Problems []scan.Problem
	// Failures are objects the transfer itself could not place.
	Failures []engine.Failure
	// Collisions are files that could not be stored because another file
	// normalizes onto the same key. Always report these: they are the one
	// way a backup can be incomplete without anything having failed.
	Collisions []plan.Collision

	// Abandoned is how many unfinished uploads this run gave up on and
	// aborted, freeing the parts the bucket was billing for.
	Abandoned int

	// Pruned is the expired trash this run cleared.
	Pruned Pruned
	// PruneErr is why expired trash could not be cleared, if it could not.
	// It never fails the run: the backup itself succeeded, and old trash
	// that outstays its window costs storage, not data. It is reported
	// rather than swallowed, because a retention window nothing enforces is
	// the bug this field exists to make visible.
	PruneErr error

	Operations int
	Elapsed    time.Duration
}

// Succeeded reports whether everything the plan asked for actually happened.
// Collisions do not make a run fail -- nothing went wrong, the folder simply
// contains two files that cannot both be stored -- but they must be reported.
func (r *Report) Succeeded() bool { return len(r.Failures) == 0 }

// Complete reports whether the bucket now holds everything on disk. A run can
// succeed and still be incomplete when two files normalize onto one key.
func (r *Report) Complete() bool { return r.Succeeded() && len(r.Collisions) == 0 }

// Observer receives phase changes and progress snapshots so a caller can draw
// a bar, write a progress file, or stay silent. It is called from the run's
// own goroutine.
type Observer interface {
	Phase(p Phase, r *Report)
	Progress(s progress.Snapshot)
}

// nopObserver is used when the caller does not want to be told anything, which
// is the normal case for a scheduled run.
type nopObserver struct{}

func (nopObserver) Phase(Phase, *Report)       {}
func (nopObserver) Progress(progress.Snapshot) {}

// Trash moves objects aside before they are overwritten or deleted, and
// clears what has outlived the set's retention. It is an interface so a run
// can be tested without one, and so retention 0 can simply pass nil.
type Trash interface {
	Move(ctx context.Context, prefix string, keys []string) error
	// Prune removes trash older than the set's retention window. A run is
	// the only thing that calls it: there is no daemon, so if a backup does
	// not expire old trash then nothing ever does -- which is exactly what
	// used to happen. trash.Prune was fully written and fully tested and had
	// no caller at all, so every "recoverable until <date>" that `trash ls`
	// printed was a date nothing acted on, and trash grew forever.
	Prune(ctx context.Context, prefix string) (Pruned, error)
}

// Pruned is what clearing expired trash did.
type Pruned struct {
	// Dates are the trash date directories removed, oldest first.
	Dates []string
	// Keys is how many trashed objects were removed.
	Keys int
	// Ops is the Class A cost of finding them. The deletes themselves are
	// free on R2 however many there were.
	Ops int
}

// Options configures one run.
type Options struct {
	Set    sets.Set
	Index  *index.DB
	Client *remote.Client

	// Trash, when non-nil, is given every key about to be overwritten or
	// deleted, before that happens.
	Trash Trash

	Observer Observer

	// ProgressEvery is how often Observer.Progress is called. Zero uses one
	// second, which is what the on-disk progress file is written at.
	ProgressEvery time.Duration

	// DetectMoves turns a renamed folder into server-side copies instead of a
	// full re-upload.
	DetectMoves bool

	Now func() time.Time
}

// ErrRootMissing means the folder this set points at is not there.
//
// It is returned rather than handled, because the two possible readings -- the
// folder was renamed, or it really was deleted -- have opposite consequences
// and only the caller knows whether a person is present to choose. Nothing is
// deleted on this path, ever.
var ErrRootMissing = errors.New("the folder this set points at no longer exists")

// ErrCancelled marks a Run that stopped because its context was cancelled --
// someone pressed q on the running screen, or the process is shutting down --
// rather than because anything actually went wrong.
//
// A caller building a history entry (runOne, and its dashboard equivalent
// recordRun) tests for this with errors.Is so `status` and the dashboard can
// read a stopped run as "cancelled" instead of "failed". That is a sentinel a
// caller can check on purpose, unlike Go's raw "context canceled" text, which
// was never written to be shown to a person and used to leak straight through
// into runstate.Past.Error and onto the screen.
var ErrCancelled = errors.New("run cancelled")

// Run performs one backup.
func Run(ctx context.Context, opts Options) (*Report, error) {
	if opts.Index == nil || opts.Client == nil {
		return nil, errors.New("backup: index and client are required")
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	obs := opts.Observer
	if obs == nil {
		obs = nopObserver{}
	}
	started := now()
	rep := &Report{Set: opts.Set.Name}

	// --- 1. Scan -----------------------------------------------------------
	obs.Phase(PhaseScanning, rep)
	set := opts.Set
	scanned, err := scan.Walk(ctx, scan.Options{
		Root: set.Root,
		Skip: func(key string, isDir bool) bool { return set.Excluded(key) },
	})
	if err != nil {
		if errors.Is(err, scan.ErrRootMissing) {
			// Deliberately not turned into 61,204 deletions. See ErrRootMissing.
			return nil, fmt.Errorf("%w: %s", ErrRootMissing, set.Root)
		}
		return nil, fmt.Errorf("scan %q: %w", set.Root, err)
	}
	rep.ScannedFiles, rep.ScannedBytes = scanned.Files, scanned.Bytes
	rep.Problems = scanned.Problems

	// --- 2. Plan -----------------------------------------------------------
	obs.Phase(PhasePlanning, rep)
	p, err := plan.Build(scanned, priorOf(opts.Index, set.Name), plan.Options{DetectMoves: opts.DetectMoves})
	if err != nil {
		return nil, err
	}
	rep.Planned = p
	rep.Unchanged = p.Unchanged
	rep.Collisions = p.Collisions

	if p.Empty() {
		// A run where nothing changed costs nothing: no LIST, no HEAD, no
		// PUT. This early return is where that claim is actually kept --
		// except for expiring trash, which is due at most once a day and has
		// to happen even here. A set that never changes never trashes
		// anything either, so if an unchanged run does not sweep, that set's
		// trash is kept and paid for forever.
		expireTrash(ctx, opts, rep)
		rep.Operations = rep.Pruned.Ops
		if rep.Operations > 0 {
			if err := opts.Index.AddOps(rep.Operations); err != nil {
				return nil, fmt.Errorf("record operation count: %w", err)
			}
		}
		rep.Elapsed = now().Sub(started)
		obs.Phase(PhaseDone, rep)
		return rep, nil
	}

	// --- 3. Trash the outgoing versions ------------------------------------
	prefix := set.Prefix
	if opts.Trash != nil && set.TrashEnabled() {
		doomed := make([]string, 0, len(p.Deletes))
		doomed = append(doomed, p.Deletes...)
		for _, u := range p.Uploads {
			if _, err := opts.Index.Get(set.Name, u.Key); err == nil {
				doomed = append(doomed, u.Key) // an overwrite
			}
		}
		if len(doomed) > 0 {
			if err := opts.Trash.Move(ctx, prefix, doomed); err != nil {
				return nil, fmt.Errorf("move %d objects to trash: %w", len(doomed), err)
			}
		}
	}

	// --- 4. Transfer -------------------------------------------------------
	obs.Phase(PhaseUploading, rep)
	tracker := progress.New(p.UploadBytes, int64(len(p.Uploads)), now)

	tickEvery := opts.ProgressEvery
	if tickEvery == 0 {
		tickEvery = time.Second
	}
	stopTicker := make(chan struct{})
	tickerDone := make(chan struct{})
	go func() {
		defer close(tickerDone)
		t := time.NewTicker(tickEvery)
		defer t.Stop()
		for {
			select {
			case <-stopTicker:
				return
			case <-ctx.Done():
				return
			case <-t.C:
				obs.Progress(tracker.Snapshot())
			}
		}
	}()

	// Only what genuinely landed is recorded. Anything else is retried on the
	// next run, which is the correct behaviour for a killed or partial run.
	var recorded []index.Record
	// Same reasoning as OnUploaded, for the other kind of transfer: a move
	// whose CopyObject failed must not be recorded as done, or the index
	// ends up pointing at a new key nothing was ever written to while the
	// source key -- the only copy that actually exists -- is forgotten and
	// never deleted either.
	var moved []plan.Move
	eng := engine.New(engine.Options{
		Root:       set.Root,
		Uploader:   uploader{client: opts.Client, prefix: prefix + "/current"},
		Reporter:   reporter{t: tracker},
		Metadata:   metadataFor,
		IsThrottle: isThrottle,
		OnUploaded: func(e scan.Entry, etag string) {
			recorded = append(recorded, index.Record{
				Key:        e.Key,
				Size:       e.Size,
				ModTime:    e.ModTime.UnixNano(),
				ETag:       etag,
				UploadedAt: now().UnixNano(),
				Kind:       index.Kind(e.Kind),
				Target:     e.Target,
			})
		},
		OnMoved: func(from, to string, size int64) {
			moved = append(moved, plan.Move{From: from, To: to, Size: size})
		},
	})

	res, runErr := eng.Run(ctx, p)
	close(stopTicker)
	<-tickerDone
	if runErr != nil {
		// eng.Run returns ctx.Err() -- almost always context.Canceled -- when
		// the caller stopped the run rather than something going wrong with
		// it: someone pressed q in the interface, or the process is shutting
		// down. Returned as ErrCancelled itself rather than wrapping runErr
		// (which carries nothing more than that same "context canceled"),
		// because ErrCancelled's Error() is what ends up in
		// runstate.Past.Error and on screen in `status`, and a person who
		// stopped a run on purpose did not ask to read Go's plumbing back.
		// errors.Is(err, ErrCancelled) is how runOne and the dashboard's
		// equivalent recordRun tell this apart from a run that failed for
		// real, when they build that history entry. Either way this return
		// is what keeps a cancelled run from being recorded as a clean one:
		// nothing below this point -- the index writes that mark uploads and
		// moves done, trash expiry, the operation count -- runs for a run
		// that did not finish, exactly as it already didn't for the only
		// other way this function used to return a non-nil error here.
		if ctx.Err() != nil {
			return nil, ErrCancelled
		}
		return nil, runErr
	}
	obs.Progress(tracker.Snapshot())

	rep.Uploaded, rep.Moved, rep.Deleted = res.Uploaded, res.Moved, res.Deleted
	rep.Bytes = res.UploadedBytes
	rep.Failures = res.Failed

	// --- 5. Record ---------------------------------------------------------
	if len(recorded) > 0 {
		if err := opts.Index.PutMany(set.Name, recorded); err != nil {
			return nil, fmt.Errorf("record %d uploads in the index: %w", len(recorded), err)
		}
	}
	if res.Deleted > 0 && len(p.Deletes) > 0 {
		if err := opts.Index.DeleteMany(set.Name, p.Deletes); err != nil {
			return nil, fmt.Errorf("forget %d deleted keys: %w", len(p.Deletes), err)
		}
	}
	for _, m := range moved {
		old, err := opts.Index.Get(set.Name, m.From)
		if err != nil {
			continue
		}
		old.Key = m.To
		if err := opts.Index.Put(set.Name, old); err != nil {
			return nil, fmt.Errorf("record move to %q: %w", m.To, err)
		}
		if err := opts.Index.Delete(set.Name, m.From); err != nil {
			return nil, fmt.Errorf("forget moved key %q: %w", m.From, err)
		}
	}

	// After the uploads, so a run that fails partway has not also spent
	// operations tidying.
	expireTrash(ctx, opts, rep)

	// Parts of an upload nobody ever finished are billed, and they appear in
	// no object listing -- a charge with nothing on screen to explain it. A
	// run that got this far is the right moment to let them go: the handful
	// of requests is noise beside the ones it just spent, and only uploads
	// older than remote.ResumeMaxAge are touched, so work that is genuinely
	// being resumed each day is never swept out from under itself.
	if n, err := opts.Client.AbandonStaleUploads(ctx); err == nil {
		rep.Abandoned = n
	}

	rep.Operations = p.Operations(set.TrashEnabled()) + rep.Pruned.Ops
	if err := opts.Index.AddOps(rep.Operations); err != nil {
		return nil, fmt.Errorf("record operation count: %w", err)
	}

	rep.Elapsed = now().Sub(started)
	obs.Phase(PhaseDone, rep)
	return rep, nil
}

// expireTrash sweeps whatever has outlived the set's retention window,
// at most once per UTC day.
//
// There is no daemon, so a run is the only thing that can do this, and for a
// long time nothing did: trash.Prune was written, documented and covered by
// its own tests with no caller anywhere, so every "recoverable until <date>"
// that `trash ls` printed was a date nothing acted on. A failure is recorded
// on the report and never fails the run -- the backup itself succeeded, and
// trash outstaying its window costs storage, not data -- but it is never
// swallowed, because a retention window nothing enforces is the whole bug.
func expireTrash(ctx context.Context, opts Options, rep *Report) {
	if opts.Trash == nil {
		return
	}
	due, err := opts.Index.ClaimDailyPrune(opts.Set.Name)
	if err != nil {
		rep.PruneErr = err
		return
	}
	if !due {
		return
	}
	pruned, err := opts.Trash.Prune(ctx, opts.Set.Prefix)
	if err != nil {
		rep.PruneErr = err
		return
	}
	rep.Pruned = pruned
}

// priorOf adapts the index to what the planner needs.
func priorOf(db *index.DB, set string) plan.Prior { return indexPrior{db: db, set: set} }

type indexPrior struct {
	db  *index.DB
	set string
}

func (i indexPrior) Each(fn func(plan.PriorEntry) bool) error {
	recs, err := i.db.All(i.set)
	if err != nil {
		return err
	}
	for _, r := range recs {
		if !fn(plan.PriorEntry{
			Key:     r.Key,
			Size:    r.Size,
			ModTime: time.Unix(0, r.ModTime),
			Kind:    scan.Kind(r.Kind),
			Target:  r.Target,
		}) {
			return nil
		}
	}
	return nil
}
