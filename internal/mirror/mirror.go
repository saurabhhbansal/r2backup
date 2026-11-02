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
	// KeyScope, or "machines/" for everything; "" is the whole store, for a
	// Drime folder copied as it is.
	Prefixes []string
	// Delete removes what the destination has and the source does not.
	Delete bool
	// DryRun works out and reports what would happen, and changes nothing.
	DryRun bool
	// Concurrency is how many files are copied at once. Zero is four.
	Concurrency int
	// Progress, if set, is called after each file is handled.
	Progress func(Progress)
	// Relay, if set, is tried first for each file: Ultra fast mode, which
	// copies cloud to cloud. A file it will not take, or fails on, is copied
	// through this computer as usual.
	Relay Relay
}

// Relay copies one file from src to dst without it passing through this
// computer, calling onBytes as bytes land.
//
// An error wrapping remote.ErrNoLink means the file is not one for the relay
// (a file bound for a pack, say), and it quietly goes the usual way. One
// wrapping ErrRelayStopped means the relay cannot go on at all -- the day's
// allowance is used up, say -- and the rest of the run goes the usual way.
// Any other error is that one file, which is then copied the usual way.
type Relay interface {
	Copy(ctx context.Context, src, dst remote.Store, key string, size int64, onBytes func(int64)) error
}

// ErrRelayStopped is a relay that cannot copy anything more in this run.
var ErrRelayStopped = errors.New("ultra fast mode stopped")

// relayGiveUp is how many files in a row the relay may fail on before the
// run stops asking it.
const relayGiveUp = 5

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

	// Relayed and RelayedBytes are what went cloud to cloud (Ultra fast
	// mode); they are included in Copied and Bytes.
	Relayed      int
	RelayedBytes int64
	// RelayFellBack counts files the relay failed on that were then copied
	// through this computer.
	RelayFellBack int
	// RelayStopped, when set, is why the relay stopped partway, and the
	// rest of the run went through this computer.
	RelayStopped error
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
// backupScope is the one backup a key belongs to: machines/<machine>/<set>/
// for the layout `add` builds, and "" for anything else, which counts as one
// scope of its own.
func backupScope(key string) string {
	if !strings.HasPrefix(key, "machines/") {
		return ""
	}
	parts := strings.SplitN(key, "/", 4)
	if len(parts) < 4 {
		return ""
	}
	return parts[0] + "/" + parts[1] + "/" + parts[2] + "/"
}

func plan(src, dst []remote.ListEntry, del bool) Plan {
	have := make(map[string]remote.ListEntry, len(dst))
	for _, e := range dst {
		have[e.Key] = e
	}
	var p Plan
	seen := make(map[string]bool, len(src))
	scopes := make(map[string]bool)
	for _, e := range src {
		seen[e.Key] = true
		scopes[backupScope(e.Key)] = true
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
		//
		// The same holds one backup at a time: a whole backup the source no
		// longer has was deleted on purpose, from where backups go only --
		// deleting it from both is done directly, not by copying -- so the
		// second copy keeps it.
		if !del || len(src) == 0 || !scopes[backupScope(e.Key)] {
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
		if prefix != "" && !strings.HasSuffix(prefix, "/") {
			return nil, fmt.Errorf("mirror: prefix %q must end in / (or be empty, for everything)", prefix)
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
		if opts.Relay != nil {
			// A relayed file costs this computer one small request, not
			// its bytes, so more of them can be in flight.
			conc = 8
		}
	}
	rl := &relayState{relay: opts.Relay}
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
				relayed, fellBack := rl.try(ctx, opts.Src, opts.Dst, j, func(n int64) { bytesDone.Add(n) })
				var err error
				if !relayed {
					err = copyOne(ctx, opts.Src, opts.Dst, j, func(n int64) { bytesDone.Add(n) })
				}
				mu.Lock()
				if err != nil {
					rep.Failures = append(rep.Failures, Failure{Key: j.key, Err: err})
				} else {
					rep.Copied++
					rep.Bytes += j.size
				}
				if relayed {
					rep.Relayed++
					rep.RelayedBytes += j.size
				}
				if fellBack {
					rep.RelayFellBack++
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
	rep.RelayStopped = rl.stoppedWhy()
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

// relayState is one run's use of the relay: whether it is still being asked,
// and why not when it stopped.
type relayState struct {
	relay   Relay
	stopped atomic.Bool
	inARow  atomic.Int32
	mu      sync.Mutex
	why     error
}

// try offers j to the relay. relayed is whether it copied the file; fellBack
// is whether it tried and failed, so the file now goes through this computer.
func (r *relayState) try(ctx context.Context, src, dst remote.Store, j copyJob, onBytes func(int64)) (relayed, fellBack bool) {
	if r.relay == nil || r.stopped.Load() || ctx.Err() != nil {
		return false, false
	}
	var sent atomic.Int64
	err := r.relay.Copy(ctx, src, dst, j.key, j.size, func(n int64) {
		sent.Add(n)
		onBytes(n)
	})
	if err == nil {
		r.inARow.Store(0)
		return true, false
	}
	// Bytes counted for parts that landed are counted again by the copy
	// that replaces them, so they come off the progress first.
	if n := sent.Load(); n != 0 {
		onBytes(-n)
	}
	if errors.Is(err, remote.ErrNoLink) || ctx.Err() != nil {
		return false, false
	}
	if errors.Is(err, ErrRelayStopped) {
		r.stop(err)
	} else if r.inARow.Add(1) >= relayGiveUp {
		r.stop(fmt.Errorf("%w: it failed on %d files in a row, the last with: %v", ErrRelayStopped, relayGiveUp, err))
	}
	return false, true
}

func (r *relayState) stop(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.why == nil {
		r.why = err
	}
	r.stopped.Store(true)
}

func (r *relayState) stoppedWhy() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.why
}
