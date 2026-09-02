package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"runtime"
	"sync"
	"time"

	"github.com/saurabhhbansal/r2backup/internal/backup"
	"github.com/saurabhhbansal/r2backup/internal/config"
	"github.com/saurabhhbansal/r2backup/internal/creds"
	"github.com/saurabhhbansal/r2backup/internal/index"
	"github.com/saurabhhbansal/r2backup/internal/progress"
	"github.com/saurabhhbansal/r2backup/internal/runstate"
	"github.com/saurabhhbansal/r2backup/internal/schedule"
	"github.com/saurabhhbansal/r2backup/internal/sets"
	"github.com/saurabhhbansal/r2backup/internal/trash"
	"github.com/saurabhhbansal/r2backup/internal/ui"
)

// dashboard implements ui.Backend on top of the same packages every command
// uses.
//
// It holds the app open for the whole session, and that is not an
// optimisation. Every method used to call openApp() itself, which opened the
// bbolt index -- and bbolt takes a file lock that contends with itself inside
// one process. A backup holds the index for its whole run, the interface
// reloads once a second, and that reload blocked for bbolt's five-second
// timeout and then failed with `open index at "...": timeout`. The progress
// screen vanished mid-backup, replaced by an error about a file, and the
// model then believed nothing was running -- so a second backup could be
// started on top of the first. Measured: a second openApp in the same process
// returns an error after 4.99s.
//
// One handle, opened once, fixed that -- and broke something else: this
// dashboard is a long-lived process that sits open far more than it is doing
// anything, and holding the one handle open for the session's whole length
// meant holding bbolt's exclusive OS file lock for that whole length too. A
// scheduled backup, or `status`, or `ls`, run from a second r2b process while
// a window merely happened to be open -- nobody backing up, nobody looking at
// a tab that reads the index -- got the same 4.98s timeout this comment used
// to be about, for no reason related to actual contention.
//
// So app.index (see app.go) is not a single already-open database any more;
// it is a handle that opens on demand and gives up the lock once nobody is
// using it, checked out through app.checkoutIndex around each operation --
// Load, Backup, Remove, Rename, Objects, and so on. This keeps the property
// that made the first fix work: every concurrent in-process user still shares
// the one *index.DB object and the one open bbolt handle bbolt allows,
// because app.index is created once and never replaced, so it never contends
// with itself. What changes is that the handle is closed, and the OS lock let
// go, in the gaps between checkouts -- in particular between the once-a-
// second Load calls, and for the whole time the window is sitting idle with
// nobody driving it. See index.DB's Acquire and Release for the mechanics.
//
// bbolt is safe for concurrent use through a single handle and sets.Store
// carries its own RWMutex; connect is the one piece of shared state that is
// not, so it has a mutex here.
//
// Each method goes through the same package the matching command goes through
// -- backup.Run, restore.Run, sets.Store, account.Client -- but not through
// the command's own function, because those write to a terminal. Where a
// command does something around the call that matters (parking a moved
// folder, refusing a restore that matched nothing), that is reproduced here
// and named in a comment, because nothing else will catch it drifting.
type dashboard struct {
	opts *Options
	app  *app

	// mu guards lazily building the R2 client, and also the schedule cache
	// just below: Load runs on the UI loop, a transfer runs on a worker, and
	// pressing the schedule toggle runs on whichever goroutine the UI framework
	// dispatches it to, so more than one of these can reach either piece of
	// shared state at once.
	mu sync.Mutex

	// schedule.Current shells out to the OS scheduler -- systemctl --user
	// show on Linux, two schtasks /query on Windows, launchctl list on macOS
	// -- and Load runs on the UI's once-a-second tick for as long as a
	// window is open. scheduleState/scheduleErr/scheduleAt cache the last
	// result so that tick asks the OS once per scheduleCacheTTL instead of
	// once a second; currentSchedule reads this and invalidateScheduleCache
	// clears it. Schedule and RepairSchedule call invalidateScheduleCache
	// whenever they actually change what's registered, so pressing the
	// toggle shows up on the very next Load rather than after however much
	// of the window happens to be left.
	scheduleState schedule.Status
	scheduleErr   error
	scheduleAt    time.Time

	// now stands in for time.Now so the schedule cache's expiry can be
	// tested without sleeping. Same pattern as index.DB's Now field, and for
	// the same reason: set it (if at all) once, before any concurrent use
	// begins.
	now func() time.Time
}

// scheduleCacheTTL is how long a schedule.Current result is trusted before
// Load shells out to the OS scheduler again on its own account. Chosen to be
// short enough that the Schedule tab still feels current, and long enough
// that an idle dashboard isn't spawning an OS process (two, on Windows)
// every second the window is open.
const scheduleCacheTTL = 30 * time.Second

// openDashboard opens the state the interface needs, once.
func openDashboard(opts *Options) (*dashboard, error) {
	a, err := openApp()
	if err != nil {
		return nil, err
	}
	return &dashboard{opts: opts, app: a, now: time.Now}, nil
}

func (d *dashboard) close() {
	if d.app != nil {
		d.app.close()
	}
}

// connected returns the app with an R2 client attached.
func (d *dashboard) connected(ctx context.Context) (*app, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.app.connect(ctx); err != nil {
		return nil, err
	}
	return d.app, nil
}

// scheduleName is the OS scheduler entry's identity, spelled once here rather
// than at each call site. It is deliberately still "r2backup": an installed
// machine already has a task under that name, and changing it would leave the
// old one running and the new one duplicating it.
const scheduleName = "r2backup"

// currentSchedule returns the OS scheduler's status for scheduleName,
// answering from the cache described on the dashboard struct when it is
// still within scheduleCacheTTL and shelling out through scheduleCurrent
// (commands.go's seam onto schedule.Current) only when it isn't. Every
// caller -- Load on its once-a-second tick, and RepairSchedule reading back
// what to reinstall -- goes through here rather than schedule.Current
// directly, so there is exactly one place that decides whether this call is
// worth making again.
func (d *dashboard) currentSchedule() (schedule.Status, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	now := d.now()
	if !d.scheduleAt.IsZero() && now.Sub(d.scheduleAt) < scheduleCacheTTL {
		return d.scheduleState, d.scheduleErr
	}
	st, err := scheduleCurrent(scheduleName)
	d.scheduleState, d.scheduleErr, d.scheduleAt = st, err, now
	return st, err
}

// invalidateScheduleCache discards the cached schedule.Current result, so
// the next call to currentSchedule asks the OS again instead of repeating
// whatever was true before this call. Schedule and RepairSchedule call this
// right after actually installing, removing or repairing the entry -- see
// the dashboard struct's comment on scheduleState for why that matters.
func (d *dashboard) invalidateScheduleCache() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.scheduleAt = time.Time{}
}

func (d *dashboard) Load(ctx context.Context) ([]ui.SetView, ui.Overview, error) {
	a := d.app

	// Checked out for this one refresh and given back at the end of it --
	// not held between ticks. See the dashboard struct's comment for why
	// that gap matters: it is what lets a second r2b process get bbolt's
	// lock in the second between this Load and the next one.
	idx, release, err := a.checkoutIndex()
	if err != nil {
		return nil, ui.Overview{}, err
	}
	defer release()

	histPath, _ := historyPath()
	hist, _ := runstate.ReadHistory(histPath)

	list := a.sets.List()
	views := make([]ui.SetView, 0, len(list))
	for _, s := range list {
		v := ui.SetView{
			Name: s.Name, Root: s.Root, Prefix: s.Prefix,
			Excludes: s.Excludes, Retention: s.RetentionDays,
			State: "ok",
		}
		if n, err := idx.Count(s.Name); err == nil {
			v.Objects = n
		}
		if last, ok := hist.Last(s.Name); ok {
			v.HasRun, v.LastRun = true, last.FinishedAt
			v.Uploaded, v.Unchanged = last.Uploaded, last.Unchanged
			v.Deleted, v.Moved, v.Bytes = last.Deleted, last.Moved, last.Bytes
			v.Operations = last.Operations
			v.Failures, v.Problems, v.Collisions = last.Failures, last.Problems, last.Collisions
			v.Examples = last.Examples
			if last.Cancelled {
				// Same distinction printStatus makes for `status`: a run
				// someone stopped on purpose is not "failed", and
				// last.Error here is only backup.ErrCancelled's text -- not
				// worth showing verbatim when a clearer, fixed Note says
				// the same thing.
				v.State, v.Note = "cancelled", "stopped before it finished"
			} else if last.Error != "" {
				v.State, v.Note = "failed", last.Error
			}
		} else {
			v.State = "never run"
		}
		// Parked wins over failed, and is checked last for that reason. They
		// arrive together -- a run against a folder that has moved both fails
		// and parks the set -- and "failed" reads as something that might
		// come right on its own, which this will not. It needs a person.
		if s.Status == sets.StatusNeedsAttention {
			v.State = "needs attention"
			if s.StatusNote != "" {
				v.Note = s.StatusNote
			}
		}
		views = append(views, v)
	}

	ov := ui.Overview{Machine: machineName(), Version: Version, OpsLimit: index.FreeTierOpsPerMonth}
	// Read from the stored credentials rather than by connecting: Load runs
	// once a second, and this must stay free.
	if c, err := a.creds.Load(); err == nil {
		ov.Bucket = c.Bucket
		// Configured is what the Folders and Account tabs gate on, and it has
		// to mean the same thing connect() would check: a credential set that
		// is actually usable, not merely a file that exists. A bucket left
		// blank -- half-finished setup, or a vault entry that arrived without
		// one -- reads as configured today with Bucket empty, and every
		// caller of Load would have to know to check for that too.
		ov.Configured = c.Valid() == nil
	} else if !errors.Is(err, creds.ErrNotFound) {
		return views, ov, err
	}
	if used, resetAt, err := idx.OpsThisMonth(); err == nil {
		ov.OpsUsed, ov.OpsResetAt = used, resetAt
	}
	if st, err := d.currentSchedule(); err == nil {
		// A clean call means a scheduler exists on this platform, whether or
		// not r2backup has registered with it yet -- Current only returns
		// ErrUnsupported for the platforms with no implementation at all.
		ov.SchedulerAvailable = true
		if st.Registered {
			ov.Scheduled, ov.Interval, ov.RunsWhenSignedOut = true, st.Interval, st.RunsWhenSignedOut
			// NextRun and LastRun are best-effort -- some platforms can't say
			// and leave them zero on Status itself -- but when the OS does
			// report them, the Schedule tab should show them rather than the
			// zero value Overview starts with regardless.
			ov.NextRun, ov.LastRun = st.NextRun, st.LastRun
		}
	} else if !errors.Is(err, schedule.ErrUnsupported) {
		// A one-off failure reading the scheduler's state -- a command that
		// didn't run, a file that briefly wasn't there -- isn't the same
		// claim as "nothing here can schedule a run", so it shouldn't hide
		// the tab either.
		ov.SchedulerAvailable = true
	}
	// A run in another process -- the scheduler's, typically -- is read from
	// the same progress file `status --watch` reads, so the interface and the
	// command agree about what is happening without either owning it.
	if p, err := config.ProgressPath(); err == nil {
		now := time.Now()
		if live, err := runstate.ReadLive(p); err == nil && !live.Stale(now) {
			ov.Running = live.Set
			if live.BytesTotal > 0 {
				ov.RunPercent = float64(live.BytesDone) / float64(live.BytesTotal)
			}
			if live.ETAKnown {
				ov.RunETA = progress.FormatDuration(time.Duration(live.ETASeconds*float64(time.Second))) + " remaining"
			} else {
				ov.RunETA = "estimating..."
			}
		} else if stopped, ok := runstate.ReadInterrupted(p, now); ok {
			// The same file, left behind by a run that was not allowed to
			// finish. Reported rather than ignored: a backup that stopped
			// when the machine did is the thing the user most wants to know
			// about when they open this, and it showed nothing at all.
			ov.Interrupted = stopped.Set
			ov.InterruptedAt = stopped.UpdatedAt
			ov.InterruptedDone, ov.InterruptedTotal = stopped.BytesDone, stopped.BytesTotal
		}
	}
	// What the bucket is already holding of uploads that were cut off. Read
	// from the index, so it costs nothing and survives the program being
	// closed and reopened -- which is the point.
	if done, total, files, err := idx.PendingBytes(); err == nil && files > 0 {
		ov.PendingDone, ov.PendingTotal, ov.PendingFiles = done, total, files
	}
	return views, ov, nil
}

// uiObserver adapts backup.Observer to the two callbacks the interface wants,
// and keeps writing progress.json so a run started here is still visible to
// `r2b status --watch` in another terminal.
type uiObserver struct {
	phase        func(string)
	snap         func(progress.Snapshot)
	set          string
	progressPath string
	current      backup.Phase
}

func (o *uiObserver) Phase(p backup.Phase, r *backup.Report) {
	o.current = p
	switch p {
	case backup.PhaseScanning:
		o.phase("scanning " + o.set)
	case backup.PhasePlanning:
		o.phase(fmt.Sprintf("planning · %s files · %s",
			progress.FormatCount(r.ScannedFiles), progress.FormatBytes(r.ScannedBytes)))
	case backup.PhaseUploading:
		o.phase(fmt.Sprintf("uploading %s files (%s)",
			progress.FormatCount(int64(len(r.Planned.Uploads))), progress.FormatBytes(r.Planned.UploadBytes)))
	}
}

func (o *uiObserver) Progress(s progress.Snapshot) {
	if o.progressPath != "" {
		_ = runstate.WriteLive(o.progressPath, runstate.Live{
			Set: o.set, Phase: string(o.current),
			BytesDone: s.BytesDone, BytesTotal: s.BytesTotal,
			FilesDone: s.FilesDone, FilesTotal: s.FilesTotal,
			ByteRate: s.ByteRate, FileRate: s.FileRate,
			ETASeconds: s.ETA.Seconds(), ETAKnown: s.ETAKnown,
		})
	}
	o.snap(s)
}

func (d *dashboard) Backup(ctx context.Context, name string, phase func(string), snap func(progress.Snapshot)) error {
	a, err := d.connected(ctx)
	if err != nil {
		return err
	}
	s, err := a.sets.Get(name)
	if err != nil {
		return err
	}

	// Checked out for the whole run, exactly as runOne does for `r2b
	// backup`: a backup started from here holds the index for as long as
	// the backup takes, and if the lock is already held by another process
	// this fails here, before recordRun, so no history entry is written for
	// a run that never happened.
	idx, release, err := a.checkoutIndex()
	if err != nil {
		return err
	}
	defer release()

	progressPath, _ := config.ProgressPath()
	obs := &uiObserver{phase: phase, snap: snap, set: s.Name, progressPath: progressPath}
	defer runstate.ClearLive(progressPath)

	rep, runErr := backup.Run(ctx, backup.Options{
		Set: s, Index: idx, Client: a.client,
		Trash:    backup.NewTrash(a.client, s.RetentionDays),
		Observer: obs, DetectMoves: true,
	})
	recordRun(s.Name, rep, runErr)

	// A folder that has been moved or deleted must be parked, exactly as
	// `r2b backup` parks it. Without this the set stays StatusOK, `status`
	// never says "needs attention", and the scheduled run goes on failing
	// every half hour with nobody told.
	if errors.Is(runErr, backup.ErrRootMissing) {
		_ = a.sets.MarkNeedsAttention(s.Name, runErr.Error())
		return fmt.Errorf("%w\n  Press m on this folder to say where it went", runErr)
	}
	return runErr
}

// recordRun writes the history entry, so a run started from the interface
// shows up in `r2b status` exactly like one started from the command line.
//
// It is a deliberate copy of runOne's tail rather than a call to runOne:
// runOne opens its own observer and writes its own progress file, and the
// interface needs a different observer. The two must be kept in step by hand,
// which is what this comment is for.
func recordRun(name string, rep *backup.Report, runErr error) {
	past := runstate.Past{Set: name, FinishedAt: time.Now()}
	if runErr != nil {
		past.Error = runErr.Error()
		past.Cancelled = errors.Is(runErr, backup.ErrCancelled)
	} else if rep != nil {
		past.Duration = rep.Elapsed.Seconds()
		past.Uploaded, past.Moved, past.Deleted = rep.Uploaded, rep.Moved, rep.Deleted
		past.Unchanged, past.Bytes, past.Operations = rep.Unchanged, rep.Bytes, rep.Operations
		past.Problems, past.Failures, past.Collisions = len(rep.Problems), len(rep.Failures), len(rep.Collisions)
		past.Examples = examples(rep)
	}
	if histPath, err := historyPath(); err == nil {
		_ = runstate.Record(histPath, past)
	}
}

func (d *dashboard) Trash(ctx context.Context, name string) ([]ui.TrashRow, error) {
	a, err := d.connected(ctx)
	if err != nil {
		return nil, err
	}
	s, err := a.sets.Get(name)
	if err != nil {
		return nil, err
	}
	if !s.TrashEnabled() {
		return nil, nil
	}
	entries, err := trash.New(a.client, trash.Clock{}).List(ctx, s.Prefix, s.RetentionDays)
	if err != nil {
		return nil, err
	}
	rows := make([]ui.TrashRow, 0, len(entries))
	for _, e := range entries {
		rows = append(rows, ui.TrashRow{
			Key: e.RelPath, Size: e.Size, Deleted: e.TrashedOn, DeletedExact: e.TrashedOnExact,
			Expires: e.ExpiresOn,
		})
	}
	return rows, nil
}

// Remove stops backing up a folder, and with purge deletes what is stored.
//
// It used to refuse purge outright and tell the user to go and type
// `r2b remove <set> --purge`, which is the one thing this interface exists to
// stop doing. The refusal was standing in for a safeguard, so the safeguard is
// now real: the interface makes the user type the folder's name back before it
// will run this, which is a stronger confirmation than a flag anyone can
// tab-complete.
//
// The order is `r2b remove`'s order, for `r2b remove`'s reasons: the objects
// go first and the set is forgotten only if that worked, because the other way
// round loses the only handle on them the moment the delete fails partway.
func (d *dashboard) Remove(ctx context.Context, name string, purge bool) error {
	a := d.app
	s, err := a.sets.Get(name)
	if err != nil {
		return err
	}
	if purge {
		conn, err := d.connected(ctx)
		if err != nil {
			return err
		}
		// KeyScope, never Prefix: a bare prefix also matches a set whose
		// name merely starts with this one's.
		if _, err := conn.client.DeletePrefix(ctx, s.KeyScope()); err != nil {
			return fmt.Errorf("%q was not removed: %w", s.Name, err)
		}
	}
	idx, release, err := a.checkoutIndex()
	if err != nil {
		return err
	}
	defer release()
	// The index goes first, and this order is load-bearing. The other way
	// round, a partial failure leaves index records behind for a later set of
	// the same name to inherit -- which would then skip every file it thought
	// it had already uploaded.
	if err := idx.DropSet(s.Name); err != nil {
		return err
	}
	if err := idx.ForgetDailyPrune(s.Name); err != nil {
		return err
	}
	// Same reasoning as `r2b remove`: an unfinished upload is billed and
	// shows up in no object listing, and DropSetUploads below is the last
	// moment anything can still say what its upload id was. Aborted here,
	// before that record is gone, not after. A failed abort does not fail
	// the removal -- see abortSetUploads -- and, like AbandonStaleUploads'
	// own count in a backup Report, is not surfaced through this interface;
	// `r2b remove` is where that gets said.
	_, _ = a.abortSetUploads(ctx, idx, s, purge, func() error {
		_, err := d.connected(ctx)
		return err
	})
	if err := idx.DropSetUploads(s.KeyScope()); err != nil {
		return err
	}
	return a.sets.Remove(s.Name)
}

func (d *dashboard) Schedule(ctx context.Context, every int, off bool) error {
	if !schedule.Supported() {
		return errors.New("no scheduler is available on this platform")
	}
	if off {
		if err := scheduleRemove(scheduleName); err != nil {
			return err
		}
		// The Schedule tab reads Overview.Scheduled, which came from the
		// cache Load fills in -- without this it would keep showing the
		// entry as registered for up to scheduleCacheTTL after the user just
		// watched it get removed.
		d.invalidateScheduleCache()
		return nil
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}
	binary, _ := scheduledBinary(runtime.GOOS, self, fileExists)
	if err := scheduleInstall(schedule.Entry{
		Name:       scheduleName,
		Interval:   time.Duration(every) * time.Minute,
		BinaryPath: binary,
		Args:       scheduledRunArgs(runtime.GOOS),
	}); err != nil {
		return err
	}
	d.invalidateScheduleCache()
	return nil
}

// RepairSchedule is `r2b schedule --repair`.
//
// The OS scheduler stores an absolute path, so moving or renaming the binary
// leaves the task aimed at a file that is not there any more -- and a backup
// that silently stops running is the worst way for this to fail. The
// installers call the command after replacing the files; this is the same
// thing for someone who moved the binary themselves and has no idea a
// command exists for it.
//
// A machine with no schedule is left with no schedule. Repairing must never
// quietly start backing things up on a timer nobody asked for, which is why
// this reports what it found rather than falling through to Install.
func (d *dashboard) RepairSchedule(ctx context.Context) (bool, error) {
	if !schedule.Supported() {
		return false, errors.New("no scheduler is available on this platform")
	}
	st, err := d.currentSchedule()
	if err != nil || !st.Registered {
		return false, nil
	}
	// d.Schedule invalidates the cache on success, so the state this read
	// back -- possibly stale by up to scheduleCacheTTL, which is harmless
	// here since the interval it's keying off doesn't change on its own --
	// doesn't linger once the repair itself has run.
	return true, d.Schedule(ctx, repairMinutes(st), false)
}
