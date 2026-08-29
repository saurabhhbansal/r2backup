package ui

import (
	"context"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/saurabhhbansal/r2backup/internal/progress"
)

// fakeBackend is a Backend with no bucket, no credentials and no scheduler
// behind it. Every screen in this package can be driven against it, which is
// the point of Backend being an interface at all.
type fakeBackend struct {
	sets    []SetView
	ov      Overview
	trash   []TrashRow
	removed []string
	sched   []bool
	backups []string
}

func (f *fakeBackend) Load(context.Context) ([]SetView, Overview, error) {
	return f.sets, f.ov, nil
}

func (f *fakeBackend) Backup(_ context.Context, name string, phase func(string), snap func(progress.Snapshot)) error {
	f.backups = append(f.backups, name)
	phase("scanning " + name)
	snap(progress.Snapshot{BytesDone: 5, BytesTotal: 10, FilesDone: 1, FilesTotal: 2})
	return nil
}

func (f *fakeBackend) Trash(context.Context, string) ([]TrashRow, error) { return f.trash, nil }

func (f *fakeBackend) Remove(_ context.Context, name string, _ bool) error {
	f.removed = append(f.removed, name)
	return nil
}

func (f *fakeBackend) Schedule(_ context.Context, _ int, off bool) error {
	f.sched = append(f.sched, !off)
	return nil
}

func twoSets() *fakeBackend {
	return &fakeBackend{
		sets: []SetView{
			{
				Name: "Documents", Root: "/home/me/Documents", Prefix: "machines/pc/Documents",
				State: "ok", HasRun: true, LastRun: time.Now().Add(-90 * time.Minute),
				Uploaded: 3, Unchanged: 120, Bytes: 4096, Operations: 6, Objects: 123,
				Retention: 30,
			},
			{
				Name: "Code", Root: "/home/me/code", Prefix: "machines/pc/Code",
				State: "never run", Excludes: []string{"node_modules"}, Retention: 30,
			},
		},
		ov: Overview{
			Machine: "pc", Bucket: "backups",
			OpsUsed: 42, OpsLimit: 1000000,
			Scheduled: true, Interval: 30 * time.Minute,
		},
	}
}

// sized returns a model that has been through a WindowSizeMsg, which is what
// a real program always gets before its first frame.
func sized(b Backend, w, h int) *Model {
	m := New(context.Background(), b)
	m.Update(tea.WindowSizeMsg{Width: w, Height: h})
	m.Update(loadedMsg{sets: b.(*fakeBackend).sets, ov: b.(*fakeBackend).ov})
	return m
}

func press(m *Model, s string) {
	if len(s) == 1 {
		m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)})
		return
	}
	switch s {
	case "enter":
		m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	case "esc":
		m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	case "down":
		m.Update(tea.KeyMsg{Type: tea.KeyDown})
	}
}

func TestHomeShowsEverySetAndItsState(t *testing.T) {
	m := sized(twoSets(), 120, 40)
	view := m.View()
	for _, want := range []string{"Documents", "Code", "never run", "backups", "automatic"} {
		if !strings.Contains(view, want) {
			t.Errorf("home view is missing %q\n---\n%s", want, view)
		}
	}
}

// The art is 139 columns wide. Drawn into an 80-column terminal every line
// would wrap in the middle and the logo would become noise, so a narrow
// window gets the plain title instead.
func TestTheBannerGivesWayOnANarrowTerminal(t *testing.T) {
	wide := Banner(bannerWidth+10, bannerHeight+20)
	if !strings.Contains(wide, "#") {
		t.Error("a terminal wide enough should get the art")
	}
	narrow := Banner(80, 40)
	if strings.Contains(narrow, "#") {
		t.Errorf("an 80-column terminal should not get 139-column art:\n%s", narrow)
	}
	if !strings.Contains(narrow, "r2backup") {
		t.Error("the fallback should still say what this is")
	}
	short := Banner(200, 10)
	if strings.Contains(short, "#") {
		t.Error("a short window should not spend every row on the banner")
	}
}

// No frame may be wider than the terminal. lipgloss will not wrap for us, and
// one over-wide line turns every row below it into staircase garbage.
func TestNoScreenOverflowsTheTerminal(t *testing.T) {
	for _, size := range [][2]int{{80, 24}, {100, 30}, {150, 45}, {60, 20}} {
		b := twoSets()
		m := sized(b, size[0], size[1])
		for _, screen := range []string{"home", "detail", "trash", "help", "confirm"} {
			switch screen {
			case "detail":
				press(m, "enter")
			case "trash":
				m.Update(trashMsg{set: "Documents", rows: []TrashRow{
					{Key: "notes.txt", Size: 12, Deleted: time.Now(), Expires: time.Now().Add(720 * time.Hour)},
				}})
			case "help":
				press(m, "?")
			case "confirm":
				m.screen = screenHome
				press(m, "x")
			}
			for i, line := range strings.Split(m.View(), "\n") {
				if w := lipglossWidth(line); w > size[0] {
					t.Errorf("%dx%d %s: line %d is %d columns wide\n%s",
						size[0], size[1], screen, i, w, line)
				}
			}
			m.screen = screenHome
		}
	}
}

func TestBackingUpFromTheListRunsTheSelectedSet(t *testing.T) {
	b := twoSets()
	m := sized(b, 120, 40)
	press(m, "b")
	if !m.running {
		t.Fatal("pressing b should start a backup")
	}
	if m.screen != screenRunning {
		t.Errorf("screen = %v, want the running screen", m.screen)
	}
	// The goroutine posts onto the events channel; drain what it sends.
	deadline := time.After(2 * time.Second)
	var done bool
	for !done {
		select {
		case msg := <-m.events:
			if d, ok := msg.(runDoneMsg); ok {
				if d.err != nil {
					t.Fatalf("backup reported %v", d.err)
				}
				done = true
			}
		case <-deadline:
			t.Fatal("the backup never finished")
		}
	}
	if len(b.backups) != 1 || b.backups[0] != "Documents" {
		t.Errorf("backed up %v, want [Documents]", b.backups)
	}
}

// Nothing that removes a folder from the backup happens on one keystroke.
func TestRemovingAsksFirst(t *testing.T) {
	b := twoSets()
	m := sized(b, 120, 40)
	press(m, "x")
	if m.screen != screenConfirm {
		t.Fatal("x should ask before removing anything")
	}
	if len(b.removed) != 0 {
		t.Fatal("x removed a set before the question was answered")
	}
	press(m, "n")
	if len(b.removed) != 0 {
		t.Fatal("answering no still removed the set")
	}
	if m.screen != screenHome {
		t.Error("answering should close the question")
	}
}

// Typing into the list's filter must not be read as commands. "b" is the
// backup key and also the first letter of a great many folder names.
func TestFilterTextIsNotTakenAsCommands(t *testing.T) {
	b := twoSets()
	m := sized(b, 120, 40)
	press(m, "/")
	if !m.list.SettingFilter() {
		t.Skip("this list build does not open a filter on /")
	}
	press(m, "b")
	if m.running {
		t.Fatal("typing 'b' into the filter box started a backup")
	}
}

// lipglossWidth measures display cells, not bytes: a styled line is mostly
// escape sequences and counting those would make every check meaningless.
func lipglossWidth(s string) int { return lipgloss.Width(s) }

// The frame must fill the terminal and never exceed it. It exceeded it by one
// row for every size at once -- the panel's two border rows were counted as
// one -- and the row that fell off the bottom was the keyboard help.
func TestTheFrameFitsTheTerminalExactly(t *testing.T) {
	for _, size := range [][2]int{{80, 24}, {100, 30}, {150, 34}, {150, 46}, {60, 20}} {
		m := sized(twoSets(), size[0], size[1])
		got := lipgloss.Height(m.View())
		if got > size[1] {
			t.Errorf("%dx%d: frame is %d rows, taller than the terminal", size[0], size[1], got)
		}
		if got < size[1] {
			t.Errorf("%dx%d: frame is %d rows, leaving %d blank at the bottom",
				size[0], size[1], got, size[1]-got)
		}
		if !strings.Contains(m.View(), "quit") {
			t.Errorf("%dx%d: the keyboard help is not on screen", size[0], size[1])
		}
	}
}

// Leaving the interface mid-run used to abandon the backup: the goroutine
// keeps uploading and keeps posting onto a channel with no reader, fills the
// buffer and blocks forever, so the run stops silently and partway. `edit`
// also ends by running a backup of its own, which would then contend for the
// same bbolt writer lock.
func TestYouCannotWalkOutOnARunningBackup(t *testing.T) {
	b := twoSets()
	m := sized(b, 120, 40)
	press(m, "b")
	if !m.running {
		t.Fatal("b should have started a backup")
	}
	for _, k := range []string{"a", "e", "r"} {
		m.screen = screenHome
		press(m, k)
		if m.quit {
			t.Fatalf("%q left the interface while a backup was running", k)
		}
		if m.Action().Kind != ActionNone {
			t.Fatalf("%q queued %v while a backup was running", k, m.Action().Kind)
		}
	}
	if !strings.Contains(m.status, "backup is running") {
		t.Errorf("the user should be told why nothing happened, got %q", m.status)
	}
}

// A backup goroutine must never block on a channel nobody is reading. If it
// does, the upload it is in the middle of stops there.
func TestProgressIsDroppedRatherThanBlocking(t *testing.T) {
	b := twoSets()
	m := sized(b, 120, 40)
	// Fill the buffer so any blocking send would deadlock.
	for len(m.events) < cap(m.events) {
		m.events <- phaseMsg("filler")
	}
	press(m, "b")

	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case msg := <-m.events:
				if _, ok := msg.(runDoneMsg); ok {
					return
				}
			case <-time.After(3 * time.Second):
				return
			}
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the backup goroutine wedged on a full channel")
	}
	if len(b.backups) != 1 {
		t.Errorf("the backup did not run: %v", b.backups)
	}
}

// `B` walks every set. The running screen named only the first one for the
// whole batch until runSetMsg existed.
func TestBackingUpEverythingNamesEachSetAsItGoes(t *testing.T) {
	b := twoSets()
	m := sized(b, 120, 40)
	press(m, "B")

	seen := map[string]bool{}
	deadline := time.After(3 * time.Second)
	for {
		select {
		case msg := <-m.events:
			m.Update(msg)
			if _, ok := msg.(runDoneMsg); ok {
				if !seen["Documents"] || !seen["Code"] {
					t.Fatalf("the title named %v, want both sets", seen)
				}
				return
			}
			if s, ok := msg.(runSetMsg); ok {
				seen[string(s)] = true
				if m.runSet != string(s) {
					t.Errorf("runSet = %q, want %q", m.runSet, string(s))
				}
			}
		case <-deadline:
			t.Fatal("the batch never finished")
		}
	}
}
