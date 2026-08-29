// Package restore is the other half of r2backup: it turns a bucket back
// into a folder.
//
// "A backup that has not been restored has not been tested" is the whole
// design brief. Every choice below follows from taking that literally: the
// three phases mirror internal/backup's list-then-plan-then-transfer
// ordering so the progress bar is just as honest here as it is there
// (Run never starts the download phase until the total byte count is
// known and fixed); every object key -- even though this program is the
// only thing that has ever written one -- is treated as untrusted input
// before it is allowed to become a filesystem path, because that is the
// only way a rogue or corrupted key can be guaranteed to never write
// outside the restore target; and a symlink is restored as a symlink,
// never as a copy of whatever it happened to point at.
package restore

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"time"

	"github.com/saurabhhbansal/r2backup/internal/progress"
	"github.com/saurabhhbansal/r2backup/internal/remote"
	"github.com/saurabhhbansal/r2backup/internal/sets"
)

// Backend is what Run needs from the remote store. It is declared here,
// narrower than *remote.Client, purely so unit tests can exercise the
// planning and download logic (overwrite, glob filtering, cancellation,
// per-object failures) against a fake with no network and no MinIO
// server -- the same reason internal/trash declares its own Backend
// rather than depending on *remote.Client directly. *remote.Client
// already satisfies this.
type Backend interface {
	List(ctx context.Context, prefix string) ([]remote.ListEntry, error)
	Get(ctx context.Context, key string) (*remote.Object, error)
}

// Phase is which of the stages a run is in.
type Phase string

const (
	PhaseListing     Phase = "listing"
	PhasePlanning    Phase = "planning"
	PhaseDownloading Phase = "downloading"
	PhaseDone        Phase = "done"
)

// Failure records one object that could not be restored. A locked file, a
// permission error, a network blip, or a verify mismatch all land here;
// none of them abort the run -- the same rule backup.Report.Failures
// documents, for the same reason: one bad object must not cost the other
// sixty thousand.
type Failure struct {
	// Key is the path relative to the set's root, forward-slashed.
	Key string
	Err error
}

// Report is what one Run told the caller happened.
type Report struct {
	Set    string
	Target string

	ListedFiles int64
	ListedBytes int64

	Downloaded int
	Bytes      int64

	// SkippedExisting is how many objects were left alone because
	// Overwrite is false and something was already at that path. Skipped
	// carries their paths, so this is never a silent count -- the whole
	// point of tracking it is that "nothing happened here" is reported
	// exactly as loudly as everything else.
	SkippedExisting int
	Skipped         []string

	// Verified is how many downloaded files were hashed, re-read from
	// disk, and confirmed to match. Only meaningful when Options.Verify
	// was set.
	Verified int
	// VerifyMismatches are paths where the re-read did not match what was
	// written. This must never be empty silently: it is the one signal
	// that a restore looked successful but the bytes on disk are wrong.
	VerifyMismatches []string

	Failures []Failure

	Elapsed time.Duration
}

// Succeeded reports whether everything the run attempted actually landed
// correctly. A verify mismatch counts as failure even though the download
// itself reported success -- the whole reason Verify exists is to catch
// exactly that gap.
func (r *Report) Succeeded() bool {
	return len(r.Failures) == 0 && len(r.VerifyMismatches) == 0
}

// Observer receives phase changes and progress snapshots. It is called
// from Run's own goroutine, exactly like backup.Observer -- so a caller
// that also drives backup can share one implementation between the two.
type Observer interface {
	Phase(p Phase, r *Report)
	Progress(s progress.Snapshot)
}

type nopObserver struct{}

func (nopObserver) Phase(Phase, *Report)       {}
func (nopObserver) Progress(progress.Snapshot) {}

// Options configures one run.
type Options struct {
	Set    sets.Set
	Client Backend

	// Target overrides where the set is restored to. Required whenever
	// Set.Root does not exist on this machine -- see resolveTarget. Never
	// guessed at: a wrong guess here means writing tens of thousands of
	// files to the wrong place.
	Target string

	// Only restricts restore to paths matching this pattern, checked
	// against the forward-slashed path relative to the set's root. Empty
	// restores everything. See matchesOnly for the pattern language.
	Only string

	// SourceMachine restores another computer's copy of this set instead
	// of this one's own. Empty uses Set's own prefix. See sourcePrefix.
	SourceMachine string

	// Deleted, when non-empty, restores exactly this one relative path
	// from trash -- its most recent version -- instead of restoring the
	// whole set. This is what `r2backup restore --deleted <path>` asks
	// for. Only is ignored in this mode; SourceMachine still applies to
	// where the file's trash is looked up.
	Deleted string

	// Overwrite replaces a file that already exists at the destination.
	// The default, false, leaves it untouched and reports it skipped.
	Overwrite bool
	// Verify hashes each downloaded file, then re-reads it from disk and
	// compares, to catch a torn write or a truncated transfer that a bare
	// "the copy finished" would miss.
	Verify bool

	Observer Observer
	// ProgressEvery is how often Observer.Progress is called during the
	// download phase. Zero uses one second.
	ProgressEvery time.Duration
	// Workers bounds concurrent downloads. Zero uses 16.
	Workers int

	Now func() time.Time
}

// ErrNoTarget means the set's original folder is not on this machine and
// no --to was given.
//
// Returned rather than guessed at, on purpose: the two honest answers to
// "the original folder isn't here" -- ask the caller, or refuse -- are the
// only options that never risk restoring 60,000 files into the wrong
// directory. A scheduled or scripted restore has no one to ask, so it must
// refuse.
var ErrNoTarget = errors.New("restore: the set's original folder is not on this machine; a --to target is required")

// ErrNotInTrash means no version of the requested path was found under the
// set's trash.
var ErrNotInTrash = errors.New("restore: no trashed version of that path was found")

// Run restores a set: the whole thing, a --only subset, another machine's
// copy, or (via Options.Deleted) a single path recovered from trash.
func Run(ctx context.Context, opts Options) (*Report, error) {
	if opts.Client == nil {
		return nil, errors.New("restore: Options.Client is required")
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

	target, err := resolveTarget(opts.Set, opts.Target)
	if err != nil {
		return nil, err
	}
	prefix, err := sourcePrefix(opts.Set, opts.SourceMachine)
	if err != nil {
		return nil, err
	}

	rep := &Report{Set: opts.Set.Name, Target: target}

	if opts.Deleted != "" {
		return runDeleted(ctx, opts, prefix, target, rep, obs, now, started)
	}
	return runSet(ctx, opts, prefix, target, rep, obs, now, started)
}

// resolveTarget decides the local directory a restore writes into.
//
// It defaults to the set's own Root when that path exists on this
// machine -- the common case, restoring onto the same computer that made
// the backup, or one set up identically. Anything else must be given
// explicitly with Options.Target; this never falls back to a guess (a
// temp directory, the current directory, a sibling path) because a wrong
// guess here is indistinguishable from success until someone goes looking
// for their files.
func resolveTarget(set sets.Set, target string) (string, error) {
	if target != "" {
		abs, err := filepath.Abs(target)
		if err != nil {
			return "", fmt.Errorf("restore: resolve target %q: %w", target, err)
		}
		if err := os.MkdirAll(abs, 0o755); err != nil {
			return "", fmt.Errorf("restore: create target %q: %w", abs, err)
		}
		return abs, nil
	}
	if set.Root != "" {
		if info, err := os.Stat(set.Root); err == nil && info.IsDir() {
			return set.Root, nil
		}
	}
	return "", ErrNoTarget
}

// currentPrefix returns the "<prefix>/current/" object-key prefix
// everything live is stored under.
func currentPrefix(prefix string) string {
	return path.Join(prefix, "current") + "/"
}
