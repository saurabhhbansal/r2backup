package ui

import (
	"errors"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// type returns each character of s as its own key message, the way a person
// typing into a form produces them.
func typeIn(m *Model, s string) {
	for _, r := range s {
		m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
}

func enter(m *Model) tea.Cmd {
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	return cmd
}

// drain runs the worker channel to completion, applying every message the way
// the bubbletea loop would.
func drain(t *testing.T, m *Model) {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		select {
		case msg := <-m.events:
			m.Update(msg)
			switch msg.(type) {
			case runDoneMsg, restoreDoneMsg:
				return
			}
		case <-deadline:
			t.Fatal("the job never finished")
		}
	}
}

// Every one of these was a sentence telling the user to leave and type a
// command. The point of the tests is that none of them is any more.

func TestAddingAFolderNeverLeavesTheWindow(t *testing.T) {
	b := twoSets()
	m := sized(b, 120, 40)

	press(m, "a")
	if m.overlay != overlayBrowse {
		t.Fatalf("a should open the folder browser, got overlay %v", m.overlay)
	}

	// Choosing the folder you are standing in, then the tree picker.
	m.Update(scannedMsg{root: "/home/me/work", name: "work", res: b.mustScan(), req: m.request})
	if m.overlay != overlayPicker {
		t.Fatalf("a scanned folder should open the picker, got %v", m.overlay)
	}
	if m.picker == nil {
		t.Fatal("no picker model was built")
	}

	// Accept everything.
	apply(t, m, enter(m))
	if m.overlay != overlayForm {
		t.Fatalf("accepting the picker should ask for a name, got %v", m.overlay)
	}

	// The name field opens prefilled with the folder's own name and the
	// cursor at the end, so typing appends. Clear it to replace.
	m.Update(tea.KeyMsg{Type: tea.KeyCtrlU})
	typeIn(m, "Work")
	enter(m)              // move to the retention field
	apply(t, m, enter(m)) // submit; registers the folder
	drain(t, m)           // and backs it up

	if len(b.added) != 1 {
		t.Fatalf("added %v, want one folder", b.added)
	}
	if b.added[0].Name != "Work" {
		t.Errorf("added name = %q, want Work", b.added[0].Name)
	}
	if b.added[0].Root != "/home/me/work" {
		t.Errorf("added root = %q, want /home/me/work", b.added[0].Root)
	}
	// And it is backed up straight away, rather than sitting there added but
	// never uploaded.
	if len(b.backups) == 0 {
		t.Error("a newly added folder was not backed up")
	}
}

func TestEditingReopensThePickerOnTheCurrentSelection(t *testing.T) {
	b := twoSets()
	b.sets[1].Excludes = []string{"sub"}
	m := sized(b, 120, 40)
	m.list.Select(1) // Code, which excludes "sub"

	press(m, "e")
	m.Update(scannedMsg{root: b.sets[1].Root, name: "Code", res: b.mustScan(), editing: "Code", req: m.request})
	if m.overlay != overlayPicker {
		t.Fatalf("e should open the picker, got %v", m.overlay)
	}
	// Accepting untouched must preserve the exclude, not wipe it. If the
	// picker opened on a blank slate this returns nothing.
	apply(t, m, enter(m))
	if got := b.excludes["Code"]; len(got) != 1 || got[0] != "sub" {
		t.Fatalf("accepting the picker untouched gave excludes %v, want [sub]", got)
	}
}

func TestRestoreAsksWhereAndThenRuns(t *testing.T) {
	b := twoSets()
	m := sized(b, 120, 40)

	press(m, "r")
	if m.overlay != overlayForm {
		t.Fatalf("r should ask where to restore, got %v", m.overlay)
	}
	typeIn(m, "/tmp/out")
	enter(m) // to "only"
	enter(m) // to "replace existing"
	enter(m) // to "re-read each file after writing"
	enter(m) // submit
	drain(t, m)

	if len(b.restores) != 1 {
		t.Fatalf("restores = %v, want one", b.restores)
	}
	if b.restores[0].Set != "Documents" || b.restores[0].To != "/tmp/out" {
		t.Errorf("restored %+v, want Documents into /tmp/out", b.restores[0])
	}
	if !strings.Contains(m.notice, "restored") {
		t.Errorf("the result should be reported, got %q", m.notice)
	}
}

func TestRenamingHappensInPlace(t *testing.T) {
	b := twoSets()
	m := sized(b, 120, 40)
	press(m, "n")
	if m.overlay != overlayForm {
		t.Fatalf("n should open a rename form, got %v", m.overlay)
	}
	// The field starts on the current name, so this is an edit not a retype.
	m.Update(tea.KeyMsg{Type: tea.KeyCtrlU})
	typeIn(m, "Papers")
	apply(t, m, enter(m))
	if len(b.renamed) != 1 || b.renamed[0] != [2]string{"Documents", "Papers"} {
		t.Fatalf("renamed = %v, want Documents -> Papers", b.renamed)
	}
}

func TestSchedulingIsAModeYouCanLookAt(t *testing.T) {
	b := twoSets()
	m := sized(b, 120, 40)
	m.tab = tabSchedule
	m.layout()

	view := m.View()
	for _, want := range []string{"Automatic backups", "every 30m", "turn off", "change how often"} {
		if !strings.Contains(view, want) {
			t.Errorf("the schedule mode is missing %q\n---\n%s", want, view)
		}
	}

	apply(t, m, press(m, "s")) // turn it off
	if len(b.sched) != 1 || b.sched[0] {
		t.Fatalf("s should have turned the schedule off, got %v", b.sched)
	}

	press(m, "e") // change the interval
	if m.overlay != overlayForm {
		t.Fatalf("e should ask for a new interval, got %v", m.overlay)
	}
	m.Update(tea.KeyMsg{Type: tea.KeyCtrlU})
	typeIn(m, "15")
	apply(t, m, enter(m))
	if len(b.sched) != 2 || !b.sched[1] {
		t.Fatalf("submitting an interval should register it, got %v", b.sched)
	}
}

func TestSigningInHappensInTheWindow(t *testing.T) {
	b := twoSets()
	b.ov.Configured = false
	m := sized(b, 120, 40)
	m.tab = tabAccount
	m.layout()

	press(m, "i")
	if m.overlay != overlayForm {
		t.Fatalf("i should open the sign-in form, got %v", m.overlay)
	}
	typeIn(m, "me@example.com")
	apply(t, m, enter(m))
	if m.overlay != overlayForm {
		t.Fatal("after the code is sent the window should ask for it")
	}
	typeIn(m, "123456")
	apply(t, m, enter(m))

	if len(b.emails) != 1 || b.emails[0] != "me@example.com" {
		t.Errorf("requested a code for %v", b.emails)
	}
	if len(b.codes) != 1 || b.codes[0] != [2]string{"me@example.com", "123456"} {
		t.Errorf("verified %v", b.codes)
	}
}

func TestR2KeysCanBeTypedInTheWindow(t *testing.T) {
	b := twoSets()
	b.ov.Configured = false
	m := sized(b, 120, 40)
	m.tab = tabAccount
	m.layout()

	press(m, "k")
	if m.overlay != overlayForm {
		t.Fatalf("k should open the keys form, got %v", m.overlay)
	}
	for i, v := range []string{"acct", "keyid", "secret", "bucket"} {
		typeIn(m, v)
		if i == 3 {
			apply(t, m, enter(m))
		} else {
			enter(m)
		}
	}
	if len(b.keys) != 1 {
		t.Fatalf("keys = %v, want one set", b.keys)
	}
	got := b.keys[0]
	if got.AccountID != "acct" || got.AccessKeyID != "keyid" || got.Secret != "secret" || got.Bucket != "bucket" {
		t.Errorf("keys came through as %+v", got)
	}
}

// An unconfigured machine must not open onto an empty folder list with no
// explanation. It is the first thing a new user sees.
func TestAnUnconfiguredMachineIsToldWhereToGo(t *testing.T) {
	b := twoSets()
	b.ov.Configured = false
	b.sets = nil
	m := sized(b, 120, 40)
	view := m.View()
	if !strings.Contains(view, "not set up yet") || !strings.Contains(view, "Account") {
		t.Errorf("an unconfigured machine should be pointed at the Account tab:\n%s", view)
	}
}

func TestTabsMoveBetweenModes(t *testing.T) {
	m := sized(twoSets(), 120, 40)
	for want := tab(1); want < numTabs; want++ {
		press(m, "tab")
		if m.tab != want {
			t.Fatalf("tab moved to %v, want %v", m.tab, want)
		}
	}
	press(m, "tab")
	if m.tab != tabFolders {
		t.Errorf("tab should wrap round to Folders, got %v", m.tab)
	}
	// And the numbers jump straight there.
	press(m, "3")
	if m.tab != tabTrash {
		t.Errorf("3 should go to Trash, got %v", m.tab)
	}
}

// A scan of a big folder takes a while, and the user may have moved on. A
// result that arrives after they cancelled or changed tab must be dropped,
// not painted over whatever they are looking at now.
func TestAStaleScanResultIsDropped(t *testing.T) {
	b := twoSets()
	m := sized(b, 120, 40)

	press(m, "a")                                  // browse
	apply(t, m, m.scanFolder("/home/me/work", "")) // request 1 in flight
	stale := m.request
	press(m, "esc") // changed their mind
	press(m, "4")   // and went to Account

	m.Update(scannedMsg{root: "/home/me/work", res: b.mustScan(), req: stale - 1})
	if m.overlay == overlayPicker {
		t.Fatal("a scan the user had moved on from reopened the picker over them")
	}
	if m.tab != tabAccount {
		t.Errorf("tab = %v, want the one the user chose", m.tab)
	}
}

// Likewise a trash listing that lands after the user left the tab.
func TestAStaleTrashListingDoesNotDragYouBack(t *testing.T) {
	b := twoSets()
	m := sized(b, 120, 40)
	m.request = 5
	m.tab = tabFolders

	m.Update(trashMsg{set: "Documents", rows: b.trash, req: 4})
	if m.tab == tabTrash {
		t.Fatal("a stale trash listing pulled the user onto the Trash tab")
	}
}

// An error from a background reload must not be read as "the backup stopped".
// It used to clear m.running, which un-tracked a live transfer and let a
// second one start on top of it.
func TestABackgroundErrorDoesNotUntrackARunningJob(t *testing.T) {
	b := twoSets()
	m := sized(b, 120, 40)
	press(m, "b")
	if !m.running {
		t.Fatal("b should have started a backup")
	}

	m.Update(errMsg{errors.New("open index: timeout")})

	if !m.running {
		t.Fatal("a refresh error cleared m.running while the backup was still going")
	}
	if m.runCancel == nil {
		t.Fatal("the run's cancel was dropped, so q could never stop it")
	}
	// And a second job is still refused. Asserted on the refusal rather than
	// on a call count: the first backup runs on a goroutine that may not
	// have reached the fake yet, which would make a count flaky.
	m.overlay = overlayNone
	press(m, "b")
	if !strings.Contains(m.notice, "already running") {
		t.Fatalf("a second backup was not refused; notice = %q", m.notice)
	}
	drain(t, m)
	if len(b.backups) != 1 {
		t.Fatalf("backups = %v, want exactly one", b.backups)
	}
}

// The detail screen prints its own list of keys. They have to work.
func TestTheDetailScreenKeysDoWhatTheySay(t *testing.T) {
	b := twoSets()
	m := sized(b, 120, 40)
	press(m, "enter")
	if m.overlay != overlayDetail {
		t.Fatalf("enter should open the detail screen, got %v", m.overlay)
	}
	if !strings.Contains(m.View(), "r restore") {
		t.Fatal("the detail screen no longer advertises these keys; update this test")
	}
	press(m, "r")
	if m.overlay != overlayForm {
		t.Fatalf("r on the detail screen should open the restore form, got %v", m.overlay)
	}
	press(m, "esc")

	press(m, "enter")
	press(m, "n")
	if m.overlay != overlayForm {
		t.Fatalf("n on the detail screen should open the rename form, got %v", m.overlay)
	}
}

// A restore must show the progress screen. The form's own "done" handling
// used to overwrite the overlay the submit had just set, so every restore ran
// invisibly behind the folder list.
func TestARestoreShowsItsProgress(t *testing.T) {
	b := twoSets()
	m := sized(b, 120, 40)
	press(m, "r")
	typeIn(m, "/tmp/out")
	enter(m)
	enter(m)
	enter(m)
	enter(m) // submit
	if m.overlay != overlayRunning {
		t.Fatalf("overlay = %v, want the progress screen", m.overlay)
	}
	if !m.running {
		t.Fatal("the restore is not tracked as running")
	}
	drain(t, m)
}

// ctrl+c has to leave from anywhere: bubbletea turns off ISIG, so it is an
// ordinary key and an overlay that ignores it is one you cannot escape.
func TestCtrlCLeavesFromEveryOverlay(t *testing.T) {
	for _, open := range []struct {
		name string
		do   func(*Model)
	}{
		{"form", func(m *Model) { press(m, "r") }},
		{"confirm", func(m *Model) { press(m, "x") }},
		{"help", func(m *Model) { press(m, "?") }},
		{"detail", func(m *Model) { press(m, "enter") }},
		{"browse", func(m *Model) { press(m, "a") }},
	} {
		m := sized(twoSets(), 120, 40)
		open.do(m)
		if m.overlay == overlayNone {
			t.Fatalf("%s: the overlay did not open", open.name)
		}
		m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
		if !m.quit {
			t.Errorf("%s: ctrl+c did not quit", open.name)
		}
	}
}

// A run left with esc must still be visible, and reachable again.
func TestAnEscapedRunStaysVisible(t *testing.T) {
	b := twoSets()
	m := sized(b, 120, 40)
	press(m, "b")
	press(m, "esc")
	if m.overlay != overlayNone {
		t.Fatalf("esc should return to the list, got %v", m.overlay)
	}
	if !strings.Contains(m.View(), "backing up") {
		t.Errorf("a running backup left the screen with no sign of it:\n%s", m.View())
	}
	press(m, "w")
	if m.overlay != overlayRunning {
		t.Errorf("w should return to the progress screen, got %v", m.overlay)
	}
	drain(t, m)
}

// Opening and closing an overlay rebuilds the trash table. The cursor must
// survive it, and a window resize too.
func TestTheTrashCursorSurvivesARebuild(t *testing.T) {
	b := twoSets()
	rows := make([]TrashRow, 8)
	for i := range rows {
		rows[i] = TrashRow{Key: "f" + string(rune('a'+i)) + ".txt", Size: 1, Deleted: time.Now(), Expires: time.Now()}
	}
	b.trash = rows
	m := sized(b, 120, 40)
	m.Update(trashMsg{set: "Documents", rows: rows, req: m.request})
	m.trash.SetCursor(5)

	press(m, "?")   // open help — triggers layout()
	press(m, "esc") // and close it
	if got := m.trash.Cursor(); got != 5 {
		t.Errorf("cursor = %d after opening and closing help, want 5", got)
	}
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	if got := m.trash.Cursor(); got != 5 {
		t.Errorf("cursor = %d after a resize, want 5", got)
	}
}

// A name sets.ValidName will reject has to be caught while the form is still
// open, not after it has closed and taken the whole flow with it.
func TestABadNameIsCaughtInTheForm(t *testing.T) {
	b := twoSets()
	m := sized(b, 120, 40)
	press(m, "a")
	m.Update(scannedMsg{root: "/home/me/work", name: "work", res: b.mustScan(), req: m.request})
	apply(t, m, enter(m))

	m.Update(tea.KeyMsg{Type: tea.KeyCtrlU})
	typeIn(m, "bad/name")
	enter(m)
	apply(t, m, enter(m))

	if m.overlay != overlayForm {
		t.Fatal("a bad name closed the form instead of saying so")
	}
	if len(b.added) != 0 {
		t.Fatalf("a bad name reached the backend: %v", b.added)
	}
	if !strings.Contains(m.form.err, "will not work") {
		t.Errorf("the form should explain the problem, got %q", m.form.err)
	}
}

// The rest of the command line, reached from the window. Each of these was a
// command with no key: `r2b ls`, `r2b restore --machine`, `r2b restore
// --verify`, `r2b remove --purge`, `r2b schedule --repair`. internal/cli's
// coverage test asserts the mapping exists; these assert it works.

func TestFListsWhatIsStored(t *testing.T) {
	b := twoSets()
	b.objects = []ObjectRow{
		{Key: "notes/big.psd", Size: 900_000},
		{Key: "a.txt", Size: 12},
	}
	m := sized(b, 120, 40)

	apply(t, m, press(m, "f"))
	if m.overlay != overlayObjects {
		t.Fatalf("f should list what is stored, got overlay %v", m.overlay)
	}
	if m.objectSet != "Documents" {
		t.Errorf("listed %q, want the highlighted folder", m.objectSet)
	}
	body := m.bodyView()
	for _, want := range []string{"notes/big.psd", "a.txt", "2 objects"} {
		if !strings.Contains(body, want) {
			t.Errorf("the listing does not mention %q:\n%s", want, body)
		}
	}
	press(m, "esc")
	if m.overlay != overlayNone {
		t.Error("esc should close the listing")
	}
}

// The case this exists for: a computer that has just signed in has working
// credentials and an empty folder list, and the data it wants is in the
// bucket under another machine's name. Nothing on screen used to say so.
func TestCFindsAnotherComputersBackupAndRestoresIt(t *testing.T) {
	b := twoSets()
	b.sets = nil
	b.remoteSets = []RemoteSet{{Name: "Documents", Machine: "laptop"}}
	m := sized(b, 120, 40)

	apply(t, m, press(m, "c"))
	if m.overlay != overlayRemote {
		t.Fatalf("c should list the bucket, got overlay %v", m.overlay)
	}
	if !strings.Contains(m.bodyView(), "laptop") {
		t.Fatalf("the listing does not name the machine:\n%s", m.bodyView())
	}

	press(m, "enter")
	if m.overlay != overlayForm {
		t.Fatalf("enter should open the restore form, got overlay %v", m.overlay)
	}
	// It has no original path on this computer, so submitting with no
	// destination has to be refused rather than guessed at.
	for i := 0; i < 4; i++ {
		enter(m)
	}
	if m.overlay != overlayForm {
		t.Fatal("a restore with nowhere to go should not have started")
	}
	if len(b.restores) != 0 {
		t.Fatalf("a restore ran with nowhere to put it: %v", b.restores)
	}

	// Back up to the first field, the way a person would after being told.
	for i := 0; i < 3; i++ {
		m.Update(tea.KeyMsg{Type: tea.KeyUp})
	}
	typeIn(m, "/tmp/from-laptop")
	for i := 0; i < 4; i++ {
		enter(m)
	}
	drain(t, m)

	if len(b.restores) != 1 {
		t.Fatalf("restores = %v, want one", b.restores)
	}
	got := b.restores[0]
	if got.Machine != "laptop" || got.To != "/tmp/from-laptop" || got.Set != "Documents" {
		t.Errorf("restored %+v, want Documents from laptop into /tmp/from-laptop", got)
	}
}

func TestTheRestoreFormCanAskForVerification(t *testing.T) {
	b := twoSets()
	m := sized(b, 120, 40)

	press(m, "r")
	typeIn(m, "/tmp/out")
	enter(m) // to "only"
	enter(m) // to "replace existing"
	enter(m) // to "re-read each file after writing"
	typeIn(m, "yes")
	enter(m) // submit
	drain(t, m)

	if len(b.restores) != 1 {
		t.Fatalf("restores = %v, want one", b.restores)
	}
	if !b.restores[0].Verify {
		t.Errorf("restored %+v, want Verify set", b.restores[0])
	}
}

// Purging is the most destructive thing the program does, and the command
// makes typing --purge the confirmation. The window asks for the folder's
// name for the same reason: a y/N everyone learns to answer "y" to is not a
// safety feature.
func TestPurgeNeedsTheNameTypedBack(t *testing.T) {
	b := twoSets()
	m := sized(b, 120, 40)

	press(m, "X")
	if m.overlay != overlayForm {
		t.Fatalf("X should ask for confirmation, got overlay %v", m.overlay)
	}
	typeIn(m, "Documnets") // a typo
	apply(t, m, enter(m))
	if len(b.purged) != 0 {
		t.Fatalf("a mistyped name purged %v", b.purged)
	}
	if m.overlay != overlayForm {
		t.Fatal("the form should still be open after a mistyped name")
	}

	for range "Documnets" {
		m.Update(tea.KeyMsg{Type: tea.KeyBackspace})
	}
	typeIn(m, "Documents")
	apply(t, m, enter(m))

	if len(b.purged) != 1 || b.purged[0] != "Documents" {
		t.Fatalf("purged = %v, want [Documents]", b.purged)
	}
}

// x is still the one that keeps what is in the bucket. The two must not be
// the same key with a different question.
func TestXRemovesWithoutPurging(t *testing.T) {
	b := twoSets()
	m := sized(b, 120, 40)

	press(m, "x")
	apply(t, m, press(m, "y"))

	if len(b.removed) != 1 || b.removed[0] != "Documents" {
		t.Fatalf("removed = %v, want [Documents]", b.removed)
	}
	if len(b.purged) != 0 {
		t.Errorf("x purged %v, and it must not", b.purged)
	}
}

func TestPRepairsTheSchedule(t *testing.T) {
	b := twoSets()
	b.hasSchedule = true
	m := sized(b, 120, 40)

	press(m, "2")
	apply(t, m, press(m, "p"))

	if b.repaired != 1 {
		t.Fatalf("repaired %d times, want once", b.repaired)
	}
	if !strings.Contains(m.notice, "Re-pointed") {
		t.Errorf("notice = %q, want it to say what happened", m.notice)
	}
}

// A machine with no schedule must be left with no schedule: repairing must
// never quietly start backing things up on a timer nobody asked for.
func TestRepairingNothingSaysSoAndSchedulesNothing(t *testing.T) {
	b := twoSets()
	b.hasSchedule = false
	m := sized(b, 120, 40)

	press(m, "2")
	apply(t, m, press(m, "p"))

	if len(b.sched) != 0 {
		t.Fatalf("repair registered a schedule: %v", b.sched)
	}
	if !strings.Contains(m.notice, "nothing to repair") {
		t.Errorf("notice = %q, want it to say there was nothing to repair", m.notice)
	}
}

// The footer help is trimmed to the room there is, and the way out survives
// every width. It used to be cut at two fixed widths, so every key added to
// a mode made the line longer without moving where it was cut -- and
// bubbles/help cuts the tail, which is where "q quit" lives.
func TestTheFooterHelpAlwaysKeepsTheWayOut(t *testing.T) {
	for _, w := range []int{60, 76, 80, 100, 110, 150, 200} {
		m := sized(twoSets(), w, 40)
		for tb := tab(0); tb < numTabs; tb++ {
			m.tab = tb
			line := m.help.ShortHelpView(m.shortHelp())
			if !strings.Contains(line, "quit") {
				t.Errorf("%d cols, %v: no way out in %q", w, tb, line)
			}
			if !strings.Contains(line, "next mode") {
				t.Errorf("%d cols, %v: no way to another mode in %q", w, tb, line)
			}
			if got := lipgloss.Width(line); got > w {
				t.Errorf("%d cols, %v: help is %d wide", w, tb, got)
			}
		}
	}
}

// A wide terminal should be given the mode's whole keyboard, not a list
// truncated for a terminal nobody is using.
func TestAWideTerminalGetsEveryKeyForTheMode(t *testing.T) {
	m := sized(twoSets(), 200, 40)
	line := m.help.ShortHelpView(m.shortHelp())
	for _, want := range []string{"add a folder", "what is stored", "another computer", "stop backing up"} {
		if !strings.Contains(line, want) {
			t.Errorf("200 cols should offer %q:\n%s", want, line)
		}
	}
}

// `r2b add` ends by offering to run backups on a timer, because a folder
// added and backed up exactly once is the opposite of what was asked for.
// The window has to make the same offer: the Schedule tab exists, and
// someone who has just added their first folder has no reason to go and look
// at it.
func TestAddingTheFirstFolderOffersASchedule(t *testing.T) {
	b := twoSets()
	b.sets = nil
	b.ov.Scheduled = false
	m := sized(b, 120, 40)

	m.Update(addedMsg("Notes"))
	drain(t, m)

	if m.overlay != overlayConfirm {
		t.Fatalf("overlay = %v, want the offer to schedule", m.overlay)
	}
	if !strings.Contains(m.confirm, "automatically") {
		t.Errorf("question = %q", m.confirm)
	}
	apply(t, m, press(m, "y"))
	if len(b.sched) != 1 || !b.sched[0] {
		t.Fatalf("sched = %v, want one registration", b.sched)
	}
}

// A second folder must not silently re-time the first, and a platform with
// no scheduler must not be offered one.
func TestTheScheduleOfferIsMadeOnlyWhenItWouldMeanSomething(t *testing.T) {
	for _, tc := range []struct {
		name      string
		scheduled bool
		available bool
	}{
		{"already scheduled", true, true},
		{"no scheduler on this platform", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := twoSets()
			b.ov.Scheduled, b.ov.SchedulerAvailable = tc.scheduled, tc.available
			m := sized(b, 120, 40)

			m.Update(addedMsg("Notes"))
			drain(t, m)

			if m.overlay == overlayConfirm {
				t.Errorf("asked %q when it should not have", m.confirm)
			}
		})
	}
}

// The offer follows a backup that worked. A first backup that failed has a
// problem to show, not a question to ask.
func TestAFailedFirstBackupIsNotFollowedByTheOffer(t *testing.T) {
	b := twoSets()
	b.sets = nil
	b.ov.Scheduled = false
	m := sized(b, 120, 40)

	m.justAdded = true
	m.Update(runDoneMsg{what: "Notes backed up.", err: errors.New("the bucket refused it")})

	if m.overlay == overlayConfirm {
		t.Errorf("asked about scheduling after a failure: %q", m.confirm)
	}
	if m.err == nil {
		t.Error("the failure should be on screen")
	}
}

// A backup stopped by a shutdown showed nothing at all: the progress file
// was there, every reader saw the process was gone, and every reader threw
// it away. The one thing a person wants when they open this after a crash is
// to know their upload is not lost.
func TestAnInterruptedRunIsShownAndSaysItResumesItself(t *testing.T) {
	b := twoSets()
	b.ov.Interrupted = "Photos"
	b.ov.InterruptedAt = time.Now().Add(-40 * time.Minute)
	b.ov.InterruptedDone, b.ov.InterruptedTotal = 3<<30, 4<<30
	b.ov.PendingDone, b.ov.PendingTotal, b.ov.PendingFiles = 3<<30, 4<<30, 1
	m := sized(b, 120, 40)

	body := m.bodyView()
	for _, want := range []string{"Photos", "interrupted", "40m ago", "resumes by itself"} {
		if !strings.Contains(body, want) {
			t.Errorf("the folder list should mention %q:\n%s", want, body)
		}
	}
	// How far it got has to be there, or "resumes by itself" is a promise
	// with nothing behind it.
	if !strings.Contains(body, "GB") {
		t.Errorf("the interrupted line does not say how far it got:\n%s", body)
	}
}

// A run that is happening now must not be described as one that stopped.
func TestALiveRunIsNotReportedAsInterrupted(t *testing.T) {
	b := twoSets()
	b.ov.Running = "Photos"
	b.ov.RunETA = "2m remaining"
	b.ov.Interrupted = "Photos"
	m := sized(b, 120, 40)

	body := m.bodyView()
	if !strings.Contains(body, "running now") {
		t.Errorf("a live run should be shown as live:\n%s", body)
	}
	if strings.Contains(body, "interrupted") {
		t.Errorf("a live run must not also read as interrupted:\n%s", body)
	}
}
