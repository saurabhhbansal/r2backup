// Package mirror copies backups from one store to another: Drime to R2, R2
// to Drime, or between two of the same kind. It is the "backup of the backup"
// -- a second copy, with a different company, of what is already off-site.
//
// It works store to store, not from the local folder, so it needs nothing
// but the two stores. That is what lets it run from any computer, and lets it
// copy the backups of machines that are not this one.
//
// A mirror is exact: what is in the source is copied, and what is in the
// destination but not the source is removed. Removal is what keeps the copy a
// copy rather than a slowly growing pile; r2backup's own trash folder is
// inside each set's prefix, so deleted files are mirrored as trash and stay
// recoverable from either side for the set's retention period.
package mirror

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/saurabhhbansal/r2backup/internal/remote"
)

// Options configures one mirror run.
type Options struct {
	Src, Dst remote.Store
	// Prefixes are what to mirror, each ending in "/". Typically a set's
	// KeyScope, or "machines/" for everything.
	Prefixes []string
	// Delete removes what the destination has and the source does not.
	Delete bool
	// DryRun works out and reports what would happen, and changes nothing.
	DryRun bool
	// Concurrency is how many files are copied at once. Zero is four.
	Concurrency int
	// Progress, if set, is called after each file is handled.
	Progress func(Progress)
}

// Progress is where a run has got to.
type Progress struct {
	FilesDone, FilesTotal int
	BytesDone, BytesTotal int64
}

// Failure is one file that could not be copied or removed. A failure does
// not stop the run; the next run tries it again.
type Failure struct {
	Key string
	Err error
}

// Report is what a run did.
type Report struct {
	Copied    int
	Bytes     int64
	Deleted   int
	Unchanged int
	Failures  []Failure
	// SkippedDeletes is set when deletions were planned but withheld
	// because the source looked empty -- see Run.
	SkippedDeletes int
	Elapsed        time.Duration
}

type copyJob struct {
	key  string
	size int64
}

// Plan is what Run will do, worked out from two listings.
type Plan struct {
	Copies    []copyJob
	Deletes   []string
	Unchanged int
	// Withheld are deletions not planned because the source prefix was
	// empty.
	Withheld int
}

// plan compares a source and destination listing.
//
// Neither store keeps a file's own modification time in its listing, so a
// changed file is recognised by its size or by when it was written: the
// destination's copy is written after the source's, so a source object newer
// than its destination copy has been replaced since the last mirror.
func plan(src, dst []remote.ListEntry, del bool) Plan {
	have := make(map[string]remote.ListEntry, len(dst))
	for _, e := range dst {
		have[e.Key] = e
	}
	var p Plan
	seen := make(map[string]bool, len(src))
	for _, e := range src {
		seen[e.Key] = true
		d, ok := have[e.Key]
		switch {
		case !ok, d.Size != e.Size, e.ModTime.After(d.ModTime):
			p.Copies = append(p.Copies, copyJob{key: e.Key, size: e.Size})
		default:
			p.Unchanged++
		}
	}
	for _, e := range dst {
		if seen[e.Key] {
			continue
		}
		// A source that lists nothing at all, against a destination that
		// has files, is far more likely to be the wrong account, an expired
		// token or an outage than a person deleting every file they own. The
		// mirror exists for exactly the day the source goes wrong, so it
		// must not empty itself on that day.
		if !del || len(src) == 0 {
			if del {
				p.Withheld++
			}
			continue
		}
		p.Deletes = append(p.Deletes, e.Key)
	}
	sort.Slice(p.Copies, func(i, j int) bool { return p.Copies[i].key < p.Copies[j].key })
	sort.Strings(p.Deletes)
	return p
}

// Run mirrors opts.Prefixes from opts.Src to opts.Dst.
func Run(ctx context.Context, opts Options) (*Report, error) {
	if opts.Src == nil || opts.Dst == nil {
		return nil, errors.New("mirror: source and destination are required")
	}
	if len(opts.Prefixes) == 0 {
		return nil, errors.New("mirror: nothing to mirror")
	}
	started := time.Now()
	rep := &Report{}

	var all Plan
	for _, prefix := range opts.Prefixes {
		if !strings.HasSuffix(prefix, "/") {
			return nil, fmt.Errorf("mirror: prefix %q must end in /", prefix)
		}
		src, err := opts.Src.List(ctx, prefix)
		if err != nil {
			return nil, fmt.Errorf("list source %s: %w", prefix, err)
		}
		dst, err := opts.Dst.List(ctx, prefix)
		if err != nil {
			return nil, fmt.Errorf("list destination %s: %w", prefix, err)
		}
		p := plan(src, dst, opts.Delete)
		all.Copies = append(all.Copies, p.Copies...)
		all.Deletes = append(all.Deletes, p.Deletes...)
		all.Unchanged += p.Unchanged
		all.Withheld += p.Withheld
	}
	rep.Unchanged = all.Unchanged
	rep.SkippedDeletes = all.Withheld

	var total int64
	for _, c := range all.Copies {
		total += c.size
	}
	if opts.DryRun {
		rep.Copied = len(all.Copies)
		rep.Bytes = total
		rep.Deleted = len(all.Deletes)
		rep.Elapsed = time.Since(started)
		return rep, nil
	}

	conc := opts.Concurrency
	if conc <= 0 {
		conc = 4
	}
	var (
		mu        sync.Mutex
		done      atomic.Int64
		bytesDone atomic.Int64
		wg        sync.WaitGroup
	)
	nFiles := len(all.Copies)
	report := func() {
		if opts.Progress != nil {
			opts.Progress(Progress{FilesDone: int(done.Load()), FilesTotal: nFiles, BytesDone: bytesDone.Load(), BytesTotal: total})
		}
	}
	jobs := make(chan copyJob)
	for i := 0; i < conc; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobs {
				err := copyOne(ctx, opts.Src, opts.Dst, j, func(n int64) { bytesDone.Add(n) })
				mu.Lock()
				if err != nil {
					rep.Failures = append(rep.Failures, Failure{Key: j.key, Err: err})
				} else {
					rep.Copied++
					rep.Bytes += j.size
				}
				mu.Unlock()
				done.Add(1)
				report()
			}
		}()
	}
feed:
	for _, j := range all.Copies {
		select {
		case jobs <- j:
		case <-ctx.Done():
			break feed
		}
	}
	close(jobs)
	wg.Wait()

	// Deletions go last, after every copy, so a run cut short never leaves
	// the destination with less than it started with.
	if ctx.Err() == nil && len(all.Deletes) > 0 {
		if err := opts.Dst.DeleteBatch(ctx, all.Deletes); err != nil {
			rep.Failures = append(rep.Failures, Failure{Key: fmt.Sprintf("(%d deletions)", len(all.Deletes)), Err: err})
		} else {
			rep.Deleted = len(all.Deletes)
		}
	}

	fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
	defer cancel()
	if err := remote.FlushStore(fctx, opts.Dst); err != nil {
		rep.Failures = append(rep.Failures, Failure{Key: "(file details)", Err: err})
	}
	rep.Elapsed = time.Since(started)
	if ctx.Err() != nil {
		return rep, ctx.Err()
	}
	return rep, nil
}

// copyOne streams one object from src to dst, carrying its metadata.
func copyOne(ctx context.Context, src, dst remote.Store, j copyJob, onBytes func(int64)) error {
	obj, err := src.Get(ctx, j.key)
	if err != nil {
		return err
	}
	defer obj.Body.Close()
	size := j.size
	md := obj.Metadata
	md.Size = size
	return dst.Put(ctx, remote.PutInput{
		Key:         j.key,
		Body:        obj.Body,
		Size:        size,
		Metadata:    md,
		ContentType: obj.ContentType,
		Progress:    onBytes,
	})
}
