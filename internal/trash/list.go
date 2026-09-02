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
	// TrashedOn is when it was moved into trash. For anything Move put
	// there, the trash key's disambiguator carries the HHMMSS of that
	// move, so this is the exact moment, to the second -- see
	// TrashedOnExact. For a foreign object someone dropped straight into
	// trash/<date>/... without going through Move, no time of day was
	// ever recorded, and TrashedOn falls back to UTC midnight on the day
	// it was found under.
	TrashedOn time.Time
	// TrashedOnExact reports whether TrashedOn is a real, recorded time
	// of day (true) or only the day-resolution fallback at UTC midnight
	// (false). A caller formatting TrashedOn for display must check this
	// first -- printing a clock time when it is false would show a
	// specific-looking moment that was never measured.
	TrashedOnExact bool
	// ExpiresOn is the day Prune becomes eligible to remove it, given the
	// retentionDays passed to List. It is deliberately day-resolution
	// only: Prune's cutoff comparison works in whole days, so a time of
	// day here would itself be a fabrication -- there is no measured
	// clock time backing it the way there sometimes is for TrashedOn.
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
		date, relPath, timeOfDay, ok := parseTrashKey(prefix, e.Key)
		if !ok {
			continue
		}
		d, err := time.Parse(dateLayout, date)
		if err != nil {
			continue
		}
		// Default to the day-resolution fallback, then upgrade to the
		// real moment when the key's disambiguator actually carries one.
		// A malformed HHMMSS should not happen -- buildTrashKey is the
		// only writer of this shape -- but if it ever did, falling back
		// rather than erroring keeps a listing failure from taking down
		// the whole trash view over one odd key.
		trashedOn, exact := d, false
		if timeOfDay != "" {
			if withTime, err := time.Parse(dateLayout+"150405", date+timeOfDay); err == nil {
				trashedOn, exact = withTime, true
			}
		}
		entries = append(entries, Entry{
			RelPath:        relPath,
			TrashKey:       e.Key,
			TrashedOn:      trashedOn,
			TrashedOnExact: exact,
			ExpiresOn:      d.AddDate(0, 0, retentionDays),
			Size:           e.Size,
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
