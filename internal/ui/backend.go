package ui

import (
	"context"
	"time"

	"github.com/saurabhhbansal/r2backup/internal/progress"
	"github.com/saurabhhbansal/r2backup/internal/scan"
)

// SetView is one backed-up folder, flattened into exactly what the screen
// draws. The UI holds no sets.Set, no runstate.Past and no index handle: it
// renders this and nothing else, which is what lets every screen in this
// package be tested with a literal instead of a bucket.
type SetView struct {
	Name      string
	Root      string
	Prefix    string
	Excludes  []string
	Retention int
	Objects   int

	// State is the one word shown beside the name: "ok", "never run",
	// "failed" or "needs attention".
	State string
	// Note explains State when it is not "ok" -- the status note left by a
	// run that stopped, or the error from one that failed.
	Note string

	LastRun    time.Time
	HasRun     bool
	Uploaded   int
	Unchanged  int
	Deleted    int
	Moved      int
	Bytes      int64
	Operations int
	Failures   int
	Problems   int
	Collisions int
	Examples   []string
}

// Overview is the machine-wide state drawn in the header and footer.
type Overview struct {
	Machine string
	Bucket  string
	Version string

	OpsUsed    int
	OpsLimit   int
	OpsResetAt time.Time

	Scheduled bool
	Interval  time.Duration
	NextRun   time.Time
	LastRun   time.Time
	// RunsWhenSignedOut is false both when the task genuinely only runs
	// while signed in and when the platform cannot say, so the Schedule tab
	// uses it to soften a claim rather than to make one.
	RunsWhenSignedOut bool
	// SchedulerAvailable is false on a platform with no scheduler at all.
	SchedulerAvailable bool

	// Configured is false until this machine has R2 credentials. Nothing
	// else in the interface works without them.
	Configured bool

	// Running names a backup in progress right now, including one the OS
	// scheduler started in another process. Empty when nothing is running.
	Running    string
	RunPercent float64
	RunETA     string
}

// AccountView is the Account tab's state.
type AccountView struct {
	SignedIn bool
	Email    string
	// VaultStored reports whether credentials are saved for other machines
	// to pick up.
	VaultStored bool
	Devices     []DeviceView
	// Reachable is false when the account service could not be contacted.
	// The tab says so rather than presenting "not signed in", which is a
	// different thing and leads somewhere different.
	Reachable bool
	Err       string
}

// DeviceView is one computer signed in to the account.
type DeviceView struct {
	Name     string
	OS       string
	LastSeen time.Time
	// This reports whether the row is this computer.
	This bool
}

// TrashRow is one recoverable object.
type TrashRow struct {
	Key     string
	Size    int64
	Deleted time.Time
	Expires time.Time
}

// AddRequest is a new folder to back up.
type AddRequest struct {
	Name      string
	Root      string
	Excludes  []string
	Retention int
}

// RestoreRequest is one restore, whole or partial.
type RestoreRequest struct {
	Set string
	// To is where it goes. Empty means the folder's original path, which
	// only works when that path exists on this machine.
	To string
	// Only narrows a whole-set restore to matching paths.
	Only string
	// Deleted names one file to recover from trash instead of restoring
	// the whole set.
	Deleted string
	// Machine restores another computer's copy.
	Machine   string
	Overwrite bool
	Verify    bool
}

// RestoreResult is what a finished restore did.
type RestoreResult struct {
	Files   int
	Bytes   int64
	Target  string
	Skipped int
	Failed  int
}

// Keys are the four values that make an R2 connection.
type Keys struct {
	AccountID   string
	AccessKeyID string
	Secret      string
	Bucket      string
}

// Backend is everything the interface needs from the rest of the program.
//
// It is deliberately the whole command surface, not a subset. The first
// version of this interface could show you what was backed up and start a
// backup, and for anything else it closed itself and told you to type a
// command -- which is the thing it exists to spare people. Every method here
// is one that used to be an instruction to go and use the command line.
//
// It stays an interface rather than a direct dependency on internal/cli's app
// so the screens can be driven in tests without credentials, a bucket or a
// scheduler, and so this package cannot grow a second copy of logic that
// already sits behind a command.
type Backend interface {
	// Load reads current state. Called on open, on refresh, and once a
	// second to pick up a scheduled run's progress -- so it must not touch
	// the network, or an idle window would spend operations all day.
	Load(ctx context.Context) ([]SetView, Overview, error)

	// Backup runs one set, reporting progress as it goes.
	Backup(ctx context.Context, name string, phase func(string), snap func(progress.Snapshot)) error

	// Scan walks a folder so the picker has a tree to show.
	Scan(ctx context.Context, root string) (*scan.Result, error)

	// Add registers a new folder. It does not back it up; the caller does
	// that next, so the progress is visible.
	Add(ctx context.Context, req AddRequest) error

	// Overlaps names another folder already covering root.
	//
	// Overlapping folders are allowed -- each carries its own retention -- but
	// every file in the overlap is stored under two prefixes and paid for
	// twice on every run that touches it. `r2b add` says so once, and on a
	// tool whose whole argument is the operations budget the interface has to
	// as well.
	Overlaps(root string) (string, bool)

	// SetExcludes changes what a set includes.
	SetExcludes(ctx context.Context, name string, excludes []string) error

	// Restore brings a folder, or one deleted file, back.
	Restore(ctx context.Context, req RestoreRequest, phase func(string), snap func(progress.Snapshot)) (RestoreResult, error)

	Rename(ctx context.Context, from, to string) error
	Relink(ctx context.Context, name, newRoot string) error

	// Trash lists what is recoverable for one set. This reaches the network,
	// which is why it happens on a keystroke and never on a timer.
	Trash(ctx context.Context, name string) ([]TrashRow, error)

	// Remove stops backing up a folder. purge also deletes what is stored.
	Remove(ctx context.Context, name string, purge bool) error

	// Schedule registers or unregisters automatic runs.
	Schedule(ctx context.Context, everyMinutes int, off bool) error

	// Account reads the sign-in state. It reaches the network, so it is
	// called when the Account tab is opened, not on the refresh timer.
	Account(ctx context.Context) (AccountView, error)
	// SignInStart mails a six-digit code.
	SignInStart(ctx context.Context, email string) error
	// SignInVerify exchanges the code for a session and saves it.
	SignInVerify(ctx context.Context, email, code string) error
	SignOut(ctx context.Context) error
	// UnlockVault pulls the stored R2 credentials and saves them here.
	UnlockVault(ctx context.Context, password string) error
	// StoreVault encrypts this machine's credentials for the next one.
	StoreVault(ctx context.Context, password string) error
	// SaveKeys stores R2 credentials typed in directly, after checking they
	// reach the bucket.
	SaveKeys(ctx context.Context, k Keys) error

	// CheckUpdate returns the newer version available, or "" for none.
	CheckUpdate(ctx context.Context) (string, error)
	// ApplyUpdate downloads, verifies and swaps in the newest release.
	ApplyUpdate(ctx context.Context) (string, error)
}
