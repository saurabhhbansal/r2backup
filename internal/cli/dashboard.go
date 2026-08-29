package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"runtime"
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

// dashboard implements ui.Backend on top of the same app every command uses.
//
// It holds no state of its own and duplicates no logic: a backup started from
// the interface goes through runOne, exactly like `r2b backup`, so the two
// cannot drift into recording different things about the same run.
type dashboard struct{ opts *Options }

// scheduleName is the OS scheduler entry's identity, spelled once here rather
// than at each call site. It is deliberately still "r2backup": an installed
// machine already has a task under that name, and changing it would leave the
// old one running and the new one duplicating it.
const scheduleName = "r2backup"

func (d *dashboard) Load(ctx context.Context) ([]ui.SetView, ui.Overview, error) {
	a, err := openApp()
	if err != nil {
		return nil, ui.Overview{}, err
	}
	defer a.close()

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
		if s.Status == sets.StatusNeedsAttention {
			v.State, v.Note = "needs attention", s.StatusNote
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
		} else if v.State == "ok" {
			v.State = "never run"
		}
		views = append(views, v)
	}

	ov := ui.Overview{Machine: machineName(), OpsLimit: index.FreeTierOpsPerMonth}
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
	a, err := openApp()
	if err != nil {
		return err
	}
	defer a.close()
	if err := a.connect(ctx); err != nil {
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
	return runErr
}

// recordRun writes the history entry, so a run started from the interface
// shows up in `r2b status` exactly like one started from the command line.
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
	a, err := openApp()
	if err != nil {
		return nil, err
	}
	defer a.close()
	if err := a.connect(ctx); err != nil {
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

func (d *dashboard) Remove(ctx context.Context, name string, purge bool) error {
	a, err := openApp()
	if err != nil {
		return err
	}
	defer a.close()
	if purge {
		return errors.New("purging from the interface is deliberately not offered; use: r2b remove " + name + " --purge")
	}
	// The index goes first, and this order is load-bearing. The other way
	// round, a partial failure leaves index records behind for a later set of
	// the same name to inherit -- which would then skip every file it thought
	// it had already uploaded.
	if err := a.index.DropSet(name); err != nil {
		return err
	}
	if err := a.index.ForgetDailyPrune(name); err != nil {
		return err
	}
	return a.sets.Remove(name)
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
