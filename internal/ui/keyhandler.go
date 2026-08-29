package ui

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/saurabhhbansal/r2backup/internal/progress"
	"github.com/saurabhhbansal/r2backup/internal/schedule"
)

func (m *Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// An overlay owns the keyboard while it is up. Checked before anything
	// else so that typing a folder name into a form is never read as a
	// command -- "b" is the backup key and the first letter of a great many
	// words someone might type.
	// ctrl+c leaves, from anywhere. bubbletea puts the terminal in raw mode
	// with ISIG off, so this is an ordinary key and not a signal: an overlay
	// that does not handle it is an overlay you cannot ctrl+c out of. From a
	// form, esc-then-q was the only way out.
	if msg.Type == tea.KeyCtrlC {
		return m.quitNow()
	}
	if m.overlay != overlayNone {
		return m.overlayKey(msg)
	}
	// Likewise the list's filter box.
	if m.tab == tabFolders && m.list.SettingFilter() {
		return m, m.forward(msg)
	}

	m.notice = ""

	switch {
	case key.Matches(msg, keys.Quit):
		return m.quitNow()

	case key.Matches(msg, keys.Help):
		m.overlay = overlayHelp
		return m, nil

	case key.Matches(msg, keys.Refresh):
		if m.tab == tabTrash && m.trashSet != "" {
			return m, m.loadTrash(m.trashSet)
		}
		return m, tea.Batch(m.load(), m.loadAccount())

	case key.Matches(msg, keys.Watch):
		// Back to a run that esc was pressed on. Without this a backup left
		// running has no screen and no way to get one -- the "running now"
		// banner is suppressed for this window's own run.
		if m.running {
			m.overlay = overlayRunning
		}
		return m, nil

	case key.Matches(msg, keys.NextTab):
		return m, m.gotoTab((m.tab + 1) % numTabs)

	case key.Matches(msg, keys.PrevTab):
		return m, m.gotoTab((m.tab + numTabs - 1) % numTabs)
	}

	// Digits jump straight to a mode, which is the fastest way to get to one
	// and the only discoverable one if you have not found tab yet.
	if n, err := strconv.Atoi(msg.String()); err == nil && n >= 1 && n <= int(numTabs) {
		return m, m.gotoTab(tab(n - 1))
	}

	// Refused up front on every tab, not after the form the key opens.
	// Asking someone to browse to a folder, name it and choose a retention
	// window and only then saying "something is already running" wastes the
	// whole conversation -- and the guard used to exist only on Folders, so
	// recovering a file from Trash mid-backup did exactly that.
	if m.running && startsWork(msg) {
		m.notice = busyNote
		return m, nil
	}

	switch m.tab {
	case tabFolders:
		return m.foldersKey(msg)
	case tabSchedule:
		return m.scheduleKey(msg)
	case tabTrash:
		return m.trashKey(msg)
	case tabAccount:
		return m.accountKey(msg)
	}
	return m, nil
}

// startsWork reports whether a key begins something that transfers data.
// Only one of those may be in flight: they contend for the same bbolt writer
// lock on the index, and the progress screen can only show one.
func startsWork(msg tea.KeyMsg) bool {
	for _, b := range []key.Binding{keys.Add, keys.Backup, keys.All, keys.Restore} {
		if key.Matches(msg, b) {
			return true
		}
	}
	return false
}

func (m *Model) quitNow() (tea.Model, tea.Cmd) {
	// Keyed off runCancel, not off m.running: they are meant to move
	// together and the whole point of cancelling here is the case where
	// something has gone wrong. A live goroutine left uncancelled keeps
	// rewriting progress.json, and every other window reads that as a run in
	// flight until it goes stale.
	if m.runCancel != nil {
		m.runCancel()
		m.runCancel = nil
	}
	m.running = false
	m.quit = true
	return m, tea.Quit
}

func (m *Model) gotoTab(t tab) tea.Cmd {
	m.tab = t
	m.notice = ""
	if t == tabTrash {
		if v, ok := m.selected(); ok && m.trashSet != v.Name {
			return m.loadTrash(v.Name)
		}
	}
	if t == tabAccount {
		return m.loadAccount()
	}
	return nil
}

// overlayKey routes a key to whichever flow is in front.
func (m *Model) overlayKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch m.overlay {
	case overlayConfirm:
		switch msg.String() {
		case "y", "Y":
			run := m.confirmYes
			m.overlay, m.confirmYes = overlayNone, nil
			if run != nil {
				return m, run()
			}
		case "n", "N", "esc", "enter", "q":
			m.overlay, m.confirmYes = overlayNone, nil
		}
		return m, nil

	case overlayHelp:
		switch {
		case key.Matches(msg, keys.Back), key.Matches(msg, keys.Quit), key.Matches(msg, keys.Help):
			m.overlay = overlayNone
			return m, nil
		}
		return m, m.forward(msg)

	case overlayDetail:
		switch {
		case key.Matches(msg, keys.Back), key.Matches(msg, keys.Quit):
			m.overlay = overlayNone
			return m, nil
		}
		// The detail screen prints its own list of keys along the bottom.
		// They did nothing: every key but esc went to the viewport. A screen
		// that advertises five shortcuts and honours none of them is worse
		// than one that advertises none.
		if v, ok := m.selected(); ok {
			if m.running && startsWork(msg) {
				m.notice = busyNote
				return m, nil
			}
			switch {
			case key.Matches(msg, keys.Backup):
				return m, m.startBackup([]string{v.Name})
			case key.Matches(msg, keys.Edit):
				m.overlay = overlayNone
				return m, m.scanFolder(v.Root, v.Name)
			case key.Matches(msg, keys.Restore):
				m.askRestore(v)
				return m, nil
			case key.Matches(msg, keys.Rename):
				m.askRename(v)
				return m, nil
			case key.Matches(msg, keys.Relink):
				m.askRelink(v)
				return m, nil
			}
		}
		return m, m.forward(msg)

	case overlayRunning:
		switch {
		case key.Matches(msg, keys.Back):
			// Esc leaves the run going and returns to the tabs. Backing out
			// of a screen must never be a way to abort work by accident.
			m.overlay = overlayNone
			return m, nil
		case key.Matches(msg, keys.Quit):
			return m.quitNow()
		}
		return m, nil

	case overlayForm:
		if key.Matches(msg, keys.Back) {
			m.overlay, m.form = overlayNone, nil
			return m, nil
		}
		cmd, done := m.form.Update(msg)
		if done {
			m.form = nil
			// Only close the form's own overlay. A submit that started a
			// transfer has already moved us to the progress screen, and
			// overwriting that back to overlayNone ran every restore with no
			// bar, no phase and no title -- just the folder list, with
			// nothing on it saying anything was happening.
			if m.overlay == overlayForm {
				m.overlay = overlayNone
			}
		}
		return m, cmd

	case overlayBrowse:
		switch {
		case key.Matches(msg, keys.Back):
			m.overlay = overlayNone
			return m, nil

		case msg.String() == "t":
			// Typing a path is the only way to reach some folders at all.
			// filepicker's "up" is filepath.Dir, and on Windows
			// filepath.Dir(`C:\`) is `C:\` -- it stops dead at the drive
			// root, so a folder on D: could not be added from the interface
			// at all. `r2b add D:\work` still worked, which is exactly the
			// command-line fallback this is meant to remove.
			m.askTypedPath()
			return m, nil

		case msg.String() == ".":
			// Without this no dot-directory can be reached: ~/.config,
			// ~/.ssh, ~/.local/share are all unbackupable from the browser.
			m.browse.ShowHidden = !m.browse.ShowHidden
			return m, m.browse.Init()

		case msg.String() == " ":
			return m, m.scanFolder(m.browse.CurrentDirectory, "")
		}
		var cmd tea.Cmd
		m.browse, cmd = m.browse.Update(msg)
		if picked, path := m.browse.DidSelectFile(msg); picked {
			return m, m.scanFolder(path, "")
		}
		return m, cmd

	case overlayPicker:
		if m.picker == nil {
			m.overlay = overlayNone
			return m, nil
		}
		m.picker.Update(msg)
		switch {
		case m.picker.Cancelled():
			m.overlay, m.picker = overlayNone, nil
			m.notice = "Cancelled. Nothing changed."
			return m, nil
		case m.picker.Accepted():
			ex := m.picker.Excludes()
			forSet, root := m.pickerFor, m.pickerRoot
			m.overlay, m.picker = overlayNone, nil
			if forSet != "" {
				return m, m.applyExcludes(forSet, ex)
			}
			return m, m.finishAdd(root, ex)
		}
		return m, nil
	}
	return m, nil
}

// --- Folders ---

func (m *Model) foldersKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// Anything that would start a transfer is refused up front, not after
	// the form it opens. Asking someone to browse to a folder, name it and
	// choose a retention window, and only then saying "something is already
	// running", wastes the whole conversation.
	switch {
	case key.Matches(msg, keys.Add):
		return m, m.startBrowse()

	case key.Matches(msg, keys.Enter):
		if v, ok := m.selected(); ok {
			m.showDetail(v)
		}
		return m, nil
	}

	v, ok := m.selected()
	if !ok {
		return m, m.forward(msg)
	}

	switch {
	case key.Matches(msg, keys.Backup):
		return m, m.startBackup([]string{v.Name})

	case key.Matches(msg, keys.All):
		names := make([]string, 0, len(m.sets))
		for _, s := range m.sets {
			names = append(names, s.Name)
		}
		if len(names) == 0 {
			return m, nil
		}
		return m, m.startBackup(names)

	case key.Matches(msg, keys.Edit):
		return m, m.scanFolder(v.Root, v.Name)

	case key.Matches(msg, keys.Restore):
		m.askRestore(v)
		return m, nil

	case key.Matches(msg, keys.Rename):
		m.askRename(v)
		return m, nil

	case key.Matches(msg, keys.Relink):
		m.askRelink(v)
		return m, nil

	case key.Matches(msg, keys.Remove):
		name := v.Name
		m.ask("Stop backing up "+name+"? What is already stored stays in the bucket.",
			func() tea.Cmd { return m.removeSet(name) })
		return m, nil
	}
	return m, m.forward(msg)
}

// --- Schedule ---

func (m *Model) scheduleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch {
	case key.Matches(msg, keys.Toggle):
		on := !m.ov.Scheduled
		every := schedule.DefaultIntervalMinutes
		if m.ov.Interval > 0 {
			every = int(m.ov.Interval / time.Minute)
		}
		return m, m.setSchedule(every, !on)

	case key.Matches(msg, keys.Every):
		m.askInterval()
		return m, nil
	}
	return m, nil
}

// --- Trash ---

func (m *Model) trashKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch {
	case key.Matches(msg, keys.Enter):
		// Recovering a file is a restore, so it is refused while something
		// is running -- and refused here, before the form, not after it.
		if m.running {
			m.notice = busyNote
			return m, nil
		}
		row := m.trash.SelectedRow()
		if len(row) == 0 || m.trashCount() == 0 {
			return m, nil
		}
		m.askRecover(m.trashSet, m.trashRows[m.trash.Cursor()])
		return m, nil
	}
	return m, m.forward(msg)
}

func (m *Model) trashCount() int { return len(m.trashRows) }

// --- Account ---

func (m *Model) accountKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch {
	case key.Matches(msg, keys.SignIn):
		m.askEmail()
		return m, nil

	case key.Matches(msg, keys.SignOut):
		if !m.acct.SignedIn {
			return m, nil
		}
		m.ask("Sign out on this computer? Your credentials and backups are untouched.",
			func() tea.Cmd {
				return func() tea.Msg {
					if err := m.backend.SignOut(m.ctx); err != nil {
						return errMsg{err}
					}
					return noticeMsg("Signed out.")
				}
			})
		return m, nil

	case key.Matches(msg, keys.Share):
		if !m.acct.SignedIn {
			m.notice = "Sign in first, with i."
			return m, nil
		}
		m.askStorePassword()
		return m, nil

	case key.Matches(msg, keys.Keys):
		m.askKeys()
		return m, nil

	case key.Matches(msg, keys.Unlock):
		if !m.acct.SignedIn {
			m.notice = "Sign in first, with i."
			return m, nil
		}
		if !m.acct.VaultStored {
			m.notice = "Nothing is stored for this account yet."
			return m, nil
		}
		m.askUnlock()
		return m, nil

	case key.Matches(msg, keys.Update):
		return m, m.checkUpdate()
	}
	return m, nil
}

// --- work ---

// ask puts up a yes/no confirmation. Nothing destructive happens without one.
func (m *Model) ask(question string, yes func() tea.Cmd) {
	m.confirm = question
	m.confirmYes = yes
	m.overlay = overlayConfirm
}

func (m *Model) showForm(f *form) {
	m.form = f
	m.overlay = overlayForm
}

// startBackup runs each named set in turn, streaming progress back through
// m.events.
//
// The goroutine never touches the model. Everything it has to say arrives as
// a message and is applied in Update, on bubbletea's own loop.
// busyNote is the one sentence said whenever a second job is refused.
const busyNote = "Something is already running. Wait for it, or press w to watch it."

func (m *Model) startBackup(names []string) tea.Cmd {
	if m.running {
		m.notice = busyNote
		return nil
	}
	ctx, cancel := context.WithCancel(m.ctx)
	m.running, m.runCancel = true, cancel
	m.runWhat, m.runPhase = names[0], "starting"
	m.runSnap = progress.Snapshot{}
	m.overlay = overlayRunning

	events, backend := m.events, m.backend
	go func() {
		post := postTo(events, ctx)
		var lastErr error
		for _, n := range names {
			post(runSetMsg(n))
			if err := backend.Backup(ctx, n,
				func(p string) { post(phaseMsg(p)) },
				func(s progress.Snapshot) { post(snapshotMsg(s)) },
			); err != nil {
				lastErr = err
				break
			}
		}
		cancel()
		finish(events, runDoneMsg{what: backedUpNote(names), err: lastErr})
	}()
	return tea.Batch(m.waitForEvent(), m.spin.Tick)
}

func backedUpNote(names []string) string {
	if len(names) == 1 {
		return names[0] + " backed up."
	}
	return strconv.Itoa(len(names)) + " folders backed up."
}

func (m *Model) startRestore(req RestoreRequest) tea.Cmd {
	if m.running {
		m.notice = busyNote
		return nil
	}
	ctx, cancel := context.WithCancel(m.ctx)
	m.running, m.runCancel = true, cancel
	m.runWhat, m.runPhase = "Restoring "+req.Set, "starting"
	m.runSnap = progress.Snapshot{}
	m.overlay = overlayRunning

	events, backend := m.events, m.backend
	go func() {
		post := postTo(events, ctx)
		res, err := backend.Restore(ctx, req,
			func(p string) { post(phaseMsg(p)) },
			func(s progress.Snapshot) { post(snapshotMsg(s)) },
		)
		cancel()
		finish(events, restoreDoneMsg{res: res, err: err})
	}()
	return tea.Batch(m.waitForEvent(), m.spin.Tick)
}

// postTo builds a non-blocking sender.
//
// A send onto a channel whose reader has gone -- which is what leaving the
// screen is -- would wedge the goroutine forever, and with it the transfer it
// is running. Progress is the most droppable thing on screen: a frame missed
// under a burst is invisible, a stalled upload is not.
func postTo(events chan tea.Msg, ctx context.Context) func(tea.Msg) {
	return func(msg tea.Msg) {
		select {
		case events <- msg:
		case <-ctx.Done():
		default:
		}
	}
}

// finish delivers the one message that must not be dropped: without it the
// interface believes a run is still going for as long as it is open.
func finish(events chan tea.Msg, msg tea.Msg) {
	select {
	case events <- msg:
	case <-time.After(5 * time.Second):
	}
}

func restoreSummary(r RestoreResult) string {
	var b strings.Builder
	b.WriteString(progress.FormatCount(int64(r.Files)) + " restored (" + progress.FormatBytes(r.Bytes) + ") into " + r.Target)
	if r.Skipped > 0 {
		b.WriteString(" · " + strconv.Itoa(r.Skipped) + " already there")
	}
	if r.Failed > 0 {
		b.WriteString(" · " + strconv.Itoa(r.Failed) + " failed")
	}
	return b.String()
}

func (m *Model) loadTrash(name string) tea.Cmd {
	m.notice = "Reading trash..."
	m.request++
	req := m.request
	return func() tea.Msg {
		rows, err := m.backend.Trash(m.ctx, name)
		if err != nil {
			return errMsg{err}
		}
		return trashMsg{set: name, rows: rows, req: req}
	}
}

func (m *Model) removeSet(name string) tea.Cmd {
	return func() tea.Msg {
		if err := m.backend.Remove(m.ctx, name, false); err != nil {
			return errMsg{err}
		}
		return noticeMsg(name + " is no longer backed up. What was stored is still in the bucket.")
	}
}

func (m *Model) setSchedule(every int, off bool) tea.Cmd {
	return func() tea.Msg {
		if err := m.backend.Schedule(m.ctx, every, off); err != nil {
			return errMsg{err}
		}
		if off {
			return noticeMsg("Automatic backups are off. They will only run when you run them.")
		}
		return noticeMsg("Backups will run every " + strconv.Itoa(every) + " minutes from now on.")
	}
}

func (m *Model) applyExcludes(name string, excludes []string) tea.Cmd {
	return func() tea.Msg {
		if err := m.backend.SetExcludes(m.ctx, name, excludes); err != nil {
			return errMsg{err}
		}
		return noticeMsg("Updated what " + name + " includes. Back it up to apply the change.")
	}
}

func (m *Model) checkUpdate() tea.Cmd {
	// Two presses: the first says what is available, the second installs it.
	if m.pendingUpdate != "" {
		v := m.pendingUpdate
		m.pendingUpdate = ""
		m.notice = "Downloading " + v + "..."
		return func() tea.Msg {
			applied, err := m.backend.ApplyUpdate(m.ctx)
			if err != nil {
				return errMsg{err}
			}
			return updateMsg{version: applied, applied: true}
		}
	}
	m.notice = "Checking..."
	return func() tea.Msg {
		v, err := m.backend.CheckUpdate(m.ctx)
		if err != nil {
			return errMsg{err}
		}
		return updateMsg{version: v}
	}
}
