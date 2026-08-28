package trash

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Entry is one object recoverable from trash.
type Entry struct {
	// RelPath is the path it lived at under current/ before it was
	// trashed.
	RelPath string
	// TrashKey is the full key it is sitting at right now -- what Restore
	// needs to bring it back.
	TrashKey string
	// TrashedOn is the calendar day it was moved into trash (UTC
	// midnight), the same granularity the bucket layout itself uses, not
	// the exact moment it happened.
	TrashedOn time.Time
	// ExpiresOn is the day Prune becomes eligible to remove it, given the
	// retentionDays passed to List.
	ExpiresOn time.Time
	// Size is the object's size in bytes.
	Size int64
}

// List reports everything recoverable from a set's trash: original path,
// when it was trashed, when it expires, and its size -- what backs
// `r2backup trash ls`.
//
// retentionDays is the set's current setting (sets.Set.RetentionDays). It
// is used only to compute each entry's ExpiresOn; it does not filter what
// List returns. Changing a set's retention is then reflected the next
// time `trash ls` runs, without touching a single stored object -- the
// expiry date is a projection made at read time, not something written
// down when the object was trashed.
func (t *Trash) List(ctx context.Context, prefix string, retentionDays int) ([]Entry, error) {
	root := prefix + "/" + trashDir + "/"
	raw, err := t.backend.List(ctx, root)
	if err != nil {
		return nil, fmt.Errorf("trash: list %q: %w", prefix, err)
	}

	entries := make([]Entry, 0, len(raw))
	for _, e := range raw {
		if strings.Contains(e.Key, "/"+currentDir+"/") {
			continue
		}
		date, relPath, ok := parseTrashKey(prefix, e.Key)
		if !ok {
			continue
		}
		d, err := time.Parse(dateLayout, date)
		if err != nil {
			continue
		}
		entries = append(entries, Entry{
			RelPath:   relPath,
			TrashKey:  e.Key,
			TrashedOn: d,
			ExpiresOn: d.AddDate(0, 0, retentionDays),
			Size:      e.Size,
		})
	}
	sort.Slice(entries, func(i, j int) bool {
		if !entries[i].TrashedOn.Equal(entries[j].TrashedOn) {
			return entries[i].TrashedOn.Before(entries[j].TrashedOn)
		}
		return entries[i].RelPath < entries[j].RelPath
	})
	return entries, nil
}
