package ui

import (
	"context"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/saurabhhbansal/r2backup/internal/progress"
	"github.com/saurabhhbansal/r2backup/internal/schedule"
)

func (m *Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// While the list's filter is open every printable key is part of the
	// query, not a command. Checking this first is what keeps typing "backup"
	// into the filter box from starting six backups.
	if m.screen == screenHome && m.list.SettingFilter() {
		return m, m.forward(msg)
	}

	// A confirmation owns the keyboard while it is up.
	if m.screen == screenConfirm {
		switch {
		case key.Matches(msg, keys.Quit), key.Matches(msg, keys.Back):
			m.screen = screenHome
			return m, nil
		}
		switch msg.String() {
		case "y", "Y":
			run := m.confirmYes
			m.screen = screenHome
			m.confirmYes = nil
			if run != nil {
				return m, run()
			}
		case "n", "N", "enter":
			m.screen = screenHome
			m.confirmYes = nil
		}
		return m, nil
	}

	m.status = ""

	switch {
	case key.Matches(msg, keys.Quit):
		if m.running {
			// A backup started here is cancelled deliberately rather than
			// abandoned: the process is about to exit, and leaving a
			// half-written progress file behind would show a phantom run in
			// every other window until it went stale.
			m.runCancel()
			m.running = false
		}
		m.quit = true
		return m, tea.Quit

	case key.Matches(msg, keys.Help):
		if m.screen == screenHelp {
			m.screen = screenHome
		} else {
			m.screen = screenHelp
		}
		return m, nil

	case key.Matches(msg, keys.Back):
		switch m.screen {
		case screenHome:
			return m, nil
		case screenRunning:
			// Esc leaves the run going and returns to the list, rather than
			// cancelling it. Backing out of a screen must never be a way to
			// abort work by accident.
			m.screen = screenHome
		default:
			m.screen = screenHome
		}
		return m, nil

	case key.Matches(msg, keys.Refresh):
		return m, m.load()
	}

	switch m.screen {
	case screenHome:
		return m.homeKey(msg)
	case screenDetail:
		return m.detailKey(msg)
	}
	return m, m.forward(msg)
}

func (m *Model) homeKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch {
	case key.Matches(msg, keys.Enter):
		if v, ok := m.selected(); ok {
			m.showDetail(v)
		}
		return m, nil

	case key.Matches(msg, keys.Add):
		return m, m.handOff(Action{Kind: ActionAdd})

	case key.Matches(msg, keys.Backup):
		if v, ok := m.selected(); ok {
			return m, m.startBackup(v.Name)
		}
		return m, nil

	case key.Matches(msg, keys.All):
		if len(m.sets) == 0 {
			return m, nil
		}
		return m, m.startBackupAll()

	case key.Matches(msg, keys.Edit):
		if v, ok := m.selected(); ok {
			return m, m.handOff(Action{Kind: ActionEdit, Set: v.Name})
		}
		return m, nil

	case key.Matches(msg, keys.Restore):
		if v, ok := m.selected(); ok {
			return m, m.handOff(Action{Kind: ActionRestore, Set: v.Name})
		}
		return m, nil

	case key.Matches(msg, keys.Trash):
		if v, ok := m.selected(); ok {
			return m, m.loadTrash(v.Name)
		}
		return m, nil

	case key.Matches(msg, keys.Remove):
		if v, ok := m.selected(); ok {
			name := v.Name
			m.ask("Stop backing up "+name+"? What is already stored stays in the bucket.",
				func() tea.Cmd { return m.removeSet(name) })
		}
		return m, nil

	case key.Matches(msg, keys.Schedule):
		return m, m.toggleSchedule()
	}
	return m, m.forward(msg)
}

func (m *Model) detailKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	v, ok := m.selected()
	if !ok {
		return m, nil
	}
	switch {
	case key.Matches(msg, keys.Backup):
		return m, m.startBackup(v.Name)
	case key.Matches(msg, keys.Trash):
		return m, m.loadTrash(v.Name)
	case key.Matches(msg, keys.Edit):
		return m, m.handOff(Action{Kind: ActionEdit, Set: v.Name})
	case key.Matches(msg, keys.Restore):
		return m, m.handOff(Action{Kind: ActionRestore, Set: v.Name})
	}
	return m, m.forward(msg)
}

// handOff leaves the interface so the command line can run a job this screen
// does not do itself -- but never while a backup is going.
//
// Leaving mid-run used to abandon it. The goroutine keeps uploading and keeps
// posting progress onto a channel that no longer has a reader, fills the
// buffer, and blocks forever: the backup stops silently, partway, having left
// a stale progress file behind. And `edit` ends by running a backup of its
// own, so even if it survived, two runs would be contending for one bbolt
// writer lock on the same index.
func (m *Model) handOff(a Action) tea.Cmd {
	if m.running {
		m.status = "A backup is running. Wait for it, or press q to stop it."
		return nil
	}
	m.action = a
	m.quit = true
	return tea.Quit
}

// ask puts up a yes/no confirmation. Nothing destructive happens without one.
func (m *Model) ask(question string, yes func() tea.Cmd) {
	m.confirm = question
	m.confirmYes = yes
	m.screen = screenConfirm
}

// startBackup runs one set, streaming progress back through m.events.
func (m *Model) startBackup(name string) tea.Cmd {
	if m.running {
		m.status = "A backup is already running."
		return nil
	}
	return m.runSets([]string{name})
}

func (m *Model) startBackupAll() tea.Cmd {
	if m.running {
		m.status = "A backup is already running."
		return nil
	}
	names := make([]string, 0, len(m.sets))
	for _, s := range m.sets {
		names = append(names, s.Name)
	}
	return m.runSets(names)
}

// runSets backs up each named set in turn on its own goroutine, posting
// phase, progress and completion onto m.events.
//
// The goroutine never touches the model. Everything it has to say arrives as
// a message and is applied in Update, on bubbletea's own loop -- the same rule
// the predecessor learned the hard way with Qt, where a worker thread
// touching a widget segfaulted the process intermittently enough to look like
// flakiness.
func (m *Model) runSets(names []string) tea.Cmd {
	ctx, cancel := context.WithCancel(m.ctx)
	m.running = true
	m.runCancel = cancel
	m.runSet = names[0]
	m.runPhase = "starting"
	m.runSnap = progress.Snapshot{}
	m.screen = screenRunning

	events := m.events
	backend := m.backend

	go func() {
		// post never blocks. A send onto a channel whose reader has gone --
		// which is what quitting is -- would otherwise wedge this goroutine
		// forever, and with it the backup it is running. Progress is the
		// most droppable thing on screen: a frame missed under a burst is
		// invisible, a stalled upload is not.
		post := func(msg tea.Msg) {
			select {
			case events <- msg:
			case <-ctx.Done():
			default:
			}
		}
		var lastErr error
		for _, n := range names {
			post(runSetMsg(n))
			err := backend.Backup(ctx, n,
				func(p string) { post(phaseMsg(p)) },
				func(s progress.Snapshot) { post(snapshotMsg(s)) },
			)
			if err != nil {
				lastErr = err
				break
			}
		}
		cancel()
		// The one message that must not be dropped: without it the interface
		// believes a backup is still running for as long as it is open.
		select {
		case events <- runDoneMsg{set: strings.Join(names, ", "), err: lastErr}:
		case <-time.After(5 * time.Second):
		}
	}()

	return tea.Batch(m.waitForEvent(), m.spin.Tick)
}

func (m *Model) loadTrash(name string) tea.Cmd {
	m.status = "Reading trash..."
	return func() tea.Msg {
		rows, err := m.backend.Trash(m.ctx, name)
		if err != nil {
			return errMsg{err}
		}
		return trashMsg{set: name, rows: rows}
	}
}

func (m *Model) removeSet(name string) tea.Cmd {
	return func() tea.Msg {
		if err := m.backend.Remove(m.ctx, name, false); err != nil {
			return errMsg{err}
		}
		return actionDoneMsg{note: name + " is no longer backed up. What was stored is still in the bucket."}
	}
}

// toggleSchedule turns automatic runs on or off. On, it uses the same default
// interval `schedule --every` does, so the two cannot disagree.
func (m *Model) toggleSchedule() tea.Cmd {
	on := !m.ov.Scheduled
	return func() tea.Msg {
		if err := m.backend.Schedule(m.ctx, schedule.DefaultIntervalMinutes, !on); err != nil {
			return errMsg{err}
		}
		if on {
			return actionDoneMsg{note: "Backups will now run automatically."}
		}
		return actionDoneMsg{note: "Automatic backups are off. They will only run when you run them."}
	}
}
