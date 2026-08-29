package restore

import (
	"context"
	"fmt"
	"os"
	"path"
	"strings"
	"time"

	"github.com/saurabhhbansal/r2backup/internal/progress"
	"github.com/saurabhhbansal/r2backup/internal/remote"
)

// matchesOnly reports whether relPath should be restored given an --only
// pattern.
//
// A pattern with no glob metacharacters is matched the same way
// sets.Set.Excluded matches an exclude rule: exactly, or as a directory
// prefix, so "--only docs" restores everything under docs/ without the
// caller needing to know glob syntax at all. A pattern ending "/**"
// restores everything under that directory explicitly, for a caller who
// does want to write a pattern. Anything else is matched with path.Match
// against the whole relative path -- note that, like path.Match itself,
// "*" does not cross a "/".
func matchesOnly(pattern, relPath string) bool {
	if pattern == "" {
		return true
	}
	if strings.HasSuffix(pattern, "/**") {
		base := strings.TrimSuffix(pattern, "/**")
		return relPath == base || strings.HasPrefix(relPath, base+"/")
	}
	if !strings.ContainsAny(pattern, "*?[") {
		return relPath == pattern || strings.HasPrefix(relPath, pattern+"/")
	}
	// path.Match, not filepath.Match: a key is always forward-slashed
	// regardless of platform, and filepath.Match would apply Windows'
	// separator rules to it when restoring there.
	ok, err := path.Match(pattern, relPath)
	return err == nil && ok
}

// planItem is one object queued for download.
type planItem struct {
	key       string // full object key in the bucket
	relPath   string // forward-slashed, relative to the set's root
	localPath string // absolute destination on disk, already safety-checked
	size      int64  // from the LIST response; used only for progress totals
}

// plan is the complete, fixed work list for a restore, built entirely from
// List() and local Lstat calls -- no object is fetched while building it.
// That ordering is what let's the download phase's progress bar start with
// a total that never moves afterward, the same reason backup finishes
// scanning and planning before a single byte transfers.
type plan struct {
	items      []planItem
	skipped    []string // relPaths left alone because they already exist and Overwrite is false
	failures   []Failure
	totalBytes int64
}

// buildPlan turns a LIST of "<prefix>/current/" into the fixed work list.
//
// Three things happen to every entry, in order: it is filtered by the
// --only pattern (cheapest check first); its key is turned into a local
// path through safeJoin, which is the only thing standing between a
// crafted or corrupted object key and a write outside target; and, unless
// Overwrite is set, an existing file at that path takes it out of the
// download list entirely rather than merely being downloaded and thrown
// away -- so an already-satisfied restore costs no bandwidth, the same
// "unchanged is free" principle backup applies on the upload side.
func buildPlan(entries []remote.ListEntry, cPrefix, target, only string, overwrite bool) *plan {
	p := &plan{}
	for _, e := range entries {
		relPath := strings.TrimPrefix(e.Key, cPrefix)
		if relPath == e.Key || relPath == "" {
			// Defensive: List(ctx, cPrefix) should never hand back a key
			// that doesn't carry the prefix it was asked to list, or the
			// prefix marker object itself. Refusing it is safer than
			// guessing at what it might mean.
			p.failures = append(p.failures, Failure{Key: e.Key, Err: fmt.Errorf("restore: key %q does not look like an object under %q", e.Key, cPrefix)})
			continue
		}
		if !matchesOnly(only, relPath) {
			continue
		}
		localPath, err := safeJoin(target, relPath)
		if err != nil {
			p.failures = append(p.failures, Failure{Key: relPath, Err: err})
			continue
		}
		if !overwrite {
			if _, err := os.Lstat(localPath); err == nil {
				p.skipped = append(p.skipped, relPath)
				continue
			}
		}
		p.items = append(p.items, planItem{key: e.Key, relPath: relPath, localPath: localPath, size: e.Size})
		p.totalBytes += e.Size
	}
	return p
}

// runSet performs a whole-set (or --only subset) restore: list, plan,
// download. See package doc for why the phases are strictly ordered.
func runSet(ctx context.Context, opts Options, prefix, target string, rep *Report, obs Observer, now func() time.Time, started time.Time) (*Report, error) {
	obs.Phase(PhaseListing, rep)
	cPrefix := currentPrefix(prefix)
	entries, err := opts.Client.List(ctx, cPrefix)
	if err != nil {
		return nil, fmt.Errorf("restore: list %q: %w", cPrefix, err)
	}
	rep.ListedFiles = int64(len(entries))
	for _, e := range entries {
		rep.ListedBytes += e.Size
	}

	obs.Phase(PhasePlanning, rep)
	p := buildPlan(entries, cPrefix, target, opts.Only, opts.Overwrite)
	rep.SkippedExisting = len(p.skipped)
	rep.Skipped = p.skipped
	rep.Failures = append(rep.Failures, p.failures...)

	if len(p.items) == 0 {
		// Nothing left to transfer: either the set is genuinely empty, the
		// --only pattern matched nothing, or everything is already present
		// with Overwrite off. None of those are errors.
		rep.Elapsed = now().Sub(started)
		obs.Phase(PhaseDone, rep)
		return rep, nil
	}

	obs.Phase(PhaseDownloading, rep)
	tracker := progress.New(p.totalBytes, int64(len(p.items)), now)

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

	res := runWorkers(ctx, opts.Client, p.items, opts.Workers, opts.Verify, tracker)
	close(stopTicker)
	<-tickerDone
	obs.Progress(tracker.Snapshot())

	rep.Downloaded = res.downloaded
	rep.Bytes = res.bytes
	rep.Verified = res.verified
	rep.VerifyMismatches = append(rep.VerifyMismatches, res.mismatches...)
	rep.Failures = append(rep.Failures, res.failures...)

	rep.Elapsed = now().Sub(started)
	obs.Phase(PhaseDone, rep)
	return rep, nil
}
