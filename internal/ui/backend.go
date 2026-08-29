package ui

import (
	"context"
	"time"

	"github.com/saurabhhbansal/r2backup/internal/progress"
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

	OpsUsed    int
	OpsLimit   int
	OpsResetAt time.Time

	Scheduled bool
	Interval  time.Duration

	// Running names a backup in progress right now, including one the OS
	// scheduler started in another process. Empty when nothing is running.
	Running    string
	RunPercent float64
	RunETA     string
}

// TrashRow is one recoverable object.
type TrashRow struct {
	Key     string
	Size    int64
	Deleted time.Time
	Expires time.Time
}

// Backend is everything the interface needs from the rest of the program.
//
// It is an interface rather than a direct dependency on internal/cli's app so
// that the screens can be driven in a test without credentials, a bucket or a
// scheduler -- and so that this package cannot quietly grow a second copy of
// any logic that already exists behind a command.
type Backend interface {
	// Load reads current state. Called on open and on refresh, and it must
	// not touch the network: it is also called every second to pick up a
	// scheduled run's progress, and a per-second network call would cost
	// operations for as long as the window is open.
	Load(ctx context.Context) ([]SetView, Overview, error)

	// Backup runs one set, reporting progress as it goes. phase receives the
	// human name of each stage; snap receives a fresh snapshot roughly once a
	// second.
	Backup(ctx context.Context, name string, phase func(string), snap func(progress.Snapshot)) error

	// Trash lists what is recoverable for one set. This one does reach the
	// network, which is why it happens on a keystroke and never on a timer.
	Trash(ctx context.Context, name string) ([]TrashRow, error)

	// Remove stops backing up a folder. purge also deletes what is stored.
	Remove(ctx context.Context, name string, purge bool) error

	// Schedule registers or unregisters automatic runs.
	Schedule(ctx context.Context, everyMinutes int, off bool) error
}

// ActionKind is a request the interface cannot serve by itself.
type ActionKind int

const (
	// ActionNone means the user simply quit.
	ActionNone ActionKind = iota
	// ActionAdd asks for `add`, which opens the folder picker.
	ActionAdd
	// ActionEdit asks for `edit` on Action.Set.
	ActionEdit
	// ActionRestore asks for `restore` on Action.Set.
	ActionRestore
)

// Action is what Run returns.
//
// Adding, editing and restoring all need a second full-screen program (the
// folder picker) or a line of typed input, and running one bubbletea program
// inside another is a reliable way to end up with two things reading the same
// terminal. So the interface closes, the command runs on a clean terminal the
// way it always has, and the interface reopens -- the same handoff a git UI
// makes when it hands you your editor.
type Action struct {
	Kind ActionKind
	Set  string
	// Path is the folder for ActionAdd, or the restore destination.
	Path string
}
