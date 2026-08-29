package ui

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
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
	m.Update(scannedMsg{root: "/home/me/work", name: "work", res: b.mustScan()})
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
	m.Update(scannedMsg{root: b.sets[1].Root, name: "Code", res: b.mustScan(), editing: "Code"})
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
