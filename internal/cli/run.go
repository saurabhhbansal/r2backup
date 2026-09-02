package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/saurabhhbansal/r2backup/internal/backup"
	"github.com/saurabhhbansal/r2backup/internal/config"
	"github.com/saurabhhbansal/r2backup/internal/progress"
	"github.com/saurabhhbansal/r2backup/internal/runstate"
	"github.com/saurabhhbansal/r2backup/internal/sets"
)

// terminalObserver draws the bar for an interactive run and publishes
// progress.json for anyone watching a scheduled one. Both, always: a run
// started by hand can still be watched from another terminal.
type terminalObserver struct {
	out          io.Writer
	set          string
	progressPath string
	interactive  bool
	lastLines    int
	phase        backup.Phase
}

func (o *terminalObserver) Phase(p backup.Phase, r *backup.Report) {
	o.phase = p
	if !o.interactive {
		return
	}
	switch p {
	case backup.PhaseScanning:
		fmt.Fprintf(o.out, "[1/3] Scanning    %s\n", o.set)
	case backup.PhasePlanning:
		fmt.Fprintf(o.out, "      %s files · %s\n[2/3] Planning\n",
			progress.FormatCount(r.ScannedFiles), progress.FormatBytes(r.ScannedBytes))
	case backup.PhaseUploading:
		p := r.Planned
		fmt.Fprintf(o.out, "      %s to upload (%s) · %s to delete · %s unchanged\n[3/3] Uploading\n",
			progress.FormatCount(int64(len(p.Uploads))), progress.FormatBytes(p.UploadBytes),
			progress.FormatCount(int64(len(p.Deletes))), progress.FormatCount(int64(p.Unchanged)))
	}
}

func (o *terminalObserver) Progress(s progress.Snapshot) {
	if o.progressPath != "" {
		_ = runstate.WriteLive(o.progressPath, runstate.Live{
			Set: o.set, Phase: string(o.phase),
			BytesDone: s.BytesDone, BytesTotal: s.BytesTotal,
			FilesDone: s.FilesDone, FilesTotal: s.FilesTotal,
			ByteRate: s.ByteRate, FileRate: s.FileRate,
			ETASeconds: s.ETA.Seconds(), ETAKnown: s.ETAKnown,
		})
	}
	if !o.interactive {
		return
	}
	o.clear()
	line := progress.Render(s, 40)
	fmt.Fprintln(o.out, line)
	o.lastLines = strings.Count(line, "\n") + 1
}

// clear rewinds the cursor over the previous frame so the bar updates in place
// instead of scrolling. Only ever used when attached to a terminal.
func (o *terminalObserver) clear() {
	for i := 0; i < o.lastLines; i++ {
		fmt.Fprint(o.out, "\033[1A\033[2K")
	}
	o.lastLines = 0
}

// runOne performs a backup of a single set and records what happened.
func runOne(ctx context.Context, a *app, s sets.Set, out io.Writer, interactive bool) (*backup.Report, error) {
	// Checked out for the whole run, not per call: backup.Run holds the
	// index for as long as the backup takes, exactly the way it always has.
	// If this fails because another r2b process already holds the lock,
	// nothing below has run yet -- no history entry is written for it, the
	// same as when this failure used to happen at openApp before any set was
	// even resolved.
	idx, release, err := a.checkoutIndex()
	if err != nil {
		return nil, err
	}
	defer release()

	progressPath, err := config.ProgressPath()
	if err != nil {
		return nil, err
	}
	obs := &terminalObserver{out: out, set: s.Name, progressPath: progressPath, interactive: interactive}
	defer runstate.ClearLive(progressPath)

	rep, runErr := backup.Run(ctx, backup.Options{
		Set:         s,
		Index:       idx,
		Client:      a.client,
		Trash:       backup.NewTrash(a.client, s.RetentionDays),
		Observer:    obs,
		DetectMoves: true,
	})
	if interactive {
		obs.clear()
	}

	past := runstate.Past{Set: s.Name, FinishedAt: time.Now()}
	if runErr != nil {
		past.Error = runErr.Error()
		past.Cancelled = errors.Is(runErr, backup.ErrCancelled)
	} else {
		past.Duration = rep.Elapsed.Seconds()
		past.Uploaded, past.Moved, past.Deleted = rep.Uploaded, rep.Moved, rep.Deleted
		past.Unchanged, past.Bytes, past.Operations = rep.Unchanged, rep.Bytes, rep.Operations
		past.Problems, past.Failures, past.Collisions = len(rep.Problems), len(rep.Failures), len(rep.Collisions)
		past.Examples = examples(rep)
	}
	if histPath, err := historyPath(); err == nil {
		_ = runstate.Record(histPath, past)
	}
	return rep, runErr
}

// examples pulls a few names out of a report so `status` can say what went
// wrong without the user opening a log.
func examples(r *backup.Report) []string {
	var out []string
	for _, f := range r.Failures {
		if len(out) == 3 {
			return out
		}
		out = append(out, f.Key+": "+f.Err.Error())
	}
	for _, p := range r.Problems {
		if len(out) == 3 {
			return out
		}
		out = append(out, p.Path+": "+p.Err.Error())
	}
	for _, c := range r.Collisions {
		if len(out) == 3 {
			return out
		}
		out = append(out, c.Key+": another file normalizes to the same name")
	}
	return out
}

func historyPath() (string, error) {
	dir, err := config.DataDir()
	if err != nil {
		return "", err
	}
	return dir + string(os.PathSeparator) + "history.json", nil
}

// summarise prints what a finished run did. Problems and collisions are always
// named: a run that quietly left files out is the failure mode this tool is
// built to not have.
func summarise(out io.Writer, r *backup.Report) {
	switch {
	case r.Planned != nil && r.Planned.Empty():
		fmt.Fprintf(out, "%s: nothing changed (%s files unchanged, 0 operations)\n",
			r.Set, progress.FormatCount(int64(r.Unchanged)))
	default:
		fmt.Fprintf(out, "%s: %s uploaded (%s), %s moved, %s deleted, %s unchanged in %s\n",
			r.Set,
			progress.FormatCount(int64(r.Uploaded)), progress.FormatBytes(r.Bytes),
			progress.FormatCount(int64(r.Moved)), progress.FormatCount(int64(r.Deleted)),
			progress.FormatCount(int64(r.Unchanged)), r.Elapsed.Round(time.Second))
	}
	// Said rather than left to be wondered about. Parts of an upload nobody
	// finished are billed and show up in no object listing, so a user who
	// noticed the charge would have had nothing to connect it to -- and this
	// is the line that says the charge has stopped.
	if r.Abandoned > 0 {
		fmt.Fprintf(out, "  Gave up on %s left unfinished for over a week. The space they held is freed.\n",
			countOf(int64(r.Abandoned), "part-uploaded file", "part-uploaded files"))
	}
	if n := len(r.Problems); n > 0 {
		fmt.Fprintf(out, "  %d file(s) could not be read:\n", n)
		for i, p := range r.Problems {
			if i == 5 {
				fmt.Fprintf(out, "    ...and %d more\n", n-5)
				break
			}
			fmt.Fprintf(out, "    %s: %v\n", p.Path, p.Err)
		}
	}
	if n := len(r.Collisions); n > 0 {
		fmt.Fprintf(out, "  %d file(s) could not be stored because another file has the same name after\n"+
			"  Unicode normalization. Only one of each can be kept:\n", n)
		for i, c := range r.Collisions {
			if i == 5 {
				fmt.Fprintf(out, "    ...and %d more\n", n-5)
				break
			}
			fmt.Fprintf(out, "    %s\n", c.Key)
		}
	}
	// Reported when it did something or when it could not. A retention
	// window that nothing enforces was the bug here -- trash.Prune had no
	// caller at all -- so a failure to enforce it is said out loud rather
	// than left for someone to notice in a storage bill.
	if r.PruneErr != nil {
		fmt.Fprintf(out, "  expired files could not be cleared from trash: %v\n", r.PruneErr)
	} else if r.Pruned.Keys > 0 {
		fmt.Fprintf(out, "  %d expired file(s) cleared from trash.\n", r.Pruned.Keys)
	}
	if n := len(r.Failures); n > 0 {
		fmt.Fprintf(out, "  %d file(s) failed to upload and will be retried next run:\n", n)
		for i, f := range r.Failures {
			if i == 5 {
				fmt.Fprintf(out, "    ...and %d more\n", n-5)
				break
			}
			fmt.Fprintf(out, "    %s: %v\n", f.Key, f.Err)
		}
	}
}
