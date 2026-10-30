// Package tray is r2backup's Windows taskbar app: an icon in the
// notification area and, on a click, a small panel that answers "are my
// backups all right?" and offers the handful of things worth doing from
// there.
//
// It shows only what r2backup actually does. Every number comes from the
// same places the terminal interface reads -- the sets, the run history, the
// progress file a running backup writes -- and every button starts one of
// r2backup's own commands. Nothing here is a second implementation of a
// backup; the panel is a window onto the program, not a fork of it.
//
// This file is the panel's data model, built from those sources by a pure
// function so it can be tested anywhere. The window itself is Windows-only
// (tray_windows.go).
package tray

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/saurabhhbansal/r2backup/internal/progress"
	"github.com/saurabhhbansal/r2backup/internal/runstate"
	"github.com/saurabhhbansal/r2backup/internal/ui"
)

// Status kinds. The panel and the tray icon pick their colour from these.
const (
	KindOK          = "ok"
	KindRunning     = "running"
	KindAttention   = "attention"
	KindPending     = "pending"
	KindInterrupted = "interrupted"
	KindSetup       = "setup"
)

// State is everything the panel draws, sent to it as JSON.
type State struct {
	Configured  bool   `json:"configured"`
	Provider    string `json:"provider"`    // "drime" or "r2"
	Destination string `json:"destination"` // "Drime · r2backup", "my-bucket"
	Machine     string `json:"machine"`
	Version     string `json:"version"`
	// SignedIn and Email come from the session saved on this computer, so
	// the panel can show who is signed in, and ask for it when nobody is.
	SignedIn bool   `json:"signedIn"`
	Email    string `json:"email,omitempty"`
	// Second is where the second copy goes, "" when there is none.
	Second string `json:"second,omitempty"`

	Status  Status   `json:"status"`
	Running *Running `json:"running,omitempty"`
	Folders []Folder `json:"folders"`
	Recent  []Recent `json:"recent"`
	Sched   Schedule `json:"schedule"`
	Login   bool     `json:"startAtLogin"`
	Busy    string   `json:"busy,omitempty"` // a command the panel started and is waiting on
	// CanStop is true while Busy is a backup this panel started, which it
	// can also stop.
	CanStop bool `json:"canStop,omitempty"`
	// Notice is something the panel should say once, such as why a backup it
	// started did not run. NoticeAt tells a new one from the same one read
	// again on the next refresh.
	Notice   string `json:"notice,omitempty"`
	NoticeAt int64  `json:"noticeAt,omitempty"`
	// Update names a newer release, from a check made at most once a day.
	Update string    `json:"update,omitempty"`
	At     time.Time `json:"at"`
}

// Status is the one-line answer at the top of the panel, and the tooltip.
type Status struct {
	Kind   string `json:"kind"`
	Title  string `json:"title"`
	Detail string `json:"detail"`
	// Action is what the status block's button does, when there is one
	// thing worth doing about it: "cost" (the spending limit paused
	// backups), "folder:<name>" (the one folder that needs a look) or
	// "backup" (carry on a run that was interrupted).
	Action string `json:"action,omitempty"`
}

// Running is a backup in progress, from this panel or the scheduler.
type Running struct {
	Set     string  `json:"set"`
	Percent float64 `json:"percent"` // 0-100; 0 while it is still working out how much
	ETA     string  `json:"eta"`
}

// Folder is one backed-up folder.
type Folder struct {
	Name     string `json:"name"`
	Root     string `json:"root"`
	Kind     string `json:"kind"` // ok, attention, never, stopped
	State    string `json:"state"`
	LastRun  string `json:"lastRun"` // "22m ago · 12 MB uploaded"
	Note     string `json:"note,omitempty"`
	Excludes int    `json:"excludes"`
	// Missing is true when the folder's note says it is not where it was,
	// which only a person can fix, by saying where it went.
	Missing bool `json:"missing,omitempty"`
}

// Recent is one finished run, for the activity list.
type Recent struct {
	Set  string    `json:"set"`
	At   time.Time `json:"at"`
	Day  string    `json:"day"` // "Today", "Yesterday", "Mon 26 Oct"
	Time string    `json:"time"`
	Kind string    `json:"kind"` // ok, failed, cancelled, nochange
	Text string    `json:"text"`

	// The rest is for the run's own page: everything the history keeps.
	Error      string   `json:"error,omitempty"`
	Uploaded   int      `json:"uploaded"`
	Moved      int      `json:"moved"`
	Deleted    int      `json:"deleted"`
	Unchanged  int      `json:"unchanged"`
	Bytes      string   `json:"bytes,omitempty"`
	Took       string   `json:"took,omitempty"`
	Failures   int      `json:"failures"`
	Problems   int      `json:"problems"`
	Collisions int      `json:"collisions"`
	Examples   []string `json:"examples,omitempty"`
}

// Schedule is whether backups run by themselves.
type Schedule struct {
	Available bool   `json:"available"`
	On        bool   `json:"on"`
	Every     int    `json:"every"` // minutes
	EveryText string `json:"everyText"`
	Next      string `json:"next,omitempty"`
}

// recentLimit is how many finished runs the activity list keeps. The
// history file holds more; the panel is for what happened lately.
const recentLimit = 30

// Summarize builds the panel's state from what the dashboard loads and the
// run history.
func Summarize(views []ui.SetView, ov ui.Overview, hist []runstate.Past, now time.Time) State {
	st := State{
		Configured:  ov.Configured,
		Provider:    ov.Provider,
		Destination: ov.Bucket,
		Machine:     ov.Machine,
		Version:     ov.Version,
		At:          now,
		Sched: Schedule{
			Available: ov.SchedulerAvailable,
			On:        ov.Scheduled,
			Every:     int(ov.Interval / time.Minute),
		},
	}
	if st.Provider == "" && st.Configured {
		st.Provider = "r2"
	}
	if ov.Scheduled {
		st.Sched.EveryText = progress.FormatInterval(ov.Interval)
		if !ov.NextRun.IsZero() {
			st.Sched.Next = clock(ov.NextRun, now)
		}
	}

	var bad, never int
	var badName string
	var last time.Time
	for _, v := range views {
		f := Folder{Name: v.Name, Root: v.Root, State: v.State, Note: Plain(v.Note), Excludes: len(v.Excludes)}
		f.Missing = missingNote(v.Note)
		switch v.State {
		case "ok":
			f.Kind = "ok"
		case "needs attention", "failed":
			f.Kind = "attention"
			bad++
			badName = v.Name
		case "never run":
			f.Kind = "never"
			never++
		default:
			f.Kind = "stopped"
		}
		f.LastRun = lastRunText(v, now)
		if v.LastRun.After(last) {
			last = v.LastRun
		}
		st.Folders = append(st.Folders, f)
	}

	if ov.Running != "" {
		// RunPercent is a fraction; the panel wants a percentage.
		st.Running = &Running{Set: ov.Running, Percent: ov.RunPercent * 100, ETA: ov.RunETA}
	}

	st.Status = status(st, ov, len(views), bad, never, last, now)
	if bad == 1 && st.Status.Kind == KindAttention {
		st.Status.Action = "folder:" + badName
	}
	st.Recent = recent(hist, now)
	return st
}

func status(st State, ov ui.Overview, n, bad, never int, last time.Time, now time.Time) Status {
	switch {
	case !st.Configured:
		return Status{Kind: KindSetup, Title: "Not set up yet",
			Detail: "Connect Drime or Cloudflare R2 to start backing up."}
	case st.Running != nil:
		d := "Working out what changed"
		if st.Running.Percent > 0 {
			d = fmt.Sprintf("%.0f%% done", st.Running.Percent)
			if st.Running.ETA != "" {
				d += " · " + st.Running.ETA
			}
		}
		return Status{Kind: KindRunning, Title: "Backing up " + st.Running.Set, Detail: d}
	case ov.BudgetState == "exceeded":
		// Said here, above everything but a running backup: a paused run
		// otherwise reads like a run with nothing to do, and the limit is
		// the one thing that stops every folder at once.
		d := "This month reached your spending limit."
		if ov.BudgetUSD > 0 {
			d = fmt.Sprintf("This month reached your $%.2f limit.", ov.BudgetUSD)
		}
		return Status{Kind: KindAttention, Title: "Backups are paused", Detail: d, Action: "cost"}
	case ov.Interrupted != "":
		return Status{Kind: KindInterrupted, Title: ov.Interrupted + " was interrupted",
			Detail: "It carries on from where it stopped at the next run.", Action: "backup"}
	case n == 0:
		return Status{Kind: KindPending, Title: "Nothing backed up yet",
			Detail: "Add a folder to start."}
	case bad == 1:
		return Status{Kind: KindAttention, Title: "1 folder needs a look", Detail: sinceLast(last, now)}
	case bad > 1:
		return Status{Kind: KindAttention, Title: fmt.Sprintf("%d folders need a look", bad), Detail: sinceLast(last, now)}
	case never == n:
		return Status{Kind: KindPending, Title: "Not backed up yet", Detail: "Back up now, or wait for the next scheduled run."}
	case never > 0:
		return Status{Kind: KindPending, Title: fmt.Sprintf("%d of %d folders backed up", n-never, n), Detail: sinceLast(last, now)}
	}
	return Status{Kind: KindOK, Title: "Your folders are backed up", Detail: sinceLast(last, now)}
}

func sinceLast(last, now time.Time) string {
	if last.IsZero() {
		return ""
	}
	return "Last backup " + ago(now.Sub(last))
}

func recent(hist []runstate.Past, now time.Time) []Recent {
	runs := append([]runstate.Past(nil), hist...)
	sort.SliceStable(runs, func(i, j int) bool { return runs[i].FinishedAt.After(runs[j].FinishedAt) })
	if len(runs) > recentLimit {
		runs = runs[:recentLimit]
	}
	out := make([]Recent, 0, len(runs))
	for _, p := range runs {
		r := Recent{Set: p.Set, At: p.FinishedAt, Day: day(p.FinishedAt, now), Time: p.FinishedAt.Local().Format("15:04"),
			Uploaded: p.Uploaded, Moved: p.Moved, Deleted: p.Deleted, Unchanged: p.Unchanged,
			Failures: p.Failures, Problems: p.Problems, Collisions: p.Collisions, Examples: p.Examples}
		if p.Bytes > 0 {
			r.Bytes = progress.FormatBytes(p.Bytes)
		}
		if p.Duration > 0 {
			r.Took = progress.FormatDuration(time.Duration(p.Duration * float64(time.Second)))
		}
		switch {
		case p.Cancelled:
			r.Kind, r.Text = "cancelled", "Stopped before it finished"
		case p.Error != "":
			r.Kind, r.Text = "failed", Plain(p.Error)
			r.Error = r.Text
		case p.Uploaded+p.Moved+p.Deleted == 0:
			r.Kind, r.Text = "nochange", "No changes"
		default:
			r.Kind, r.Text = "ok", changeText(p)
		}
		if r.Kind == "ok" && p.Failures > 0 {
			r.Kind = "failed"
			r.Text += fmt.Sprintf(" · %d failed", p.Failures)
		}
		if r.Kind == "nochange" && p.Problems > 0 {
			r.Text += fmt.Sprintf(" · %s could not be read", plural(p.Problems, "file"))
		}
		out = append(out, r)
	}
	return out
}

// missingNote reports whether a folder's note is the one a backup leaves
// when the folder is not where it was.
func missingNote(note string) bool {
	return strings.Contains(note, "no longer exists") || strings.Contains(note, "root does not exist")
}

func changeText(p runstate.Past) string {
	var parts []string
	if p.Uploaded > 0 {
		parts = append(parts, fmt.Sprintf("%s uploaded (%s)", plural(p.Uploaded, "file"), progress.FormatBytes(p.Bytes)))
	}
	if p.Moved > 0 {
		parts = append(parts, plural(p.Moved, "file")+" moved")
	}
	if p.Deleted > 0 {
		parts = append(parts, plural(p.Deleted, "file")+" to trash")
	}
	s := ""
	for i, part := range parts {
		if i > 0 {
			s += " · "
		}
		s += part
	}
	return s
}

func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return progress.FormatCount(int64(n)) + " " + noun + "s"
}

func lastRunText(v ui.SetView, now time.Time) string {
	if !v.HasRun {
		return "Not backed up yet"
	}
	when := ago(now.Sub(v.LastRun))
	switch {
	case v.State == "failed":
		return when + " · failed"
	case v.State == "cancelled":
		return when + " · stopped"
	case v.Uploaded == 0 && v.Deleted == 0 && v.Moved == 0:
		return when + " · no changes"
	}
	return when + " · " + progress.FormatBytes(v.Bytes) + " uploaded"
}

func ago(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
	return fmt.Sprintf("%dd ago", int(d.Hours()/24))
}

func day(t, now time.Time) string {
	t, now = t.Local(), now.Local()
	y1, m1, d1 := t.Date()
	y2, m2, d2 := now.Date()
	if y1 == y2 && m1 == m2 && d1 == d2 {
		return "Today"
	}
	if y, m, d := now.AddDate(0, 0, -1).Date(); y1 == y && m1 == m && d1 == d {
		return "Yesterday"
	}
	return t.Format("Mon 2 Jan")
}

// clock is a time the way a person reads one: "15:30" today, "Tue 15:30"
// otherwise.
func clock(t, now time.Time) string {
	if day(t, now) == "Today" {
		return t.Local().Format("15:04")
	}
	return t.Local().Format("Mon 15:04")
}
