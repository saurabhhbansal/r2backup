package trash

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
)

// PruneResult is what a Prune call did.
type PruneResult struct {
	// DatesPruned are the trash date directories removed, oldest first.
	DatesPruned []string
	// KeysDeleted is the number of trashed objects removed.
	KeysDeleted int
	// ClassAOps is the number of ListObjectsV2 calls Prune actually made
	// while discovering what to delete. DeleteObjects/DeleteObject are
	// free on R2, so they add nothing here regardless of how many keys
	// were removed.
	ClassAOps int
}

// Prune permanently removes every trash date directory strictly older
// than retentionDays, measured against the clock's "now" truncated to a
// calendar day. A date exactly retentionDays old is kept -- it has not
// yet finished its retention window -- and only a date older than that is
// removed; Prune run on consecutive days is expected to eventually catch
// every date exactly once as it crosses that boundary.
//
// It works from one List of the whole <prefix>/trash/ tree rather than
// one List per date directory: the Backend this package depends on lists
// recursively with no delimiter support (mirroring remote.Client.List),
// so there is no cheaper way to discover which dates even exist. That
// List is Prune's only Class A cost -- see PruneResult.ClassAOps.
//
// Prune never deletes anything under <prefix>/current/. It only acts on
// keys it derived from that one List call under <prefix>/trash/, and as a
// second, independent check it skips any key found to contain
// "/current/" before that key is ever handed to Delete or DeleteBatch --
// the live mirror is the one thing this whole package exists to protect,
// and a bug that let a stray current/-shaped key slip through the date
// filter must not also slip past this guard.
func (t *Trash) Prune(ctx context.Context, prefix string, retentionDays int) (PruneResult, error) {
	if retentionDays <= 0 {
		return PruneResult{}, nil
	}
	if err := ctx.Err(); err != nil {
		return PruneResult{}, fmt.Errorf("trash: prune: %w", err)
	}

	cutoff := t.clock.now().UTC().Truncate(24*time.Hour).AddDate(0, 0, -retentionDays)

	root := prefix + "/" + trashDir + "/"
	entries, err := t.backend.List(ctx, root)
	if err != nil {
		return PruneResult{}, fmt.Errorf("trash: prune %q: list trash: %w", prefix, err)
	}
	// remote.List paginates internally at 1,000 keys per ListObjectsV2
	// call but does not report how many pages that took, so this is a
	// ceiling derived from the key count, not a measured call count.
	classAOps := (len(entries) + 999) / 1000
	if classAOps == 0 {
		classAOps = 1 // List still makes one call even over an empty prefix.
	}

	byDate := make(map[string][]string)
	for _, e := range entries {
		if strings.Contains(e.Key, "/"+currentDir+"/") {
			continue // never let a live-tree key reach the delete path
		}
		date, _, _, ok := parseTrashKey(prefix, e.Key)
		if !ok {
			continue
		}
		d, err := time.Parse(dateLayout, date)
		if err != nil {
			continue
		}
		if !d.Before(cutoff) {
			continue // within retention: keep it
		}
		byDate[date] = append(byDate[date], e.Key)
	}

	dates := make([]string, 0, len(byDate))
	for date := range byDate {
		dates = append(dates, date)
	}
	sort.Strings(dates)

	res := PruneResult{ClassAOps: classAOps}
	for _, date := range dates {
		if err := ctx.Err(); err != nil {
			return res, fmt.Errorf("trash: prune %q: %w", prefix, err)
		}
		keys := byDate[date]
		if err := t.backend.DeleteBatch(ctx, keys); err != nil {
			return res, fmt.Errorf("trash: prune %q: delete %s: %w", prefix, date, err)
		}
		res.DatesPruned = append(res.DatesPruned, date)
		res.KeysDeleted += len(keys)
	}
	return res, nil
}
