package restore

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/saurabhhbansal/r2backup/internal/progress"
	"github.com/saurabhhbansal/r2backup/internal/sets"
)

// sourcePrefix returns the bucket prefix objects should be read from: the
// set's own prefix by default, or another machine's copy of the same set
// when a different machine is named.
//
// A set's Prefix is built as "machines/<machine>/<name>" (see
// sets.Set.Machine and internal/backup/backup_test.go's harness, the only
// place that shape is written down so far). Swapping just the machine
// segment is what lets one computer read a backup another computer made of
// the identically-named set. A prefix that was not built that way cannot
// be mapped this way, so this refuses rather than guessing at some other
// layout -- restoring from the wrong prefix because of a guessed
// convention would silently show the wrong computer's files as if they
// were the requested one's.
func sourcePrefix(set sets.Set, machine string) (string, error) {
	if machine == "" || machine == set.Machine {
		return set.Prefix, nil
	}
	parts := strings.SplitN(set.Prefix, "/", 3)
	if len(parts) != 3 || parts[0] != "machines" || parts[1] != set.Machine {
		return "", fmt.Errorf("restore: cannot find %q's copy of %q: prefix %q is not in machines/<machine>/<name> form",
			machine, set.Name, set.Prefix)
	}
	return path.Join("machines", machine, parts[2]), nil
}

// trashSuffixRE mirrors internal/trash/keys.go's disambiguator exactly:
// "<name>~<HHMMSS>-<hex>[.ext]". The two must be kept in step, the same
// way internal/index.Kind is deliberately kept in step with scan.Kind
// rather than importing it (see index.go) -- this package does not import
// internal/trash itself because everything restore needs from it (find
// the newest trashed version of one path) is a single LIST call, which
// does not warrant depending on trash.Backend's Copy/Delete/DeleteBatch
// methods that this package would never call.
var trashSuffixRE = regexp.MustCompile(`^(.*)~(\d{6})-([0-9a-f]+)(\.[^.]*)?$`)

const trashDateLayout = "2006-01-02"

// findInTrash locates the most recent trashed version of relPath under
// prefix, returning its full object key and size.
func findInTrash(ctx context.Context, client Backend, prefix, relPath string) (key string, size int64, err error) {
	root := path.Join(prefix, "trash") + "/"
	entries, err := client.List(ctx, root)
	if err != nil {
		return "", 0, fmt.Errorf("restore: list %q: %w", root, err)
	}

	type candidate struct {
		key, date, hhmmss string
		size              int64
	}
	var candidates []candidate
	for _, e := range entries {
		rest := strings.TrimPrefix(e.Key, root)
		slash := strings.Index(rest, "/")
		if slash < 0 {
			continue // an object directly under trash/, not trash/<date>/...
		}
		date := rest[:slash]
		if _, err := time.Parse(trashDateLayout, date); err != nil {
			continue
		}
		underDate := rest[slash+1:]
		dir, base := path.Split(underDate)

		var original, hhmmss string
		if m := trashSuffixRE.FindStringSubmatch(base); m != nil {
			original = path.Join(dir, m[1]+m[4])
			hhmmss = m[2]
		} else {
			// Not a key this format's disambiguator produced -- report it
			// at face value rather than dropping it, the same tolerance
			// trash.parseTrashKey applies to a foreign object under
			// trash/.
			original = underDate
		}
		if original != relPath {
			continue
		}
		candidates = append(candidates, candidate{key: e.Key, date: date, hhmmss: hhmmss, size: e.Size})
	}
	if len(candidates) == 0 {
		return "", 0, fmt.Errorf("restore: %q: %w", relPath, ErrNotInTrash)
	}
	// The date directory only has day resolution; the embedded HHMMSS
	// breaks ties within a day the same way trash/move.go's disambiguator
	// intends it to, so this always recovers the version that was trashed
	// last rather than an arbitrary one of several from the same day.
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].date != candidates[j].date {
			return candidates[i].date < candidates[j].date
		}
		return candidates[i].hhmmss < candidates[j].hhmmss
	})
	newest := candidates[len(candidates)-1]
	return newest.key, newest.size, nil
}

// runDeleted restores exactly one path recovered from trash -- what
// `r2backup restore --deleted <path>` asks for.
func runDeleted(ctx context.Context, opts Options, prefix, target string, rep *Report, obs Observer, now func() time.Time, started time.Time) (*Report, error) {
	relPath := opts.Deleted

	obs.Phase(PhaseListing, rep)
	key, size, err := findInTrash(ctx, opts.Client, prefix, relPath)
	if err != nil {
		return nil, err
	}
	rep.ListedFiles = 1
	rep.ListedBytes = size

	obs.Phase(PhasePlanning, rep)
	localPath, err := safeJoin(target, relPath)
	if err != nil {
		return nil, err
	}
	if !opts.Overwrite {
		if _, err := os.Lstat(localPath); err == nil {
			rep.SkippedExisting = 1
			rep.Skipped = []string{relPath}
			rep.Elapsed = now().Sub(started)
			obs.Phase(PhaseDone, rep)
			return rep, nil
		}
	}

	obs.Phase(PhaseDownloading, rep)
	tracker := progress.New(size, 1, now)
	item := planItem{key: key, relPath: relPath, localPath: localPath, size: size}
	n, verified, err := processItem(ctx, opts.Client, item, opts.Verify, tracker)
	obs.Progress(tracker.Snapshot())

	if err != nil {
		rep.Failures = []Failure{{Key: relPath, Err: err}}
		if errors.Is(err, errVerifyMismatch) {
			rep.VerifyMismatches = []string{relPath}
		}
	} else {
		rep.Downloaded = 1
		rep.Bytes = n
		if verified {
			rep.Verified = 1
		}
	}

	rep.Elapsed = now().Sub(started)
	obs.Phase(PhaseDone, rep)
	return rep, nil
}
