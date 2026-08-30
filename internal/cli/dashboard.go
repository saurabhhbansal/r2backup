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
// optimisation. Every method used to call openApp() itself, which opens the
// bbolt index -- and bbolt takes a file lock that contends with itself inside
// one process. A backup holds the index for its whole run, the interface
// reloads once a second, and that reload blocked for bbolt's five-second
// timeout and then failed with `open index at "...": timeout`. The progress
// screen vanished mid-backup, replaced by an error about a file, and the
// model then believed nothing was running -- so a second backup could be
// started on top of the first. Measured: a second openApp in the same process
// returns an error after 4.99s.
//
// One handle, opened once. bbolt is safe for concurrent use through a single
// handle and sets.Store carries its own RWMutex; connect is the one piece of
// shared state that is not, so it has a mutex here.
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

	// mu guards lazily building the R2 client. Load runs on the UI loop and
	// a transfer runs on a worker, so two goroutines can reach connect at
	// once.
	mu sync.Mutex
}

// openDashboard opens the state the interface needs, once.
func openDashboard(opts *Options) (*dashboard, error) {
	a, err := openApp()
	if err != nil {
		return nil, err
	}
	return &dashboard{opts: opts, app: a}, nil
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

func (d *dashboard) Load(ctx context.Context) ([]ui.SetView, ui.Overview, error) {
	a := d.app

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
		if n, err := a.index.Count(s.Name); err == nil {
			v.Objects = n
		}
		if last, ok := hist.Last(s.Name); ok {
			v.HasRun, v.LastRun = true, last.FinishedAt
			v.Uploaded, v.Unchanged = last.Uploaded, last.Unchanged
			v.Deleted, v.Moved, v.Bytes = last.Deleted, last.Moved, last.Bytes
			v.Operations = last.Operations
			v.Failures, v.Problems, v.Collisions = last.Failures, last.Problems, last.Collisions
			v.Examples = last.Examples
			if last.Error != "" {
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
	} else if !errors.Is(err, creds.ErrNotFound) {
		return views, ov, err
	}
	if used, resetAt, err := a.index.OpsThisMonth(); err == nil {
		ov.OpsUsed, ov.OpsResetAt = used, resetAt
	}
	if st, err := schedule.Current(scheduleName); err == nil && st.Registered {
		ov.Scheduled, ov.Interval = true, st.Interval
	}
	// A run in another process -- the scheduler's, typically -- is read from
	// the same progress file `status --watch` reads, so the interface and the
	// command agree about what is happening without either owning it.
	if p, err := config.ProgressPath(); err == nil {
		if live, err := runstate.ReadLive(p); err == nil && !live.Stale(time.Now()) {
			ov.Running = live.Set
			if live.BytesTotal > 0 {
				ov.RunPercent = float64(live.BytesDone) / float64(live.BytesTotal)
			}
			if live.ETAKnown {
				ov.RunETA = progress.FormatDuration(time.Duration(live.ETASeconds*float64(time.Second))) + " remaining"
			} else {
				ov.RunETA = "estimating..."
			}
		}
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

	progressPath, _ := config.ProgressPath()
	obs := &uiObserver{phase: phase, snap: snap, set: s.Name, progressPath: progressPath}
	defer runstate.ClearLive(progressPath)

	rep, runErr := backup.Run(ctx, backup.Options{
		Set: s, Index: a.index, Client: a.client,
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
			Key: e.RelPath, Size: e.Size, Deleted: e.TrashedOn, Expires: e.ExpiresOn,
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
	// The index goes first, and this order is load-bearing. The other way
	// round, a partial failure leaves index records behind for a later set of
	// the same name to inherit -- which would then skip every file it thought
	// it had already uploaded.
	if err := a.index.DropSet(s.Name); err != nil {
		return err
	}
	if err := a.index.ForgetDailyPrune(s.Name); err != nil {
		return err
	}
	if err := a.index.DropSetUploads(s.KeyScope()); err != nil {
		return err
	}
	return a.sets.Remove(s.Name)
}

func (d *dashboard) Schedule(ctx context.Context, every int, off bool) error {
	if !schedule.Supported() {
		return errors.New("no scheduler is available on this platform")
	}
	if off {
		return schedule.Remove(scheduleName)
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}
	binary, _ := scheduledBinary(runtime.GOOS, self, fileExists)
	return schedule.Install(schedule.Entry{
		Name:       scheduleName,
		Interval:   time.Duration(every) * time.Minute,
		BinaryPath: binary,
		Args:       scheduledRunArgs(runtime.GOOS),
	})
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
	st, err := schedule.Current(scheduleName)
	if err != nil || !st.Registered {
		return false, nil
	}
	every := int(st.Interval.Round(time.Minute) / time.Minute)
	if every < 1 {
		every = schedule.DefaultIntervalMinutes
	}
	return true, d.Schedule(ctx, every, false)
}
