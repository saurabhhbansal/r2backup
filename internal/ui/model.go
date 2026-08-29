package ui

import (
	"context"
	"time"

	"github.com/charmbracelet/bubbles/filepicker"
	"github.com/charmbracelet/bubbles/help"
	"github.com/charmbracelet/bubbles/list"
	bprogress "github.com/charmbracelet/bubbles/progress"
	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/table"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/saurabhhbansal/r2backup/internal/progress"
	"github.com/saurabhhbansal/r2backup/internal/scan"
	"github.com/saurabhhbansal/r2backup/internal/tui"
)

// tab is one of the modes across the top of the window.
//
// Scheduling used to be a single unexplained keystroke on the folder list,
// which is no way to present the setting that decides whether any of this
// happens without you. Each mode is now somewhere you can go and look at.
type tab int

const (
	tabFolders tab = iota
	tabSchedule
	tabTrash
	tabAccount
	numTabs
)

func (t tab) String() string {
	switch t {
	case tabFolders:
		return "Folders"
	case tabSchedule:
		return "Schedule"
	case tabTrash:
		return "Trash"
	case tabAccount:
		return "Account"
	}
	return ""
}

// overlay is a flow that takes over the window until it is finished or
// cancelled. Tabs are places; overlays are jobs.
type overlay int

const (
	overlayNone    overlay = iota
	overlayBrowse          // choosing a folder to add
	overlayPicker          // the tree picker, for add and edit
	overlayForm            // any of the typed forms
	overlayRunning         // a backup or restore in progress
	overlayConfirm         // a yes/no question
	overlayHelp
	overlayDetail
)

// tickInterval is how often local state is re-read. A second, because that is
// how often a running backup rewrites progress.json, and Load is required not
// to touch the network precisely so this can be cheap enough to do on a timer.
const tickInterval = time.Second

type (
	loadedMsg struct {
		sets []SetView
		ov   Overview
	}
	accountMsg AccountView
	errMsg     struct{ err error }
	noticeMsg  string
	tickMsg    time.Time
	scannedMsg struct {
		root string
		name string
		res  *scan.Result
		// editing names the set when this scan is for `edit` rather than
		// for a new folder.
		editing string
		// req is the request number this scan answers. A result whose number
		// is no longer current is stale and dropped.
		req int
	}
	phaseMsg    string
	runSetMsg   string
	snapshotMsg progress.Snapshot
	runDoneMsg  struct {
		what string
		err  error
	}
	// reopenRunMsg brings the progress screen back after esc.
	reopenRunMsg   struct{}
	restoreDoneMsg struct {
		res RestoreResult
		err error
	}
	trashMsg struct {
		set  string
		rows []TrashRow
		req  int
	}
	updateMsg struct {
		version string
		applied bool
	}
)

// Model is the whole interface: four tabs and the flows that run over them.
type Model struct {
	backend Backend
	ctx     context.Context

	tab     tab
	overlay overlay
	width   int
	height  int

	sets []SetView
	ov   Overview
	acct AccountView

	list   list.Model
	trash  table.Model
	detail viewport.Model
	help   help.Model
	spin   spinner.Model
	bar    bprogress.Model

	browse filepicker.Model
	picker *tui.Model
	// pickerFor is the set being edited, or "" when the picker is choosing
	// what a brand new folder should include.
	pickerFor  string
	pickerRoot string
	form       *form

	detailBody string
	trashSet   string
	trashRows  []TrashRow

	running   bool
	runWhat   string
	runPhase  string
	runSnap   progress.Snapshot
	runCancel context.CancelFunc
	events    chan tea.Msg

	// request counts the asynchronous jobs this model has started. A result
	// carrying an older number is stale and dropped -- see scannedMsg.
	request int

	confirm    string
	confirmYes func() tea.Cmd

	// pendingUpdate is a version found by a check and not yet installed, so
	// the same key can confirm it.
	pendingUpdate string

	notice string
	err    error

	quit bool
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

	fp := filepicker.New()
	fp.DirAllowed = true
	fp.FileAllowed = false
	fp.ShowHidden = false
	fp.AutoHeight = false

	return &Model{
		backend: b,
		ctx:     ctx,
		width:   80,
		height:  24,
		list:    l,
		help:    help.New(),
		spin:    sp,
		bar:     bprogress.New(bprogress.WithDefaultGradient()),
		detail:  viewport.New(0, 0),
		browse:  fp,
		events:  make(chan tea.Msg, 64),
	}
}

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

// loadAccount reaches the network, so it runs when the Account tab is opened
// and on an explicit refresh -- never on the tick.
func (m *Model) loadAccount() tea.Cmd {
	return func() tea.Msg {
		a, err := m.backend.Account(m.ctx)
		if err != nil {
			return accountMsg(AccountView{Err: err.Error()})
		}
		return accountMsg(a)
	}
}

// waitForEvent turns the worker goroutine's channel into a stream of
// messages. Every message re-arms it, which is how bubbletea consumes a
// channel without blocking Update.
func (m *Model) waitForEvent() tea.Cmd {
	ch := m.events
	return func() tea.Msg { return <-ch }
}

// Update is a thin wrapper so a screen change always re-runs layout: the
// header is nineteen rows taller on the folder tab than in an overlay, so the
// body budget every component sizes itself against changes with it.
func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	beforeTab, beforeOverlay := m.tab, m.overlay
	model, cmd := m.update(msg)
	if m.tab != beforeTab || m.overlay != beforeOverlay {
		m.layout()
	}
	return model, cmd
}

func (m *Model) update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.layout()
		// The picker is a child model with its own geometry, and it is the
		// one component layout() cannot resize by setting a field: it sizes
		// itself from WindowSizeMsg. Without this it stays at its 80x20
		// default forever -- a 15-row frame in a 45-row window, and rows
		// wide enough to be truncated on a narrow one, losing the size and
		// item counts the picker exists to show.
		if m.picker != nil {
			m.picker.Update(m.pickerSize())
		}
		return m, nil

	case tea.KeyMsg:
		return m.handleKey(msg)

	case loadedMsg:
		m.sets, m.ov = msg.sets, msg.ov
		m.err = nil
		m.syncList()
		return m, nil

	case accountMsg:
		m.acct = AccountView(msg)
		return m, nil

	case errMsg:
		// An error from anywhere -- a refresh, a trash listing, the account
		// service -- must not be read as "the backup stopped". It used to
		// clear m.running and close the progress screen, which meant a
		// failed background reload silently un-tracked a live transfer and
		// let a second one start on top of it. Only runDoneMsg and
		// restoreDoneMsg end a run.
		m.err = msg.err
		return m, nil

	case noticeMsg:
		m.notice = string(msg)
		return m, m.load()

	case tickMsg:
		// Only the resting screens refresh on a timer. Reloading underneath
		// a form or a confirmation would move what the user is looking at.
		if m.overlay == overlayNone || m.overlay == overlayRunning {
			return m, tea.Batch(tick(), m.load())
		}
		return m, tick()

	case scannedMsg:
		// A scan of a large folder takes a while, and the user may have
		// moved on -- cancelled the browser, changed tab, started a backup.
		// Reopening the picker over whatever they are looking at now is
		// worse than dropping a result they no longer asked for.
		if msg.req != m.request {
			return m, nil
		}
		return m, m.openPicker(msg)

	case reopenRunMsg:
		if m.running {
			m.overlay = overlayRunning
		}
		return m, nil

	case runSetMsg:
		m.runWhat = string(msg)
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
		m.overlay = overlayNone
		if msg.err != nil {
			m.err = msg.err
		} else {
			m.notice = msg.what
		}
		return m, m.load()

	case restoreDoneMsg:
		m.running = false
		m.runCancel = nil
		m.overlay = overlayNone
		if msg.err != nil {
			m.err = msg.err
		} else {
			m.notice = restoreSummary(msg.res)
		}
		return m, m.load()

	case trashMsg:
		if msg.req != m.request {
			return m, nil
		}
		m.showTrash(msg)
		return m, nil

	case addedMsg:
		m.notice = ""
		return m, m.startBackup([]string{string(msg)})

	case codeSentMsg:
		m.askCode(string(msg))
		return m, nil

	case signedInMsg:
		// Signing in is only half of it. What the user wants is this
		// computer working, so the moment the session exists we go looking
		// for stored credentials rather than making them find another key.
		return m, tea.Batch(m.loadAccount(), m.afterSignIn())

	case unlockNeededMsg:
		m.askUnlock()
		return m, nil

	case unlockFailedMsg:
		m.err = msg.err
		m.askUnlock()
		return m, nil

	case updateMsg:
		if !msg.applied && msg.version != "" {
			m.pendingUpdate = msg.version
		}
		switch {
		case msg.applied:
			m.notice = "Updated to " + msg.version + ". Restart r2b to use it."
		case msg.version == "":
			m.notice = "You are on the latest version."
		default:
			m.notice = msg.version + " is available. Press u again to install it."
		}
		return m, nil

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
	switch {
	case m.overlay == overlayBrowse:
		m.browse, cmd = m.browse.Update(msg)
	case m.overlay == overlayPicker && m.picker != nil:
		_, cmd = m.picker.Update(msg)
	case m.overlay == overlayDetail:
		m.detail, cmd = m.detail.Update(msg)
	case m.overlay != overlayNone:
		return nil
	case m.tab == tabFolders:
		m.list, cmd = m.list.Update(msg)
	case m.tab == tabTrash:
		m.trash, cmd = m.trash.Update(msg)
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

// pickerSize is the window the embedded picker should draw into. It is given
// the panel's interior, not the terminal, because that is where it is drawn.
func (m *Model) pickerSize() tea.WindowSizeMsg {
	w, h := m.width-4, m.height-m.chromeHeight()
	if w < 20 {
		w = 20
	}
	if h < 6 {
		h = 6
	}
	// The picker reserves its own header and footer rows out of what it is
	// given, so it is handed the body budget plus that reservation.
	return tea.WindowSizeMsg{Width: w, Height: h + 6}
}

func (m *Model) setByName(name string) (SetView, bool) {
	for _, s := range m.sets {
		if s.Name == name {
			return s, true
		}
	}
	return SetView{}, false
}
