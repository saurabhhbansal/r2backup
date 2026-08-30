package ui

import (
	"context"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/saurabhhbansal/r2backup/internal/scan"

	"github.com/saurabhhbansal/r2backup/internal/progress"
)

// fakeBackend is a Backend with no bucket, no credentials and no scheduler
// behind it. Every screen in this package can be driven against it, which is
// the point of Backend being an interface at all.
type fakeBackend struct {
	sets  []SetView
	ov    Overview
	acct  AccountView
	trash []TrashRow

	removed   []string
	sched     []bool
	backups   []string
	added     []AddRequest
	excludes  map[string][]string
	renamed   [][2]string
	relinked  [][2]string
	restores  []RestoreRequest
	keys      []Keys
	emails    []string
	codes     [][2]string
	vaultPw   []string
	signedOut bool

	overlap    string
	scanErr    error
	restoreErr error

	objects     []ObjectRow
	remoteSets  []RemoteSet
	remoteErr   error
	purged      []string
	repaired    int
	hasSchedule bool
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

func (f *fakeBackend) Scan(_ context.Context, root string) (*scan.Result, error) {
	if f.scanErr != nil {
		return nil, f.scanErr
	}
	return newResult(map[string]int64{"a.txt": 1, "sub/b.txt": 2}), nil
}

func (f *fakeBackend) Add(_ context.Context, req AddRequest) error {
	f.added = append(f.added, req)
	return nil
}

func (f *fakeBackend) Overlaps(root string) (string, bool) { return f.overlap, f.overlap != "" }

func (f *fakeBackend) SetExcludes(_ context.Context, name string, ex []string) error {
	if f.excludes == nil {
		f.excludes = map[string][]string{}
	}
	f.excludes[name] = ex
	return nil
}

func (f *fakeBackend) Restore(_ context.Context, req RestoreRequest, phase func(string), snap func(progress.Snapshot)) (RestoreResult, error) {
	f.restores = append(f.restores, req)
	if f.restoreErr != nil {
		return RestoreResult{}, f.restoreErr
	}
	phase("listing")
	snap(progress.Snapshot{BytesDone: 1, BytesTotal: 2})
	return RestoreResult{Files: 3, Bytes: 300, Target: req.To}, nil
}

func (f *fakeBackend) Rename(_ context.Context, from, to string) error {
	f.renamed = append(f.renamed, [2]string{from, to})
	return nil
}

func (f *fakeBackend) Relink(_ context.Context, name, root string) error {
	f.relinked = append(f.relinked, [2]string{name, root})
	return nil
}

func (f *fakeBackend) Trash(context.Context, string) ([]TrashRow, error) { return f.trash, nil }

func (f *fakeBackend) Remove(_ context.Context, name string, purge bool) error {
	f.removed = append(f.removed, name)
	if purge {
		f.purged = append(f.purged, name)
	}
	return nil
}

func (f *fakeBackend) Objects(context.Context, string) ([]ObjectRow, error) {
	return f.objects, nil
}

func (f *fakeBackend) RemoteSets(context.Context) ([]RemoteSet, error) {
	return f.remoteSets, f.remoteErr
}

func (f *fakeBackend) RepairSchedule(context.Context) (bool, error) {
	f.repaired++
	return f.hasSchedule, nil
}

func (f *fakeBackend) Schedule(_ context.Context, _ int, off bool) error {
	f.sched = append(f.sched, !off)
	return nil
}

func (f *fakeBackend) Account(context.Context) (AccountView, error) { return f.acct, nil }

func (f *fakeBackend) SignInStart(_ context.Context, email string) error {
	f.emails = append(f.emails, email)
	return nil
}

func (f *fakeBackend) SignInVerify(_ context.Context, email, code string) error {
	f.codes = append(f.codes, [2]string{email, code})
	f.acct.SignedIn, f.acct.Email = true, email
	return nil
}

func (f *fakeBackend) SignOut(context.Context) error { f.signedOut = true; return nil }

func (f *fakeBackend) UnlockVault(_ context.Context, pw string) error {
	f.vaultPw = append(f.vaultPw, pw)
	return nil
}

func (f *fakeBackend) StoreVault(_ context.Context, pw string) error {
	f.vaultPw = append(f.vaultPw, pw)
	return nil
}

func (f *fakeBackend) SaveKeys(_ context.Context, k Keys) error {
	f.keys = append(f.keys, k)
	return nil
}

func (f *fakeBackend) CheckUpdate(context.Context) (string, error) { return "v9.9.9", nil }
func (f *fakeBackend) ApplyUpdate(context.Context) (string, error) { return "v9.9.9", nil }

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
			Machine: "pc", Bucket: "backups", Configured: true,
			OpsUsed: 42, OpsLimit: 1000000,
			Scheduled: true, Interval: 30 * time.Minute,
			SchedulerAvailable: true,
		},
	}
}

// sized returns a model that has been through a WindowSizeMsg, which is what
// a real program always gets before its first frame.
func sized(b Backend, w, h int) *Model {
	m := New(context.Background(), b)
	m.Update(tea.WindowSizeMsg{Width: w, Height: h})
	f := b.(*fakeBackend)
	m.Update(loadedMsg{sets: f.sets, ov: f.ov})
	return m
}

func press(m *Model, s string) tea.Cmd {
	var cmd tea.Cmd
	if len(s) == 1 {
		_, cmd = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)})
		return cmd
	}
	switch s {
	case "enter":
		_, cmd = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	case "esc":
		_, cmd = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	case "down":
		_, cmd = m.Update(tea.KeyMsg{Type: tea.KeyDown})
	case "tab":
		_, cmd = m.Update(tea.KeyMsg{Type: tea.KeyTab})
	}
	return cmd
}

// apply runs one command and feeds what it produces back into the model, the
// way the bubbletea runtime would.
//
// It gives up after a moment rather than blocking: some of this model's
// commands are meant to block -- the one-second tick, and the reader waiting
// on the worker channel -- and a test that ran those would simply hang.
func apply(t *testing.T, m *Model, cmd tea.Cmd) {
	t.Helper()
	if cmd == nil {
		return
	}
	done := make(chan tea.Msg, 1)
	go func() { done <- cmd() }()
	select {
	case msg := <-done:
		if msg != nil {
			m.Update(msg)
		}
	case <-time.After(2 * time.Second):
	}
}

func TestHomeShowsEverySetAndItsState(t *testing.T) {
	m := sized(twoSets(), 120, 40)
	view := m.View()
	for _, want := range []string{"Documents", "Code", "never run", "backups"} {
		if !strings.Contains(view, want) {
			t.Errorf("home view is missing %q\n---\n%s", want, view)
		}
	}
}

// Whether backups are automatic used to be a standing line in the footer,
// on every screen. It is one of this computer's answers, not one of the
// folder list's, so it moved to the Account tab -- and the assertion moved
// with it rather than being dropped, because "it is said somewhere" is the
// part that matters and "it is said in the footer" was never the point.
func TestTheAccountTabAnswersHowThisComputerIsSetUp(t *testing.T) {
	m := sized(twoSets(), 120, 40)
	m.tab = tabAccount
	view := m.View()
	for _, want := range []string{"This computer", "automatic", "operations this month", "Account"} {
		if !strings.Contains(strings.ToLower(view), strings.ToLower(want)) {
			t.Errorf("account tab is missing %q\n---\n%s", want, view)
		}
	}
	if strings.Contains(m.footer(), "operations") {
		t.Errorf("the footer should carry keys only, not standing status:\n%s", m.footer())
	}
}

// Banner is now a ladder of five rungs rather than one piece of art and a
// fallback, so the narrow-terminal behaviour it used to guard here is
// exercised in much more depth by TestBannerFitsEveryRealisticSize and its
// neighbours in banner_test.go. What is left to check from here is the
// shape of the API a caller outside banner.go sees: something wide and tall
// enough gets art, and something genuinely tiny still falls back to a word
// rather than printing a scrap of a rung that would not read as anything.
func TestTheBannerGivesWayOnANarrowTerminal(t *testing.T) {
	wide := Banner(200, 60)
	if !strings.Contains(wide, "#") {
		t.Error("a terminal wide and tall enough should get art")
	}
	tiny := Banner(15, 8)
	if strings.Contains(tiny, "#") {
		t.Errorf("a 15x8 terminal should not fit even the smallest rung:\n%s", tiny)
	}
	if !strings.Contains(tiny, "r2backup") {
		t.Error("the fallback should still say what this is")
	}
}

// No frame may be wider than the terminal. lipgloss will not wrap for us, and
// one over-wide line turns every row below it into staircase garbage.
func TestNoScreenOverflowsTheTerminal(t *testing.T) {
	for _, size := range [][2]int{{80, 24}, {100, 30}, {150, 45}, {60, 20}} {
		b := twoSets()
		b.trash = []TrashRow{{Key: "notes.txt", Size: 12, Deleted: time.Now(), Expires: time.Now().Add(720 * time.Hour)}}
		m := sized(b, size[0], size[1])

		check := func(what string) {
			for i, line := range strings.Split(m.View(), "\n") {
				if w := lipglossWidth(line); w > size[0] {
					t.Errorf("%dx%d %s: line %d is %d columns wide\n%s",
						size[0], size[1], what, i, w, line)
				}
			}
		}
		// Every tab.
		for t2 := tab(0); t2 < numTabs; t2++ {
			m.tab, m.overlay = t2, overlayNone
			m.layout()
			check(t2.String())
		}
		// And every overlay.
		m.tab, m.overlay = tabFolders, overlayNone
		press(m, "enter")
		check("detail")
		m.overlay = overlayNone
		press(m, "?")
		check("help")
		m.overlay = overlayNone
		press(m, "x")
		check("confirm")
		m.overlay = overlayNone
		press(m, "r")
		check("restore form")
		m.overlay = overlayNone
		m.Update(trashMsg{set: "Documents", rows: b.trash})
		check("trash")
	}
}

func TestBackingUpFromTheListRunsTheSelectedSet(t *testing.T) {
	b := twoSets()
	m := sized(b, 120, 40)
	press(m, "b")
	if !m.running {
		t.Fatal("pressing b should start a backup")
	}
	if m.overlay != overlayRunning {
		t.Errorf("overlay = %v, want the running screen", m.overlay)
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
	if m.overlay != overlayConfirm {
		t.Fatal("x should ask before removing anything")
	}
	if len(b.removed) != 0 {
		t.Fatal("x removed a set before the question was answered")
	}
	press(m, "n")
	if len(b.removed) != 0 {
		t.Fatal("answering no still removed the set")
	}
	if m.overlay != overlayNone {
		t.Error("answering should close the question")
	}
}

// Typing into the list's filter must not be read as commands. "b" is the
// backup key and also the first letter of a great many folder names.
func TestFilterTextIsNotTakenAsCommands(t *testing.T) {
	b := twoSets()
	m := sized(b, 120, 40)
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("/")})
	if !m.list.SettingFilter() {
		t.Fatal("/ should open the list filter")
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
func TestASecondJobCannotStartOnTopOfARunningOne(t *testing.T) {
	b := twoSets()
	m := sized(b, 120, 40)
	press(m, "b")
	if !m.running {
		t.Fatal("b should have started a backup")
	}
	press(m, "esc")
	press(m, "r")
	if m.overlay == overlayForm {
		t.Fatal("a second job was started on top of a running one")
	}
	if !strings.Contains(m.notice, "already running") {
		t.Errorf("the user should be told why nothing happened, got %q", m.notice)
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
				if m.runWhat != string(s) {
					t.Errorf("runSet = %q, want %q", m.runWhat, string(s))
				}
			}
		case <-deadline:
			t.Fatal("the batch never finished")
		}
	}
}

// mustScan is the tree the fake hands back for any folder.
func (f *fakeBackend) mustScan() *scan.Result {
	return newResult(map[string]int64{"a.txt": 1, "sub/b.txt": 2})
}
