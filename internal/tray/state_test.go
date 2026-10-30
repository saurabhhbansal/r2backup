package tray

import (
	"bytes"
	"image/png"
	"strings"
	"testing"
	"time"

	"github.com/saurabhhbansal/r2backup/internal/runstate"
	"github.com/saurabhhbansal/r2backup/internal/ui"
)

var now = time.Date(2026, 10, 27, 15, 0, 0, 0, time.Local)

func TestSummarizeHealthy(t *testing.T) {
	views := []ui.SetView{
		{Name: "Docs", Root: `C:\Docs`, State: "ok", HasRun: true, LastRun: now.Add(-22 * time.Minute), Uploaded: 3, Bytes: 12 << 20},
		{Name: "Photos", Root: `C:\Photos`, State: "ok", HasRun: true, LastRun: now.Add(-2 * time.Hour)},
	}
	ov := ui.Overview{Configured: true, Provider: "drime", Bucket: "Drime · r2backup", Machine: "pc",
		SchedulerAvailable: true, Scheduled: true, Interval: 30 * time.Minute}
	st := Summarize(views, ov, nil, now)
	if st.Status.Kind != KindOK || !strings.Contains(st.Status.Detail, "22m ago") {
		t.Fatalf("status = %+v", st.Status)
	}
	if st.Folders[0].Kind != "ok" || !strings.Contains(st.Folders[0].LastRun, "12 MB uploaded") {
		t.Fatalf("folder = %+v", st.Folders[0])
	}
	if !strings.Contains(st.Folders[1].LastRun, "no changes") {
		t.Fatalf("unchanged folder = %+v", st.Folders[1])
	}
	if !st.Sched.On || st.Sched.Every != 30 || st.Sched.EveryText == "" {
		t.Fatalf("schedule = %+v", st.Sched)
	}
}

func TestSummarizeStatusPriorities(t *testing.T) {
	ok := ui.SetView{Name: "A", State: "ok", HasRun: true, LastRun: now.Add(-time.Hour)}
	bad := ui.SetView{Name: "B", State: "needs attention", Note: "folder moved"}
	never := ui.SetView{Name: "C", State: "never run"}
	cfg := ui.Overview{Configured: true}

	cases := []struct {
		name  string
		views []ui.SetView
		ov    ui.Overview
		want  string
	}{
		{"not set up", nil, ui.Overview{}, KindSetup},
		{"nothing added", nil, cfg, KindPending},
		{"a problem wins over ok", []ui.SetView{ok, bad}, cfg, KindAttention},
		{"never run", []ui.SetView{never}, cfg, KindPending},
		{"partly backed up", []ui.SetView{ok, never}, cfg, KindPending},
		{"running wins over everything", []ui.SetView{ok, bad}, ui.Overview{Configured: true, Running: "A", RunPercent: 0.42}, KindRunning},
		{"interrupted", []ui.SetView{ok}, ui.Overview{Configured: true, Interrupted: "A"}, KindInterrupted},
	}
	for _, c := range cases {
		st := Summarize(c.views, c.ov, nil, now)
		if st.Status.Kind != c.want {
			t.Errorf("%s: kind = %q (%s), want %q", c.name, st.Status.Kind, st.Status.Title, c.want)
		}
	}
	// RunPercent arrives as a fraction and is shown as a percentage.
	st := Summarize([]ui.SetView{ok}, ui.Overview{Configured: true, Running: "A", RunPercent: 0.42, RunETA: "2 min remaining"}, nil, now)
	if st.Running == nil || st.Running.Percent != 42 || !strings.Contains(st.Status.Detail, "42%") {
		t.Fatalf("running = %+v, status %+v", st.Running, st.Status)
	}
}

func TestRecentActivityNewestFirstAndDescribed(t *testing.T) {
	hist := []runstate.Past{
		{Set: "A", FinishedAt: now.Add(-26 * time.Hour), Uploaded: 1, Bytes: 10},
		{Set: "A", FinishedAt: now.Add(-time.Hour), Uploaded: 2, Deleted: 1, Bytes: 2048},
		{Set: "B", FinishedAt: now.Add(-2 * time.Hour), Error: "folder missing"},
		{Set: "B", FinishedAt: now.Add(-3 * time.Hour), Cancelled: true},
		{Set: "C", FinishedAt: now.Add(-4 * time.Hour), Unchanged: 9},
	}
	r := Summarize(nil, ui.Overview{Configured: true}, hist, now).Recent
	if len(r) != 5 || r[0].Set != "A" || r[0].Day != "Today" {
		t.Fatalf("order/day: %+v", r)
	}
	if r[0].Text != "2 files uploaded (2 KB) · 1 file to trash" {
		t.Errorf("text = %q", r[0].Text)
	}
	want := []string{"ok", "failed", "cancelled", "nochange", "ok"}
	for i, k := range want {
		if r[i].Kind != k {
			t.Errorf("run %d kind = %q, want %q", i, r[i].Kind, k)
		}
	}
	if r[4].Day != "Yesterday" {
		t.Errorf("day = %q, want Yesterday", r[4].Day)
	}
}

func TestIconIsAValidICO(t *testing.T) {
	for _, k := range []string{KindOK, KindRunning, KindAttention, KindPending, KindSetup} {
		ico := Icon(k)
		if len(ico) < 22 || ico[2] != 1 || ico[4] != 5 {
			t.Fatalf("%s: bad ICO header", k)
		}
		// The first image's PNG starts where its directory entry says.
		off := int(ico[18]) | int(ico[19])<<8 | int(ico[20])<<16 | int(ico[21])<<24
		if _, err := png.Decode(bytes.NewReader(ico[off:])); err != nil {
			t.Fatalf("%s: first image is not a PNG: %v", k, err)
		}
	}
}

func TestStatusSaysWhatToDo(t *testing.T) {
	ok := ui.SetView{Name: "A", State: "ok", HasRun: true, LastRun: now.Add(-time.Hour)}
	gone := ui.SetView{Name: "Photos", State: "needs attention", Note: "the folder this set points at no longer exists"}

	st := Summarize([]ui.SetView{ok}, ui.Overview{Configured: true, BudgetState: "exceeded", BudgetUSD: 5}, nil, now)
	if st.Status.Kind != KindAttention || st.Status.Action != "cost" || !strings.Contains(st.Status.Detail, "$5.00") {
		t.Errorf("a paused budget: %+v", st.Status)
	}
	st = Summarize([]ui.SetView{ok}, ui.Overview{Configured: true, BudgetState: "exceeded", Running: "A"}, nil, now)
	if st.Status.Kind != KindRunning {
		t.Errorf("a running backup still wins: %+v", st.Status)
	}

	st = Summarize([]ui.SetView{ok, gone}, ui.Overview{Configured: true}, nil, now)
	if st.Status.Action != "folder:Photos" {
		t.Errorf("one folder needing a look should lead to it: %+v", st.Status)
	}
	f := st.Folders[1]
	if !f.Missing || strings.Contains(f.Note, "set points at") {
		t.Errorf("a missing folder: %+v", f)
	}
	two := gone
	two.Name = "Docs"
	if st = Summarize([]ui.SetView{gone, two}, ui.Overview{Configured: true}, nil, now); st.Status.Action != "" {
		t.Errorf("two folders: no single one to open, got %q", st.Status.Action)
	}
	if st = Summarize([]ui.SetView{ok}, ui.Overview{Configured: true, Interrupted: "A"}, nil, now); st.Status.Action != "backup" {
		t.Errorf("an interrupted run can be carried on: %+v", st.Status)
	}
}

func TestRecentRunsKeepTheirDetail(t *testing.T) {
	hist := []runstate.Past{
		{Set: "A", FinishedAt: now.Add(-time.Hour), Uploaded: 3, Bytes: 2048, Failures: 1, Problems: 2,
			Examples: []string{"a.txt: denied"}, Duration: 75},
		{Set: "B", FinishedAt: now.Add(-2 * time.Hour),
			Error: "monthly spending limit reached; backups are paused and nothing was uploaded: about $5.10 of $5.00 this month. To carry on: r2b budget continue"},
	}
	st := Summarize(nil, ui.Overview{Configured: true}, hist, now)
	a, b := st.Recent[0], st.Recent[1]
	if a.Kind != "failed" || a.Failures != 1 || a.Problems != 2 || a.Bytes != "2 KB" || a.Took == "" || len(a.Examples) != 1 {
		t.Errorf("run detail lost: %+v", a)
	}
	if b.Kind != "failed" || strings.Contains(b.Text, "r2b") || b.Error != b.Text {
		t.Errorf("a failed run's words: %+v", b)
	}
}
