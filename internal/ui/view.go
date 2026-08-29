package ui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/help"
	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/list"
	"github.com/charmbracelet/bubbles/table"
	"github.com/charmbracelet/lipgloss"

	"github.com/saurabhhbansal/r2backup/internal/progress"
)

// chromeHeight is how many rows everything that is not the body takes.
//
// The +2 is the panel's own top and bottom border. Getting it wrong is not a
// cosmetic matter: the body is sized against this, the frame then comes out
// one row taller than the terminal, and MaxHeight clips the bottom line --
// which is the keyboard help, the one thing on screen telling a new user what
// they can press.
func (m *Model) chromeHeight() int {
	return lipgloss.Height(m.header()) + lipgloss.Height(m.footer()) + 2
}

// layout resizes every component to the current terminal. Called on every
// WindowSizeMsg rather than computed inside View, because the list and the
// viewport both hold their own dimensions and scroll against them.
func (m *Model) layout() {
	body := m.height - m.chromeHeight()
	if body < 3 {
		body = 3
	}
	inner := m.width - 4
	if inner < 20 {
		inner = 20
	}
	m.list.SetSize(inner, body)
	m.list.SetDelegate(setDelegate{width: inner})
	// Resized, never rebuilt. layout runs on every screen change, and a fresh
	// viewport.New here threw away the content showDetail had just put in it
	// -- the detail screen rendered as an empty box.
	m.detail.Width, m.detail.Height = inner, body
	if m.detailBody != "" {
		m.detail.SetContent(m.detailBody)
	}
	m.help.Width = m.width
	m.bar.Width = min(inner-2, 60)
	if m.screen == screenTrash {
		m.buildTrashTable()
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func (m *Model) syncList() {
	items := make([]list.Item, 0, len(m.sets))
	for _, s := range m.sets {
		items = append(items, setItem{v: s})
	}
	m.list.SetItems(items)
}

// header is the banner plus the one line that says where this is pointed.
func (m *Model) header() string {
	var b strings.Builder
	// The art is nineteen rows. That is a reasonable opening, and an
	// unreasonable permanent cost: on a 46-row terminal it is 40% of the
	// window, and a detail screen has better uses for it. So the wordmark
	// greets you on the way in and stands aside once you are working.
	art := bannerStyle.Render("r2backup")
	if m.screen == screenHome {
		art = Banner(m.width, m.height)
	}
	b.WriteString(m.fit(art))
	b.WriteString("\n")

	where := m.ov.Machine
	if m.ov.Bucket != "" {
		where += dimStyle.Render(" → ") + m.ov.Bucket
	}
	b.WriteString(m.fit(dimStyle.Render(where)))
	return b.String()
}

// footer carries the two facts a person checks without asking -- whether this
// runs by itself, and how much of the free tier is gone -- plus the key help
// and whatever just happened.
//
// Every line goes through fit. lipgloss does not wrap for us and
// JoinVertical pads every line out to the widest one, so a single line one
// column too long does not just overflow itself: it widens the whole frame,
// and the border and the list below it staircase off the right edge. The
// operations counter alone is 64 columns, which is more than a 60-column pane
// has. TestNoScreenOverflowsTheTerminal is what found this.
func (m *Model) footer() string {
	var parts []string

	if m.ov.Scheduled {
		parts = append(parts, goodStyle.Render("● automatic")+dimStyle.Render(" every "+m.ov.Interval.Round(time.Minute).String()))
	} else {
		parts = append(parts, warnStyle.Render("○ manual only")+dimStyle.Render(" (s to schedule)"))
	}
	if m.ov.OpsLimit > 0 {
		parts = append(parts, dimStyle.Render(fmt.Sprintf("%s / %s operations this month",
			progress.FormatCount(int64(m.ov.OpsUsed)), progress.FormatCount(int64(m.ov.OpsLimit)))))
	}

	var b strings.Builder
	b.WriteString(m.fit(strings.Join(parts, dimStyle.Render("  ·  "))))
	b.WriteString("\n")

	switch {
	case m.err != nil:
		b.WriteString(m.fit(errorStyle.Render("! " + m.err.Error())))
	case m.status != "":
		b.WriteString(m.fit(statusStyle.Render(m.status)))
	default:
		b.WriteString(m.fit(m.help.ShortHelpView(m.shortHelp())))
	}
	return b.String()
}

// shortHelp is the footer's binding list, trimmed for the room there is.
//
// On an 80-column terminal the full short line is a few columns too long and
// bubbles/help cuts the tail, which took "q quit" off the screen -- leaving a
// full-screen program with no visible way out. Narrow windows lose the
// middle instead.
func (m *Model) shortHelp() []key.Binding {
	if m.width < 76 {
		return []key.Binding{keys.Up, keys.Enter, keys.Help, keys.Quit}
	}
	return keys.ShortHelp()
}

// fit truncates a rendered line to the terminal width, preserving styling.
// MaxWidth is lipgloss's own truncation, so it counts display cells rather
// than bytes and does not cut an escape sequence in half.
func (m *Model) fit(s string) string {
	return lipgloss.NewStyle().MaxWidth(m.width).Render(s)
}

func (m *Model) View() string {
	if m.quit {
		// Leave the terminal as it was found rather than painting one last
		// frame over the shell prompt.
		return ""
	}

	var body string
	switch m.screen {
	case screenHome:
		body = m.homeView()
	case screenDetail:
		body = m.detail.View()
	case screenTrash:
		body = m.trashView()
	case screenRunning:
		body = m.runningView()
	case screenConfirm:
		body = m.confirmView()
	case screenHelp:
		body = m.helpView()
	}

	frame := lipgloss.JoinVertical(lipgloss.Left,
		m.header(),
		panelStyle.Width(m.width-2).Render(body),
		m.footer(),
	)
	// A backstop, not a substitute for the per-line fits above: it keeps a
	// body that grows a long line later from widening the frame, but a line
	// truncated here is still a line the user cannot read.
	return lipgloss.NewStyle().MaxWidth(m.width).MaxHeight(m.height).Render(frame)
}

func (m *Model) homeView() string {
	if len(m.sets) == 0 {
		return strings.Join([]string{
			"",
			titleStyle.Render("  Nothing is being backed up yet."),
			"",
			dimStyle.Render("  Press ") + titleStyle.Render("a") + dimStyle.Render(" to pick a folder."),
			"",
		}, "\n")
	}
	// A run started elsewhere -- by the OS scheduler, or another window --
	// is shown here rather than hidden, because otherwise the numbers move on
	// their own with no explanation.
	var head string
	if m.ov.Running != "" && !m.running {
		head = m.spin.View() + " " +
			warnStyle.Render("running now: "+m.ov.Running) + " " +
			dimStyle.Render(m.ov.RunETA) + "\n"
	}
	return head + m.list.View()
}

func (m *Model) showDetail(v SetView) {
	var b strings.Builder
	row := func(label, value string) {
		b.WriteString(labelStyle.Render(label) + value + "\n")
	}

	b.WriteString(titleStyle.Render(v.Name) + "  " + stateStyle(v.State).Render(v.State) + "\n\n")
	row("folder", v.Root)
	row("in bucket", v.Prefix)
	row("objects", progress.FormatCount(int64(v.Objects)))
	if v.Retention > 0 {
		row("trash kept", fmt.Sprintf("%d days", v.Retention))
	} else {
		row("trash", "off — deletions are permanent")
	}

	if len(v.Excludes) > 0 {
		b.WriteString("\n" + titleStyle.Render("Left out") + "\n")
		for _, e := range v.Excludes {
			b.WriteString("  " + dimStyle.Render(e) + "\n")
		}
	}

	b.WriteString("\n" + titleStyle.Render("Last run") + "\n")
	switch {
	case !v.HasRun:
		b.WriteString("  " + dimStyle.Render("never") + "\n")
	case v.State == "failed":
		b.WriteString("  " + badStyle.Render(humanAgo(time.Since(v.LastRun))+" — failed") + "\n")
		b.WriteString("  " + v.Note + "\n")
	default:
		row("  when", humanAgo(time.Since(v.LastRun)))
		row("  uploaded", progress.FormatCount(int64(v.Uploaded))+" ("+progress.FormatBytes(v.Bytes)+")")
		row("  unchanged", progress.FormatCount(int64(v.Unchanged)))
		row("  deleted", progress.FormatCount(int64(v.Deleted)))
		row("  operations", progress.FormatCount(int64(v.Operations)))
	}

	if v.Failures+v.Problems+v.Collisions > 0 {
		b.WriteString("\n" + badStyle.Render("Needs a look") + "\n")
		b.WriteString(fmt.Sprintf("  %d failed · %d unreadable · %d name collisions\n",
			v.Failures, v.Problems, v.Collisions))
		for _, e := range v.Examples {
			b.WriteString("  " + dimStyle.Render(e) + "\n")
		}
	}

	b.WriteString("\n" + dimStyle.Render("b back up · e change what is included · r restore · t trash · esc back"))

	m.detailBody = b.String()
	m.detail.SetContent(m.detailBody)
	m.detail.GotoTop()
	m.screen = screenDetail
}

func (m *Model) showTrash(msg trashMsg) {
	m.trashSet = msg.set
	m.trashRows = msg.rows
	m.trashCount = len(msg.rows)
	m.status = ""
	m.screen = screenTrash
	m.buildTrashTable()
}

// buildTrashTable sizes the columns to the window.
//
// bubbles/table does not resize columns for you: SetWidth changes the frame
// and leaves the column widths alone, so the table has to be rebuilt whenever
// the space changes. The arithmetic is exact rather than approximate because
// being two columns over does not clip -- the header wraps onto a second line
// and the rule under it wraps with it, which looks like a rendering bug.
func (m *Model) buildTrashTable() {
	const sizeW, deletedW, untilW = 10, 14, 12
	// Each cell carries one column of padding on each side.
	const padding = 2 * 4
	inner := m.width - 4
	fileW := inner - sizeW - deletedW - untilW - padding
	if fileW < 12 {
		fileW = 12
	}

	cols := []table.Column{
		{Title: "File", Width: fileW},
		{Title: "Size", Width: sizeW},
		{Title: "Deleted", Width: deletedW},
		{Title: "Until", Width: untilW},
	}
	rows := make([]table.Row, 0, len(m.trashRows))
	for _, r := range m.trashRows {
		rows = append(rows, table.Row{
			truncate(r.Key, fileW),
			progress.FormatBytes(r.Size),
			r.Deleted.Format("2 Jan 15:04"),
			r.Expires.Format("2 Jan"),
		})
	}

	st := table.DefaultStyles()
	st.Header = st.Header.BorderStyle(lipgloss.NormalBorder()).
		BorderForeground(subtle).BorderBottom(true).Bold(true)
	st.Selected = st.Selected.Foreground(lipgloss.Color("0")).Background(accent)

	height := m.height - m.chromeHeight() - 3
	if height < 3 {
		height = 3
	}
	m.trash = table.New(
		table.WithColumns(cols),
		table.WithRows(rows),
		table.WithFocused(true),
		table.WithHeight(height),
		table.WithStyles(st),
	)
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func (m *Model) trashView() string {
	head := titleStyle.Render("Trash · "+m.trashSet) + "\n"
	if m.trashCount == 0 {
		return head + "\n" + dimStyle.Render("  Nothing recoverable. Every file here is the current one.") + "\n"
	}
	return head + m.trash.View() + "\n" +
		dimStyle.Render("Recover one with: r2b restore "+m.trashSet+" --deleted <file>")
}

func (m *Model) runningView() string {
	var b strings.Builder
	b.WriteString(titleStyle.Render("Backing up "+m.runSet) + "\n\n")
	b.WriteString(m.spin.View() + " " + m.runPhase + "\n\n")

	s := m.runSnap
	if s.BytesTotal > 0 {
		b.WriteString(m.bar.ViewAs(float64(s.BytesDone)/float64(s.BytesTotal)) + "\n\n")
		b.WriteString(fmt.Sprintf("%s of %s   ·   %s files   ·   %s\n",
			progress.FormatBytes(s.BytesDone), progress.FormatBytes(s.BytesTotal),
			progress.FormatCount(s.FilesDone), etaText(s)))
	}
	b.WriteString("\n" + dimStyle.Render("esc returns to the list and leaves this running · q cancels it"))
	return b.String()
}

func etaText(s progress.Snapshot) string {
	if !s.ETAKnown {
		return "estimating..."
	}
	return progress.FormatDuration(s.ETA) + " remaining"
}

func (m *Model) confirmView() string {
	return "\n" + titleStyle.Render("  "+m.confirm) + "\n\n" +
		"  " + warnStyle.Render("y") + dimStyle.Render(" yes") +
		"    " + titleStyle.Render("n") + dimStyle.Render(" no (default)") + "\n"
}

func (m *Model) helpView() string {
	return "\n" + m.help.ShortHelpView(keys.ShortHelp()) + "\n\n" +
		lipgloss.NewStyle().Render(fullHelpBlock()) + "\n"
}

func fullHelpBlock() string {
	h := help.New()
	h.ShowAll = true
	return h.View(keys)
}
