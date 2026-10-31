package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"

	"github.com/saurabhhbansal/r2backup/internal/account"
	"github.com/saurabhhbansal/r2backup/internal/creds"
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
	t := &trayBackend{d: d, checkUpdates: true}
	defer t.endCF()
	return tray.Run(ctx, t)
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
	// stopped is set when the person stopped the child, so its exit is not
	// reported as a failure.
	stopped bool

	// notice is said once by the panel: why something it started did not
	// work, when that happened before there was a run to record it.
	notice   string
	noticeAt time.Time

	// update is a newer release, found by a check made at most once a day
	// when checkUpdates is on (it is off in tests, which must not reach
	// the network).
	update       string
	updateCheck  time.Time
	checkUpdates bool

	mj *mirrorJob // the mirror copy the panel is running or has just run
	cf *cfSession // a Cloudflare sign-in in progress

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

// noticeFor is how long a notice stays on offer: long enough for a panel
// opened a little later to still say it.
const noticeFor = 2 * time.Minute

// say leaves a notice for the panel.
func (t *trayBackend) say(msg string) {
	t.mu.Lock()
	t.notice, t.noticeAt = msg, time.Now()
	t.mu.Unlock()
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
	st := tray.Summarize(views, ov, runs, now)
	st.Email, st.SignedIn = signedInHere(now)
	st.Mode = accountMode()
	if st.SignedIn {
		_, err := account.LoadSealKey()
		st.NeedsPIN = errors.Is(err, account.ErrPINRequired)
	}
	if mc, err := t.d.app.mirror.Load(); err == nil {
		st.Second = destLabel(mc)
	}
	t.mu.Lock()
	st.CanStop = t.child != nil && !t.stopped && t.busy == busyBackingUp
	if t.notice != "" && now.Sub(t.noticeAt) < noticeFor {
		st.Notice, st.NoticeAt = t.notice, t.noticeAt.UnixMilli()
	}
	st.Update = t.update
	t.mu.Unlock()
	t.maybeCheckUpdate(now)
	return st
}

func (t *trayBackend) Busy() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.busy
}

// busyBackingUp is the busy label of a backup the panel started.
const busyBackingUp = "Backing up…"

// tailWriter keeps the last few kilobytes written to it: the end of a hidden
// child's error output, which is where it says why it stopped.
type tailWriter struct {
	mu  sync.Mutex
	buf []byte
}

const tailMax = 4 << 10

func (w *tailWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf = append(w.buf, p...)
	if len(w.buf) > tailMax {
		w.buf = w.buf[len(w.buf)-tailMax:]
	}
	return len(p), nil
}

// lastProblem picks the line that says what went wrong out of a child's
// error output: the first line naming a folder's failure, else the last line.
// "<set>: <error>" is how r2b backup reports each folder that did not finish,
// followed by a count that says less.
func lastProblem(out string) string {
	var lines []string
	for _, l := range strings.Split(out, "\n") {
		l = strings.TrimSpace(l)
		if l == "" || strings.HasPrefix(l, "Nothing was deleted.") {
			continue
		}
		lines = append(lines, strings.TrimPrefix(l, "Error: "))
	}
	if len(lines) == 0 {
		return ""
	}
	if len(lines) > 1 && strings.Contains(lines[len(lines)-1], "did not finish") {
		return lines[0]
	}
	return lines[len(lines)-1]
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
	// What a hidden child says when it fails would otherwise go nowhere,
	// and a backup that never started -- storage not connected, another
	// run holding the index -- records no run to show for it either.
	errOut := &tailWriter{}
	cmd.Stderr = errOut
	if err := cmd.Start(); err != nil {
		return err
	}
	t.child, t.busy, t.what, t.stopped = cmd, label, what, false
	go func() {
		werr := cmd.Wait()
		t.mu.Lock()
		stopped := t.stopped
		t.child, t.busy, t.stopped = nil, "", false
		t.last.ok = false // reload: the run has changed what there is to show
		t.mu.Unlock()
		switch {
		case stopped:
			t.say("Stopped. What was uploaded is kept, and the next backup carries on from there.")
		case werr != nil:
			errOut.mu.Lock()
			msg := lastProblem(string(errOut.buf))
			errOut.mu.Unlock()
			if msg == "" {
				msg = werr.Error()
			}
			t.say(tray.Plain(msg))
		}
	}()
	return nil
}

// stop ends a backup this panel started. The run is cut off where it is,
// exactly as if the computer had been shut down: what was uploaded stays,
// and the next run carries on from there.
func (t *trayBackend) stop() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.child == nil || t.busy != busyBackingUp {
		return errors.New("there is no backup from here to stop")
	}
	t.stopped = true
	return t.child.Process.Kill()
}

func (t *trayBackend) BackUp(name string) error {
	args, what := []string{"backup"}, "your folders"
	if name != "" {
		args, what = append(args, name), name
	}
	return t.start(busyBackingUp, what, args...)
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
	switch c.Kind() {
	case creds.ProviderB2:
		return "https://secure.backblaze.com/b2_buckets.htm"
	case creds.ProviderS3:
		return "https://s3.console.aws.amazon.com/s3/buckets/" + c.Bucket
	case creds.ProviderCustom:
		return ""
	}
	if c.AccountID != "" && c.Bucket != "" {
		return "https://dash.cloudflare.com/" + c.AccountID + "/r2/default/buckets/" + c.Bucket
	}
	return "https://dash.cloudflare.com/"
}
