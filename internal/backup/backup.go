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

// Trash moves objects aside before they are overwritten or deleted. It is an
// interface so a run can be tested without one, and so retention 0 can simply
// pass nil.
type Trash interface {
	Move(ctx context.Context, prefix string, keys []string) error
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
		// PUT. This early return is where that claim is actually kept.
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
	})

	res, runErr := eng.Run(ctx, p)
	close(stopTicker)
	<-tickerDone
	if runErr != nil {
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
	for _, m := range p.Moves {
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

	rep.Operations = p.Operations(set.TrashEnabled())
	if err := opts.Index.AddOps(rep.Operations); err != nil {
		return nil, fmt.Errorf("record operation count: %w", err)
	}

	rep.Elapsed = now().Sub(started)
	obs.Phase(PhaseDone, rep)
	return rep, nil
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
