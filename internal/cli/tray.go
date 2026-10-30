package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"github.com/spf13/cobra"

	"github.com/saurabhhbansal/r2backup/internal/index"
	"github.com/saurabhhbansal/r2backup/internal/runstate"
	"github.com/saurabhhbansal/r2backup/internal/sets"
	"github.com/saurabhhbansal/r2backup/internal/tray"
	"github.com/saurabhhbansal/r2backup/internal/ui"
)

func newTrayCmd(opts *Options) *cobra.Command {
	return &cobra.Command{
		Use:   "tray",
		Short: "Keep an icon in the Windows taskbar",
		Long: "Puts an icon in the notification area. Click it for a panel\n" +
			"showing how your backups are doing, with buttons to back up now, add\n" +
			"a folder and turn automatic backups on or off.\n\n" +
			"Settings in the panel can start it with Windows. Run it through r2bw.exe\n" +
			"(r2bw tray) for no console window. Windows only.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			err := runTray(cmd.Context(), opts)
			// Started from the Start menu or at sign-in there is no console
			// to print this to, so it is shown on screen as well.
			tray.Fail(err)
			return err
		},
	}
}

func runTray(ctx context.Context, opts *Options) error {
	d, err := openDashboard(opts)
	if err != nil {
		return err
	}
	defer d.close()
	return tray.Run(ctx, &trayBackend{d: d})
}

// trayBackend is the panel's view of the program. Reading goes through the
// dashboard -- the same loader the terminal interface uses -- and every
// action starts one of r2b's own commands in a hidden window, so nothing in
// the panel is a second way of doing a backup.
type trayBackend struct {
	d *dashboard

	mu    sync.Mutex
	busy  string
	what  string // what the running child is backing up, for the status line
	child *exec.Cmd

	rj    *restoreJob           // the restore the panel is running or has just run
	trees map[string]cachedTree // backed-up files by set, for browsing a restore
	local map[string]cachedTree // each folder as it is on disk, for choosing what to include
	last  struct {
		views []ui.SetView
		ov    ui.Overview
		ok    bool
	}
}

var _ tray.Backend = (*trayBackend)(nil)

// State loads what the panel shows. While a backup is running it holds the
// index for the whole run, so the folders are taken from the last load and
// only the live progress is read fresh -- otherwise each refresh would wait
// out the index's five-second lock timeout and the panel would freeze.
func (t *trayBackend) State(ctx context.Context) (tray.State, error) {
	now := time.Now()
	t.mu.Lock()
	cached, views, ov, ours, what := t.last.ok, t.last.views, t.last.ov, t.child != nil, t.what
	t.mu.Unlock()

	// A run in progress -- ours, or one whose progress file is live -- holds
	// the index, so the folders come from the last load.
	// Folders are added and removed by other r2b processes -- the panel's
	// own "Back up another folder" runs `r2b add` -- so the list is read
	// again rather than kept from start-up. A failed read keeps the last one.
	_ = t.d.app.sets.Reload()
	if cached && (applyLiveRun(&ov, now) || ours) {
		if ours && ov.Running == "" {
			// Ours, still scanning: no progress written yet, but it is
			// running, and the panel should say so from the first moment.
			ov.Running = what
		}
		// The index is held, so what each folder holds comes from the last
		// load -- but the list of folders does not need the index, and a
		// folder just added (the one being backed up, often) belongs in it
		// now rather than when the run ends.
		return t.summarize(withCurrentSets(views, t.d.app.sets.List()), ov, now), nil
	}
	fresh, freshOV, err := t.d.Load(ctx)
	if err != nil {
		if errors.Is(err, index.ErrLocked) && cached {
			applyLiveRun(&ov, time.Now())
			return t.summarize(views, ov, now), nil
		}
		return tray.State{}, err
	}
	views, ov = fresh, freshOV
	t.mu.Lock()
	t.last.views, t.last.ov, t.last.ok = views, ov, true
	t.mu.Unlock()
	return t.summarize(views, ov, now), nil
}

// withCurrentSets brings views loaded earlier in line with the sets as they
// are now: a set added since appears, not yet run, and a removed one goes.
func withCurrentSets(views []ui.SetView, current []sets.Set) []ui.SetView {
	byName := make(map[string]ui.SetView, len(views))
	for _, v := range views {
		byName[v.Name] = v
	}
	out := make([]ui.SetView, 0, len(current))
	for _, s := range current {
		if v, ok := byName[s.Name]; ok {
			out = append(out, v)
			continue
		}
		out = append(out, ui.SetView{
			Name: s.Name, Root: s.Root, Prefix: s.Prefix,
			Excludes: s.Excludes, Retention: s.RetentionDays,
			State: "never run",
		})
	}
	return out
}

func (t *trayBackend) summarize(views []ui.SetView, ov ui.Overview, now time.Time) tray.State {
	var runs []runstate.Past
	if p, err := historyPath(); err == nil {
		if h, err := runstate.ReadHistory(p); err == nil {
			runs = h.Runs
		}
	}
	return tray.Summarize(views, ov, runs, now)
}

func (t *trayBackend) Busy() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.busy
}

// start runs r2b with args in the background, hidden, and remembers it so
// the panel can say it is working and refuse to start a second one.
func (t *trayBackend) start(label, what string, args ...string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.child != nil {
		return fmt.Errorf("%s is still running", t.busy)
	}
	if t.last.ov.Running != "" {
		// A run the scheduler started holds the index; a second would wait
		// for it and then give up.
		var ov ui.Overview
		if applyLiveRun(&ov, time.Now()) {
			return fmt.Errorf("a backup of %s is already running", ov.Running)
		}
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := exec.Command(exe, append([]string{"--no"}, args...)...)
	hideWindow(cmd)
	if err := cmd.Start(); err != nil {
		return err
	}
	t.child, t.busy, t.what = cmd, label, what
	go func() {
		_ = cmd.Wait()
		t.mu.Lock()
		t.child, t.busy = nil, ""
		t.last.ok = false // reload: the run has changed what there is to show
		t.mu.Unlock()
	}()
	return nil
}

func (t *trayBackend) BackUp(name string) error {
	args, what := []string{"backup"}, "your folders"
	if name != "" {
		args, what = append(args, name), name
	}
	return t.start("Backing up…", what, args...)
}

func (t *trayBackend) AddFolder(path string) error {
	return t.start("Adding folder…", filepath.Base(path), "add", path, "--all")
}

func (t *trayBackend) SetSchedule(ctx context.Context, on bool, every int) error {
	return t.d.Schedule(ctx, every, !on)
}

func (t *trayBackend) StorageURL() string {
	c, err := t.d.app.creds.Load()
	if err != nil {
		return ""
	}
	if c.IsDrime() {
		return "https://app.drime.cloud/drive"
	}
	if c.AccountID != "" && c.Bucket != "" {
		return "https://dash.cloudflare.com/" + c.AccountID + "/r2/default/buckets/" + c.Bucket
	}
	return "https://dash.cloudflare.com/"
}
