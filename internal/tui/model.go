package tui

import (
	"strconv"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/saurabhhbansal/r2backup/internal/progress"
	"github.com/saurabhhbansal/r2backup/internal/scan"
)

// defaultHeight is the number of rows shown before a real terminal size is
// known. bubbletea sends a WindowSizeMsg as soon as a program actually runs,
// but a Model built directly (as every test in this package does) never
// receives one, so it needs a workable size out of the box.
const defaultHeight = 20
const defaultWidth = 80

// reservedLines is how much of the terminal the header and footer take, so
// Update can turn a WindowSizeMsg into a row budget for the tree itself.
const reservedLines = 6

// Model is the picker's bubbletea model. All the state that decides what gets
// backed up lives in the *Node tree (see tree.go); everything here is either
// cursor/scroll position or terminal geometry, which is why the tree logic
// can be tested without ever constructing one of these.
type Model struct {
	root *Node

	// rows is the flattened list of currently visible nodes (collapsed
	// subtrees excluded). It is rebuilt only when expand/collapse changes
	// what's visible, not on every keystroke -- rebuilding it is O(visible
	// rows), and there is no reason to pay that on a plain cursor move.
	rows   []Row
	cursor int // index into rows
	offset int // index of the first row currently drawn

	height int // rows available for the tree, excluding header/footer
	width  int

	accepted  bool
	cancelled bool
}

// NewModel builds a picker over scanned, rooted for display at root (the
// folder the user pointed the tool at). Everything starts checked, matching
// the spec this package exists to satisfy: press enter immediately and the
// whole tree goes.
func NewModel(root string, scanned *scan.Result) *Model {
	m := &Model{
		root:   BuildTree(root, scanned),
		height: defaultHeight,
		width:  defaultWidth,
	}
	m.refreshRows()
	return m
}

// Accepted reports whether the user confirmed the selection with enter.
func (m *Model) Accepted() bool { return m.accepted }

// Cancelled reports whether the user backed out with q/esc/ctrl+c.
func (m *Model) Cancelled() bool { return m.cancelled }

// Excludes computes the minimal exclude list for the current selection. Safe
// to call regardless of Accepted/Cancelled; Pick only uses it when accepted.
func (m *Model) Excludes() []string { return ComputeExcludes(m.root) }

// Init satisfies tea.Model. There is nothing to kick off asynchronously: the
// whole tree is already built by the time the program starts.
func (m *Model) Init() tea.Cmd { return nil }

// Update satisfies tea.Model. All it does is translate a key into a mutation
// on the *Node tree or the cursor/scroll state above; the mutation itself
// lives in tree.go so it stays testable without a KeyMsg in sight.
func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		h := msg.Height - reservedLines
		if h < 1 {
			h = 1
		}
		m.height = h
		m.ensureVisible()
		return m, nil

	case tea.KeyMsg:
		switch msg.Type {
		case tea.KeyCtrlC, tea.KeyEsc:
			m.cancelled = true
			return m, tea.Quit
		case tea.KeyEnter:
			m.accepted = true
			return m, tea.Quit
		case tea.KeyUp:
			m.moveCursor(-1)
		case tea.KeyDown:
			m.moveCursor(1)
		case tea.KeyLeft:
			m.collapseCurrent()
		case tea.KeyRight:
			m.expandCurrent()
		case tea.KeySpace:
			m.toggleCurrent()
		case tea.KeyPgUp:
			m.moveCursor(-m.height)
		case tea.KeyPgDown:
			m.moveCursor(m.height)
		case tea.KeyHome:
			m.setCursor(0)
		case tea.KeyEnd:
			m.setCursor(len(m.rows) - 1)
		case tea.KeyRunes:
			switch string(msg.Runes) {
			case "q":
				m.cancelled = true
				return m, tea.Quit
			case "k":
				m.moveCursor(-1)
			case "j":
				m.moveCursor(1)
			case "h":
				m.collapseCurrent()
			case "l":
				m.expandCurrent()
			case "a":
				SetAll(m.root, Checked)
			case "n":
				SetAll(m.root, Unchecked)
			}
		}
	}
	return m, nil
}

// moveCursor shifts the cursor by delta rows (negative moves up), clamping to
// the row list and pulling the viewport along with it.
func (m *Model) moveCursor(delta int) {
	if len(m.rows) == 0 {
		return
	}
	m.setCursor(m.cursor + delta)
}

// setCursor moves the cursor to an absolute row index, clamped to range.
func (m *Model) setCursor(i int) {
	if len(m.rows) == 0 {
		m.cursor, m.offset = 0, 0
		return
	}
	if i < 0 {
		i = 0
	}
	if i > len(m.rows)-1 {
		i = len(m.rows) - 1
	}
	m.cursor = i
	m.ensureVisible()
}

// ensureVisible slides the scroll offset just far enough that the cursor row
// is inside [offset, offset+height) -- the viewport window that keeps a
// 60,000-row tree from ever being rendered in full.
func (m *Model) ensureVisible() {
	if len(m.rows) == 0 {
		m.offset = 0
		return
	}
	if m.cursor < m.offset {
		m.offset = m.cursor
	}
	if m.height > 0 && m.cursor >= m.offset+m.height {
		m.offset = m.cursor - m.height + 1
	}
	if m.offset < 0 {
		m.offset = 0
	}
	maxOffset := len(m.rows) - m.height
	if maxOffset < 0 {
		maxOffset = 0
	}
	if m.offset > maxOffset {
		m.offset = maxOffset
	}
}

// refreshRows rebuilds the visible-row list after a structural change
// (expand/collapse). It is not called on every keystroke on purpose: a plain
// cursor move or a checkbox toggle never changes which rows are visible.
func (m *Model) refreshRows() {
	m.rows = VisibleRows(m.root)
	if m.cursor > len(m.rows)-1 {
		m.cursor = len(m.rows) - 1
	}
	if m.cursor < 0 {
		m.cursor = 0
	}
	m.ensureVisible()
}

// current returns the node under the cursor, or nil if the tree is empty.
func (m *Model) current() *Node {
	if len(m.rows) == 0 {
		return nil
	}
	return m.rows[m.cursor].Node
}

// expandCurrent opens a collapsed directory. Anything else -- a file, an
// already-open directory, an empty directory with nothing to show -- is left
// alone rather than treated as an error.
func (m *Model) expandCurrent() {
	n := m.current()
	if n == nil || !n.expandable() || n.Expanded {
		return
	}
	n.Expanded = true
	m.refreshRows()
}

// collapseCurrent closes an open directory, or -- since there is then nothing
// left to collapse -- jumps the cursor to the parent instead. That second
// behavior is what makes "left" a reliable way to walk back up the tree
// rather than a no-op the moment a directory is already shut.
func (m *Model) collapseCurrent() {
	n := m.current()
	if n == nil {
		return
	}
	if n.expandable() && n.Expanded {
		n.Expanded = false
		m.refreshRows()
		return
	}
	if n.Parent != nil && n.Parent != m.root {
		m.jumpTo(n.Parent)
	}
}

// jumpTo moves the cursor to wherever target currently sits in the visible
// row list. It is only reached on an explicit "collapse with nothing to
// collapse" left-press, not on every keystroke, so the linear scan it costs
// is rare rather than a hot path.
func (m *Model) jumpTo(target *Node) {
	for i, r := range m.rows {
		if r.Node == target {
			m.setCursor(i)
			return
		}
	}
}

// toggleCurrent flips the checked state of the node under the cursor.
func (m *Model) toggleCurrent() {
	if n := m.current(); n != nil {
		Toggle(n)
	}
}

// --- rendering ---

var (
	headerStyle = lipgloss.NewStyle().Bold(true)
	helpStyle   = lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "240", Dark: "245"})
	footerStyle = lipgloss.NewStyle().Bold(true)

	// Check-state colors are the only place this UI adds color, and each one
	// is a light/dark pair rather than a fixed code -- the terminal's own
	// background is never assumed.
	checkedStyle   = lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "28", Dark: "42"})
	partialStyle   = lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "130", Dark: "214"})
	uncheckedStyle = lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "241", Dark: "246"})
	cursorStyle    = lipgloss.NewStyle().Reverse(true)
)

// View satisfies tea.Model. It renders exactly three things: a header with
// the tree's totals, a viewport-sized window of rows, and a footer with the
// live selected totals -- never the whole tree, no matter how large it is.
func (m *Model) View() string {
	var b strings.Builder
	b.WriteString(m.headerView())
	b.WriteString("\n\n")

	if len(m.rows) == 0 {
		b.WriteString("(nothing found)\n")
	} else {
		end := m.offset + m.height
		if end > len(m.rows) {
			end = len(m.rows)
		}
		for i := m.offset; i < end; i++ {
			b.WriteString(m.renderRow(m.rows[i], i == m.cursor))
			b.WriteString("\n")
		}
	}

	b.WriteString("\n")
	b.WriteString(m.footerView())
	return b.String()
}

func (m *Model) headerView() string {
	title := m.root.Name + "  (" + progress.FormatBytes(m.root.TotalSize) + ", " + countLabel(m.root.TotalFiles) + ")"
	help := "up/down move  left/right expand  space toggle  a all  n none  enter accept  q cancel"
	return headerStyle.Render(title) + "\n" + helpStyle.Render(help)
}

func (m *Model) footerView() string {
	return footerStyle.Render("Will back up: " + progress.FormatBytes(m.root.SelectedSize) + " in " + countLabel(m.root.SelectedFiles))
}

// checkbox renders the tri-state box. "[~]" for partial is deliberate: a
// two-state box has no way to say "some of this, not all of it" honestly.
func checkbox(c CheckState) string {
	switch c {
	case Checked:
		return "[x]"
	case Partial:
		return "[~]"
	default:
		return "[ ]"
	}
}

func countLabel(n int64) string {
	if n == 1 {
		return "1 item"
	}
	return strconv.FormatInt(n, 10) + " items"
}

// renderRow lays out one line: indent, expand marker, checkbox, name, then
// size/count pushed to the right edge of the terminal width.
func (m *Model) renderRow(r Row, isCursor bool) string {
	n := r.Node

	marker := " "
	if n.expandable() {
		if n.Expanded {
			marker = "-"
		} else {
			marker = "+"
		}
	}

	left := strings.Repeat("  ", r.Depth) + marker + " " + checkbox(n.Check) + " " + n.Name

	var right string
	if n.IsDir {
		right = progress.FormatBytes(n.TotalSize) + "  " + countLabel(n.TotalFiles)
	} else {
		right = progress.FormatBytes(n.TotalSize)
	}

	line := padBetween(left, right, m.width)

	style := uncheckedStyle
	switch n.Check {
	case Checked:
		style = checkedStyle
	case Partial:
		style = partialStyle
	}
	if isCursor {
		style = style.Reverse(true)
	}
	return style.Render(line)
}

// padBetween right-aligns right against left within width, using at least a
// two-space gap so a long name never runs into the numbers.
func padBetween(left, right string, width int) string {
	gap := width - lipgloss.Width(left) - lipgloss.Width(right)
	if gap < 2 {
		gap = 2
	}
	return left + strings.Repeat(" ", gap) + right
}
