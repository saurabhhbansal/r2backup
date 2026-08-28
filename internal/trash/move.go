package trash

import (
	"context"
	"fmt"
	"sync"
)

// moveConcurrency bounds how many CopyObject calls Move has in flight at
// once. A single run can trash thousands of changed objects; unbounded
// concurrency would open that many simultaneous requests and, against
// R2/S3, tends to trade throughput for a wall of throttling retries
// rather than gain anything.
const moveConcurrency = 16

// MovedEntry records where one live object ended up in trash.
type MovedEntry struct {
	// RelPath is the path relative to current/, unchanged.
	RelPath string
	// TrashKey is the full object key it was copied to.
	TrashKey string
}

// MoveResult is what a Move call did.
type MoveResult struct {
	Moved []MovedEntry
	// ClassAOps is the number of CopyObject calls actually made -- zero
	// when retention is disabled, since Move is then a no-op.
	ClassAOps int
}

// Move copies each of relPaths, found live at <prefix>/current/<relPath>,
// aside to its dated trash key with a server-side Copy. It does not
// delete or overwrite the source -- that stays the caller's job,
// ordinarily done immediately after by whichever sync operation is about
// to replace or remove the file. Trash's role is only to make sure the
// old version survives that; deciding when the source itself changes is
// the sync engine's business, not this package's.
//
// retentionDays comes from the owning set (sets.Set.RetentionDays). A
// value of 0 disables trash for that set entirely -- some sets are pure
// build output, and keeping 30 days of it is pure waste -- so Move does
// nothing and reports zero operations.
func (t *Trash) Move(ctx context.Context, prefix string, relPaths []string, retentionDays int) (MoveResult, error) {
	if retentionDays <= 0 || len(relPaths) == 0 {
		return MoveResult{}, nil
	}
	if err := ctx.Err(); err != nil {
		return MoveResult{}, fmt.Errorf("trash: move: %w", err)
	}

	now := t.clock.now()

	type outcome struct {
		entry MovedEntry
		err   error
	}
	results := make([]outcome, len(relPaths))

	sem := make(chan struct{}, moveConcurrency)
	var wg sync.WaitGroup
	for i, relPath := range relPaths {
		i, relPath := i, relPath
		sem <- struct{}{} // blocks once moveConcurrency copies are in flight
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()

			src := liveKey(prefix, relPath)
			dst := buildTrashKey(prefix, relPath, now)
			if err := t.backend.Copy(ctx, src, dst); err != nil {
				results[i] = outcome{err: fmt.Errorf("trash: move %q: %w", relPath, err)}
				return
			}
			results[i] = outcome{entry: MovedEntry{RelPath: relPath, TrashKey: dst}}
		}()
	}
	wg.Wait()

	res := MoveResult{Moved: make([]MovedEntry, 0, len(relPaths))}
	var firstErr error
	for _, o := range results {
		if o.err != nil {
			if firstErr == nil {
				firstErr = o.err
			}
			continue
		}
		res.Moved = append(res.Moved, o.entry)
		res.ClassAOps++
	}
	return res, firstErr
}

// EstimateMoveOps returns how many Class A operations
// Move(ctx, prefix, relPaths, retentionDays) will spend: one CopyObject
// per key, or zero when retention is disabled. Unlike Prune's estimate,
// this one is exact rather than a floor -- Move's entire cost is "one
// Copy per key it is asked to move", so there is nothing left to discover
// before running it, which is what lets a caller show this figure against
// the free tier before spending it rather than only after.
func EstimateMoveOps(relPaths []string, retentionDays int) int {
	if retentionDays <= 0 {
		return 0
	}
	return len(relPaths)
}
