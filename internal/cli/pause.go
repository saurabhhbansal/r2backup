package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/saurabhhbansal/r2backup/internal/config"
	"github.com/saurabhhbansal/r2backup/internal/pause"
	"github.com/saurabhhbansal/r2backup/internal/progress"
	"github.com/saurabhhbansal/r2backup/internal/runstate"
)

// livePublisher writes progress.json for a run in this process: what it has
// found while it is still scanning, then the bar and the estimate as it is
// shown, and whether it is paused. Shared by `r2b backup` and the full-screen
// interface so both say the same thing to the panel and to `status`.
type livePublisher struct {
	path      string
	set       string
	gate      pause.Gate
	pres      progress.Presenter
	lastFound time.Time
}

func (l *livePublisher) paused() bool { return l.gate != nil && l.gate.Paused() }

// found publishes the scan's running count. Called from the scan itself, so
// it writes at most twice a second however fast files turn up.
func (l *livePublisher) found(files, bytes int64, force bool) {
	if l.path == "" {
		return
	}
	if now := time.Now(); force || now.Sub(l.lastFound) >= 500*time.Millisecond {
		l.lastFound = now
		_ = runstate.WriteLive(l.path, runstate.Live{
			Set: l.set, Phase: runstate.PhaseFinding,
			FoundFiles: files, FoundBytes: bytes, Paused: l.paused(),
		})
	}
}

// progress publishes one snapshot of the transfer.
func (l *livePublisher) progress(phase string, s progress.Snapshot) {
	if l.path == "" {
		return
	}
	eta, known := l.pres.Present(s)
	_ = runstate.WriteLive(l.path, runstate.Live{
		Set: l.set, Phase: phase,
		BytesDone: s.BytesDone, BytesTotal: s.BytesTotal,
		FilesDone: s.FilesDone, FilesTotal: s.FilesTotal,
		ByteRate: s.ByteRate, FileRate: s.FileRate,
		ETASeconds: eta.Seconds(), ETAKnown: known,
		Paused: s.Paused || l.paused(),
	})
}

// watchPause makes a run in this process pausable from outside it. Every run
// starts unpaused and ends unpaused: a marker this process did not ask for --
// left by a run that crashed, or by a computer switched off while paused --
// is cleared rather than obeyed, and the run's own marker is cleared when it
// ends, so nothing carries over to the next one.
func watchPause(ctx context.Context, progressPath string) (context.Context, pause.Gate, func()) {
	if progressPath == "" {
		return ctx, nil, func() {}
	}
	path := pause.Path(progressPath)
	pid := os.Getpid()
	if !pause.For(path, pid) {
		_ = pause.Clear(path)
	}
	w := pause.Watch(path, pid, 0)
	return pause.NewContext(ctx, w), w, func() {
		w.Close()
		_ = pause.Clear(path)
	}
}

// errNoRunToPause is what pausing says when there is nothing to pause.
var errNoRunToPause = errors.New("no backup is running")

// setRunPaused pauses or resumes the backup running right now, whichever
// process it is in. It returns the run it acted on.
func setRunPaused(on bool) (*runstate.Live, error) {
	p, err := config.ProgressPath()
	if err != nil {
		return nil, err
	}
	live, err := runstate.ReadLive(p)
	if err != nil || live.Stale(time.Now()) {
		return nil, errNoRunToPause
	}
	path := pause.Path(p)
	if on {
		err = pause.Set(path, live.PID)
	} else {
		err = pause.Clear(path)
	}
	if err != nil {
		return nil, fmt.Errorf("could not %s the backup: %w", map[bool]string{true: "pause", false: "resume"}[on], err)
	}
	return live, nil
}

func newPauseCmd(opts *Options) *cobra.Command {
	return &cobra.Command{
		Use:   "pause",
		Short: "Pause the backup that is running, here or scheduled",
		Long: "Pause the backup that is running, whether it was started here, from the taskbar panel or by the schedule.\n" +
			"Files already being sent finish; nothing new starts until `r2b resume`.\n" +
			"A pause lasts for that run only: the next run starts as usual.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			live, err := setRunPaused(true)
			if err != nil {
				return err
			}
			fmt.Fprintf(opts.Out, "Paused backing up %s. Files already being sent will finish. r2b resume carries on.\n", live.Set)
			return nil
		},
	}
}

func newResumeCmd(opts *Options) *cobra.Command {
	return &cobra.Command{
		Use:   "resume",
		Short: "Carry on with a paused backup",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			live, err := setRunPaused(false)
			if err != nil {
				return err
			}
			fmt.Fprintf(opts.Out, "Carrying on backing up %s.\n", live.Set)
			return nil
		},
	}
}

// PauseRun is the full-screen interface's p on the progress screen.
func (d *dashboard) PauseRun(on bool) error {
	_, err := setRunPaused(on)
	return err
}
