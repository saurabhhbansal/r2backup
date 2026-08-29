package ui

import (
	"context"
	"fmt"
	"time"

	"github.com/charmbracelet/bubbles/help"
	"github.com/charmbracelet/bubbles/list"
	bprogress "github.com/charmbracelet/bubbles/progress"
	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/table"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/saurabhhbansal/r2backup/internal/progress"
)

type screen int

const (
	screenHome screen = iota
	screenDetail
	screenTrash
	screenRunning
	screenConfirm
	screenHelp
)

// tickInterval is how often the home screen re-reads local state. It is a
// second because that is how often a running backup rewrites progress.json,
// and Load is required not to touch the network precisely so this can be
// cheap enough to do on a timer.
const tickInterval = time.Second

type (
	loadedMsg struct {
		sets []SetView
		ov   Overview
	}
	errMsg      struct{ err error }
	tickMsg     time.Time
	phaseMsg    string
	runSetMsg   string
	snapshotMsg progress.Snapshot
	runDoneMsg  struct {
		set string
		err error
	}
	trashMsg struct {
		set  string
		rows []TrashRow
	}
	actionDoneMsg struct{ note string }
)

// Model is the whole interface. One model with a screen field, rather than a
// stack of separate programs: every screen shares the same loaded state, and
// the alternative is each of them re-reading it and disagreeing.
type Model struct {
	backend Backend
	ctx     context.Context

	screen screen
	width  int
	height int

	sets []SetView
	ov   Overview

	list   list.Model
	trash  table.Model
	detail viewport.Model
	help   help.Model
	spin   spinner.Model
	bar    bprogress.Model

	// runState is the live backup this window itself started.
	running   bool
	runSet    string
	runPhase  string
	runSnap   progress.Snapshot
	runCancel context.CancelFunc
	events    chan tea.Msg

	detailBody string
	trashSet   string
	trashRows  []TrashRow
	trashCount int

	confirm    string
	confirmYes func() tea.Cmd

	status string
	err    error

	// action is what Run returns: a job this interface hands back to the
	// command line rather than doing itself.
	action Action
	quit   bool
}

// New builds the interface over a backend.
func New(ctx context.Context, b Backend) *Model {
	l := list.New(nil, setDelegate{width: 80}, 0, 0)
	l.SetShowTitle(false)
	l.SetShowStatusBar(false)
	l.SetShowHelp(false)
	l.SetFilteringEnabled(true)
	l.SetStatusBarItemName("folder", "folders")

	sp := spinner.New()
	sp.Spinner = spinner.Dot
	sp.Style = lipgloss.NewStyle().Foreground(accent)

	h := help.New()
	h.ShowAll = false

	return &Model{
		backend: b,
		ctx:     ctx,
		screen:  screenHome,
		width:   80,
		height:  24,
		list:    l,
		help:    h,
		spin:    sp,
		bar:     bprogress.New(bprogress.WithDefaultGradient()),
		detail:  viewport.New(0, 0),
		events:  make(chan tea.Msg, 64),
	}
}

// Action reports what the user asked for on the way out.
func (m *Model) Action() Action { return m.action }

func (m *Model) Init() tea.Cmd {
	return tea.Batch(m.load(), m.spin.Tick, tick())
}

func tick() tea.Cmd {
	return tea.Tick(tickInterval, func(t time.Time) tea.Msg { return tickMsg(t) })
}

func (m *Model) load() tea.Cmd {
	return func() tea.Msg {
		s, ov, err := m.backend.Load(m.ctx)
		if err != nil {
			return errMsg{err}
		}
		return loadedMsg{sets: s, ov: ov}
	}
}

// waitForEvent turns the backup goroutine's channel into a stream of
// messages. Every message re-arms it, which is the standard way to consume a
// channel from bubbletea without blocking Update.
func (m *Model) waitForEvent() tea.Cmd {
	ch := m.events
	return func() tea.Msg { return <-ch }
}

// Update is a thin wrapper so that a screen change always re-runs layout.
//
// The header is nineteen rows taller on the home screen than anywhere else,
// so the body budget every component sizes itself against changes the moment
// the screen does. Without this the list keeps a home-sized height on the
// detail screen and paints past the bottom of the window.
func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	before := m.screen
	model, cmd := m.update(msg)
	if m.screen != before {
		m.layout()
	}
	return model, cmd
}

func (m *Model) update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.layout()
		return m, nil

	case tea.KeyMsg:
		return m.handleKey(msg)

	case loadedMsg:
		m.sets, m.ov = msg.sets, msg.ov
		m.err = nil
		m.syncList()
		return m, nil

	case errMsg:
		m.err = msg.err
		return m, nil

	case tickMsg:
		// Only the home screen refreshes on a timer. Reloading underneath a
		// confirmation prompt or a trash listing would move the thing the
		// user is looking at.
		if m.screen == screenHome || m.screen == screenRunning {
			return m, tea.Batch(tick(), m.load())
		}
		return m, tick()

	case runSetMsg:
		// Which set is being worked on now. `B` backs up every set in turn,
		// and the running screen showed the first one's name for the whole
		// batch until this existed.
		m.runSet = string(msg)
		m.runPhase = "starting"
		return m, m.waitForEvent()

	case phaseMsg:
		m.runPhase = string(msg)
		return m, m.waitForEvent()

	case snapshotMsg:
		m.runSnap = progress.Snapshot(msg)
		return m, m.waitForEvent()

	case runDoneMsg:
		m.running = false
		m.runCancel = nil
		m.screen = screenHome
		if msg.err != nil {
			m.err = fmt.Errorf("%s: %w", msg.set, msg.err)
		} else {
			m.status = msg.set + " backed up."
		}
		return m, m.load()

	case trashMsg:
		m.showTrash(msg)
		return m, nil

	case actionDoneMsg:
		m.status = msg.note
		return m, m.load()

	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spin, cmd = m.spin.Update(msg)
		return m, cmd
	}

	return m, m.forward(msg)
}

// forward passes anything unhandled to whichever component owns the screen.
func (m *Model) forward(msg tea.Msg) tea.Cmd {
	var cmd tea.Cmd
	switch m.screen {
	case screenHome:
		m.list, cmd = m.list.Update(msg)
	case screenTrash:
		m.trash, cmd = m.trash.Update(msg)
	case screenDetail:
		m.detail, cmd = m.detail.Update(msg)
	}
	return cmd
}

func (m *Model) selected() (SetView, bool) {
	it, ok := m.list.SelectedItem().(setItem)
	if !ok {
		return SetView{}, false
	}
	return it.v, true
}
