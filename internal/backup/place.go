package backup

import (
	"context"
	"fmt"
	"strings"
)

// checkPlace keeps the set's records true of the storage this run writes to.
//
// A record means "this file is already in storage". After backups start
// going somewhere else -- S3 becomes where backups go, a key file with other
// storage is imported, setup picks other storage -- the records still say
// that, but about the old storage, and a backup that trusted them would never
// send a file the new storage lacks until it changed on disk. So when the
// storage differs from the one the records were made against, this lists
// the set in storage once and forgets every record whose file is not there;
// the plan then sends just those. Files that are there are left alone, so a
// switch to storage that already holds a copy re-sends nothing.
//
// A set with no place recorded yet (an index from before this existed) is
// taken to match: checking would cost a listing of every set on the first
// run after updating, for storage that has not changed.
func checkPlace(ctx context.Context, opts Options) error {
	if opts.Place == "" {
		return nil
	}
	set := opts.Set
	was, err := opts.Index.Place(set.Name)
	if err != nil {
		return fmt.Errorf("read where this backup was made: %w", err)
	}
	if was == opts.Place {
		return nil
	}
	if was != "" {
		count, err := opts.Index.Count(set.Name)
		if err != nil {
			return fmt.Errorf("read the backup records: %w", err)
		}
		if count > 0 {
			if err := forgetMissing(ctx, opts); err != nil {
				return err
			}
		}
	}
	if err := opts.Index.SetPlace(set.Name, opts.Place); err != nil {
		return fmt.Errorf("record where this backup is made: %w", err)
	}
	return nil
}

// forgetMissing drops the records of files the storage does not hold.
func forgetMissing(ctx context.Context, opts Options) error {
	set := opts.Set
	scope := set.Prefix + "/current/"
	listed, err := opts.Client.List(ctx, scope)
	if err != nil {
		return fmt.Errorf("check what %s already holds: %w", opts.Client.Bucket(), err)
	}
	have := make(map[string]bool, len(listed))
	for _, o := range listed {
		if k, ok := strings.CutPrefix(o.Key, scope); ok {
			have[k] = true
		}
	}
	recs, err := opts.Index.All(set.Name)
	if err != nil {
		return fmt.Errorf("read the backup records: %w", err)
	}
	var gone []string
	for _, r := range recs {
		if !have[r.Key] {
			gone = append(gone, r.Key)
		}
	}
	if len(gone) == 0 {
		return nil
	}
	if err := opts.Index.DeleteMany(set.Name, gone); err != nil {
		return fmt.Errorf("forget %d files the storage lacks: %w", len(gone), err)
	}
	return nil
}
