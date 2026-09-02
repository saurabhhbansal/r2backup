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
// The +2 is the panel's own top and bottom border. Getting it wrong is not
// cosmetic: the body is sized against this, the frame comes out a row taller
// than the terminal, and the bottom line -- the keyboard help -- is clipped.
func (m *Model) chromeHeight() int {
	return lipgloss.Height(m.header()) + lipgloss.Height(m.footer()) + 2
}

// layout resizes every component to the current terminal. Called on every
// size change and every screen change, because the list, the viewport, the
// table and the browser all hold their own dimensions and scroll against them.
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
	// Resized, never rebuilt: a fresh viewport.New here would throw away the
	// content showDetail just put in it.
	m.detail.Width, m.detail.Height = inner, body
	if m.detailBody != "" {
		m.detail.SetContent(m.detailBody)
	}
	m.help.Width = m.width
	m.bar.Width = min(inner-2, 60)
	m.browse.SetHeight(max(3, body-4))
	if m.overlay == overlayNone && m.tab == tabTrash {
		m.buildTrashTable()
	}
	// Same reason as the trash table: bubbles/table leaves its columns alone
	// when the frame changes, so a resize has to rebuild them or the header
	// wraps and takes the rule under it with it.
	if m.overlay == overlayObjects {
		m.buildObjectTable()
	}
	if m.overlay == overlayRemote {
		m.buildRemoteTable()
	}
}

// styledTable builds a table in the interface's own colours, keeping the
// cursor where it was.
//
// Every table here is rebuilt rather than resized -- see buildTrashTable --
// and rebuilding one is also what layout() does when the user opens help and
// closes it again. Carrying the cursor across is the difference between that
// and being thrown back to the first row every time.
func styledTable(cols []table.Column, rows []table.Row, height, cursor int) table.Model {
	st := table.DefaultStyles()
	st.Header = st.Header.BorderStyle(lipgloss.NormalBorder()).
		BorderForeground(subtle).BorderBottom(true).Bold(true)
	st.Selected = st.Selected.Foreground(lipgloss.Color("0")).Background(accent)
	t := table.New(
		table.WithColumns(cols), table.WithRows(rows),
		table.WithFocused(true), table.WithHeight(height), table.WithStyles(st),
	)
	if cursor > 0 && cursor < len(rows) {
		t.SetCursor(cursor)
	}
	return t
}

// tableHeight is the room a full-width table has inside the panel, after its
// own title and hint lines.
func (m *Model) tableHeight() int {
	h := m.height - m.chromeHeight() - 3
	if h < 3 {
		h = 3
	}
	return h
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func max(a, b int) int {
	if a > b {
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

// header is the wordmark, the tab bar, and the one line saying where this is
// pointed.
func (m *Model) header() string {
	var b strings.Builder
	// Banner now has a rung for nearly any size, not just the nineteen-row
	// original, so the cost that used to confine the art to the folder list
	// is mostly gone -- every overlay-free screen picks whatever rung fits.
	// An overlay is a different kind of screen: a form, a confirmation, a
	// running backup. It is a focused, momentary interaction rather than a
	// place to greet, so it keeps the plain word rather than competing with
	// the art for attention.
	art := bannerStyle.Render("r2backup")
	if m.overlay == overlayNone {
		art = Banner(m.width, m.height)
	}
	b.WriteString(m.fit(art))
	b.WriteString("\n")
	b.WriteString(m.fit(m.tabBar()))
	b.WriteString("\n")

	where := m.ov.Machine
	if m.ov.Bucket != "" {
		where += dimStyle.Render(" → ") + m.ov.Bucket
	}
	if m.ov.Version != "" {
		where += dimStyle.Render("   v" + m.ov.Version)
	}
	b.WriteString(m.fit(dimStyle.Render(where)))
	return b.String()
}

// tabBar draws the modes. They are numbered because a number is the fastest
// way to reach one and the only discoverable way before you have found tab.
func (m *Model) tabBar() string {
	var parts []string
	for t := tab(0); t < numTabs; t++ {
		label := fmt.Sprintf("%d %s", int(t)+1, t)
		style := tabStyle
		if t == m.tab {
			style = activeTabStyle
		}
		parts = append(parts, style.Render(label))
		if t < numTabs-1 {
			parts = append(parts, tabGapStyle.Render(" "))
		}
	}
	// JoinHorizontal, not concatenation: a styled cell can be more than one
	// line and joining the strings would stack them.
	return lipgloss.JoinHorizontal(lipgloss.Top, parts...)
}

// footer is the keys, and whatever just happened. Nothing else.
//
// It used to carry a standing status line above the keys as well -- whether
// backups were automatic, and how many operations the month had used. Both
// are real answers to questions a person asks, but neither is a question
// they are asking *while reading the keys for the screen they are on*, and
// paying a permanent row of the window for them on every screen is how the
// list ended up a row shorter than it needed to be everywhere. They live on
// the Account tab now, next to the rest of this computer's standing state,
// where they can be read together instead of one at a time.
//
// Every line goes through fit. lipgloss does not wrap and JoinVertical pads
// every line to the widest, so one line a column too long does not overflow
// itself -- it widens the whole frame and staircases the border.
func (m *Model) footer() string {
	switch {
	case m.err != nil:
		return m.fit(errorStyle.Render("! " + m.err.Error()))
	case m.notice != "":
		return m.fit(statusStyle.Render(m.notice))
	default:
		return m.fit(m.help.ShortHelpView(m.shortHelp()))
	}
}

// shortHelp is the footer's binding list, trimmed for the room there is.
//
// bubbles/help cuts the tail, and on an 80-column terminal the full line was
// a few columns too long -- which took "q quit" off the screen and left a
// full-screen program with no visible way out. So the trimming happens here
// and it takes from the middle: the way out and the way to the next mode are
// the two that must survive any width.
//
// It measures rather than guesses. The list used to be cut at two fixed
// widths, which meant every key added to a mode made the line longer without
// changing where it was cut -- and the mode with the most to offer was the
// one whose help fell off the end first.
func (m *Model) shortHelp() []key.Binding {
	if m.overlay != overlayNone {
		return []key.Binding{keys.Back, keys.Quit}
	}
	base := append([]key.Binding{keys.NextTab}, tabKeys(m.tab)...)
	base = append(base, keys.Help, keys.Quit)
	// Two from the front, two from the back: tab and the mode's first key,
	// then ? and q. Below that there is nothing left to drop.
	for len(base) > 4 && helpWidth(base) > m.width {
		cut := len(base) / 2
		base = append(base[:cut:cut], base[cut+1:]...)
	}
	if helpWidth(base) > m.width {
		return []key.Binding{keys.NextTab, keys.Help, keys.Quit}
	}
	return base
}

// measure renders the help line at its natural width.
//
// m.help cannot be asked: layout() gives it the terminal's width, and
// bubbles/help truncates to that before returning -- so measuring through it
// reports the width it was clipped to and the line is never found to be too
// long. A help.Model with no width set does not truncate.
var measure = help.New()

func helpWidth(b []key.Binding) int {
	return lipgloss.Width(measure.ShortHelpView(b))
}

func (m *Model) fit(s string) string {
	return lipgloss.NewStyle().MaxWidth(m.width).Render(s)
}

func (m *Model) View() string {
	if m.quit {
		// Leave the terminal as it was found rather than painting one last
		// frame over the shell prompt.
		return ""
	}

	body := m.bodyView()
	frame := lipgloss.JoinVertical(lipgloss.Left,
		m.header(),
		panelStyle.Width(m.width-2).Render(body),
		m.footer(),
	)
	// A backstop, not a substitute for the per-line fits above.
	return lipgloss.NewStyle().MaxWidth(m.width).MaxHeight(m.height).Render(frame)
}

func (m *Model) bodyView() string {
	switch m.overlay {
	case overlayDetail:
		return m.detail.View()
	case overlayHelp:
		return m.helpView()
	case overlayConfirm:
		return m.confirmView()
	case overlayRunning:
		return m.runningView()
	case overlayForm:
		return m.form.View(m.width - 4)
	case overlayBrowse:
		return m.browseView()
	case overlayPicker:
		return m.pickerView()
	case overlayObjects:
		return m.objectsView()
	case overlayRemote:
		return m.remoteView()
	}
	switch m.tab {
	case tabFolders:
		return m.foldersView()
	case tabSchedule:
		return m.scheduleView()
	case tabTrash:
		return m.trashView()
	case tabAccount:
		return m.accountView()
	}
	return ""
}

// --- Folders ---

func (m *Model) foldersView() string {
	if !m.ov.Configured {
		return "\n" + titleStyle.Render("  This computer is not set up yet.") + "\n\n" +
			dimStyle.Render("  Go to ") + titleStyle.Render("4 Account") + dimStyle.Render(" and either sign in, or enter your R2 keys.") + "\n"
	}
	if len(m.sets) == 0 {
		// c is offered here and not only in the footer. An empty list on a
		// computer that has just signed in is exactly the moment someone
		// needs to be told their data is in the bucket and reachable.
		return "\n" + titleStyle.Render("  Nothing is being backed up yet.") + "\n\n" +
			dimStyle.Render("  Press ") + titleStyle.Render("a") + dimStyle.Render(" to choose a folder, or ") +
			titleStyle.Render("c") + dimStyle.Render(" to see what is already in your bucket.") + "\n"
	}
	var head string
	// A run started elsewhere -- by the scheduler, or another window -- is
	// shown rather than hidden, because otherwise the numbers move on their
	// own with no explanation.
	switch {
	case m.running:
		// This window's own run, left with esc. Suppressing the banner here
		// left a backup in flight with nothing on screen saying so and no way
		// back to the progress screen.
		head = m.spin.View() + " " + warnStyle.Render("backing up "+m.runWhat) + " " +
			dimStyle.Render("· w to watch") + "\n"
	case m.ov.Running != "":
		head = m.spin.View() + " " + warnStyle.Render("running now: "+m.ov.Running) + " " +
			dimStyle.Render(m.ov.RunETA) + "\n"
	case m.ov.Interrupted != "":
		head = warnStyle.Render("⏸ "+m.ov.Interrupted+" was interrupted") + " " +
			dimStyle.Render(interruptedNote(m.ov)) + "\n"
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
		b.WriteString("  " + badStyle.Render(humanAgo(time.Since(v.LastRun))+" — failed") + "\n  " + v.Note + "\n")
	case v.State == "cancelled":
		// warnStyle, not badStyle: matches stateStyle's own colour choice
		// for this state, since a stopped run is not a failure.
		b.WriteString("  " + warnStyle.Render(humanAgo(time.Since(v.LastRun))+" — cancelled") + "\n  " + v.Note + "\n")
	default:
		row("  when", humanAgo(time.Since(v.LastRun)))
		row("  uploaded", progress.FormatCount(int64(v.Uploaded))+" ("+progress.FormatBytes(v.Bytes)+")")
		row("  unchanged", progress.FormatCount(int64(v.Unchanged)))
		row("  deleted", progress.FormatCount(int64(v.Deleted)))
		row("  operations", progress.FormatCount(int64(v.Operations)))
	}
	if v.Failures+v.Problems+v.Collisions > 0 {
		b.WriteString("\n" + badStyle.Render("Needs a look") + "\n")
		fmt.Fprintf(&b, "  %d failed · %d unreadable · %d name collisions\n", v.Failures, v.Problems, v.Collisions)
		for _, e := range v.Examples {
			b.WriteString("  " + dimStyle.Render(e) + "\n")
		}
	}
	b.WriteString("\n" + dimStyle.Render("b back up · e change what is included · r restore · f what is stored\nn rename · m moved · x stop backing up · X stop and delete · esc back"))

	m.detailBody = b.String()
	m.detail.SetContent(m.detailBody)
	m.detail.GotoTop()
	m.overlay = overlayDetail
}

// --- Schedule ---

func (m *Model) scheduleView() string {
	var b strings.Builder
	b.WriteString(titleStyle.Render("Automatic backups") + "\n\n")

	if !m.ov.SchedulerAvailable {
		b.WriteString(badStyle.Render("  No scheduler is available on this platform.") + "\n")
		return b.String()
	}

	if m.ov.Scheduled {
		b.WriteString("  " + goodStyle.Render("● On") + dimStyle.Render(" — every ") +
			titleStyle.Render(humanEvery(m.ov.Interval)) + "\n\n")
		if !m.ov.NextRun.IsZero() {
			b.WriteString(labelStyle.Render("  next run") + m.ov.NextRun.Format("Mon 2 Jan, 15:04") + "\n")
		}
		if !m.ov.LastRun.IsZero() {
			b.WriteString(labelStyle.Render("  last run") + humanAgo(time.Since(m.ov.LastRun)) + "\n")
		}
		b.WriteString(labelStyle.Render("  covers") + fmt.Sprintf("all %d folders", len(m.sets)) + "\n")
		if !m.ov.RunsWhenSignedOut {
			b.WriteString("\n" + dimStyle.Render("  It runs while you are signed in.") + "\n")
		} else {
			b.WriteString("\n" + dimStyle.Render("  It runs whether or not you are signed in.") + "\n")
		}
	} else {
		b.WriteString("  " + warnStyle.Render("○ Off") + dimStyle.Render(" — backups only run when you run them.") + "\n\n")
		b.WriteString(dimStyle.Render("  Nothing of ours stays running in between. The operating system's own\n"+
			"  scheduler starts r2b, it does its work, and it exits.") + "\n")
	}

	b.WriteString("\n  " + titleStyle.Render("s") + dimStyle.Render(" turn "))
	if m.ov.Scheduled {
		b.WriteString(dimStyle.Render("off"))
	} else {
		b.WriteString(dimStyle.Render("on"))
	}
	b.WriteString(dimStyle.Render("    ") + titleStyle.Render("e") + dimStyle.Render(" change how often"))
	b.WriteString(dimStyle.Render("    ") + titleStyle.Render("p") + dimStyle.Render(" re-point it at this copy of r2b"))
	return b.String()
}

// --- Trash ---

func (m *Model) showTrash(msg trashMsg) {
	m.trashSet = msg.set
	m.trashRows = msg.rows
	m.notice = ""
	m.tab = tabTrash
	m.buildTrashTable()
}

// buildTrashTable sizes the columns to the window.
//
// bubbles/table does not resize columns: SetWidth changes the frame and
// leaves them alone, so the table is rebuilt when the space changes. The
// arithmetic is exact rather than approximate because being two columns over
// does not clip -- the header wraps onto a second line and the rule under it
// wraps with it, which looks like a rendering bug.
func (m *Model) buildTrashTable() {
	const sizeW, deletedW, untilW = 10, 14, 12
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
	m.trash = styledTable(cols, rows, m.tableHeight(), m.trash.Cursor())
}

func (m *Model) trashView() string {
	if m.trashSet == "" {
		return "\n" + dimStyle.Render("  Choose a folder on the Folders tab first.") + "\n"
	}
	// "Trash · <folder>", not "Recoverable · <folder>". The heading names
	// which folder's trash is being shown, but a folder's own name printed
	// after the word "Recoverable" reads as a claim about the folder -- that
	// it has been deleted and can be brought back -- which is alarming and
	// wrong, and it is shown the moment you tab across here whether or not
	// anything is in it. The word still belongs on this screen; it belongs
	// attached to a count of files, where it is a fact rather than a verdict.
	head := titleStyle.Render("Trash · "+m.trashSet) + "\n"
	if len(m.trashRows) == 0 {
		return head + "\n" + dimStyle.Render("  Nothing deleted or overwritten. Every file here is the current one.") + "\n"
	}
	return head + m.trash.View() + "\n" +
		dimStyle.Render(countOf(int64(len(m.trashRows)), "file", "files")+" recoverable · enter recovers the highlighted one")
}

// interruptedNote says how far a stopped run got and what happens next.
//
// "What happens next" is the part that matters. A backup that stopped when
// the machine did is alarming on its own, and the true answer -- it carries
// on by itself, from where it stopped -- is the whole reason the resumable
// upload exists. Saying only "interrupted" would make a solved problem look
// like an unsolved one.
func interruptedNote(ov Overview) string {
	var b strings.Builder
	b.WriteString(humanAgo(time.Since(ov.InterruptedAt)))
	if ov.InterruptedTotal > 0 {
		fmt.Fprintf(&b, " · %s of %s",
			progress.FormatBytes(ov.InterruptedDone), progress.FormatBytes(ov.InterruptedTotal))
	}
	if ov.PendingFiles > 0 {
		fmt.Fprintf(&b, " · %s already sent of %d part-uploaded file(s)",
			progress.FormatBytes(ov.PendingDone), ov.PendingFiles)
	}
	b.WriteString(" · resumes by itself")
	return b.String()
}

// --- What is stored (`r2b ls`) ---

// buildObjectTable sizes the two columns to the window. The arithmetic is
// exact, like the trash table's, because being two columns over wraps the
// header rather than clipping it.
func (m *Model) buildObjectTable() {
	const sizeW = 12
	const padding = 2 * 2
	inner := m.width - 4
	fileW := inner - sizeW - padding
	if fileW < 12 {
		fileW = 12
	}
	rows := make([]table.Row, 0, len(m.objectRows))
	for _, r := range m.objectRows {
		rows = append(rows, table.Row{truncate(r.Key, fileW), progress.FormatBytes(r.Size)})
	}
	m.objects = styledTable(
		[]table.Column{{Title: "File", Width: fileW}, {Title: "Size", Width: sizeW}},
		rows, m.tableHeight(), m.objects.Cursor())
}

func (m *Model) objectsView() string {
	head := titleStyle.Render("Stored · "+m.objectSet) + "\n"
	if len(m.objectRows) == 0 {
		return head + "\n" + dimStyle.Render("  Nothing is stored for this folder yet. Press b on it to back it up.") + "\n"
	}
	var total int64
	for _, r := range m.objectRows {
		total += r.Size
	}
	return head + m.objects.View() + "\n" +
		dimStyle.Render(fmt.Sprintf("%s · %s · largest first · esc back",
			countOf(int64(len(m.objectRows)), "object", "objects"), progress.FormatBytes(total)))
}

// countOf renders a count with its noun, singular or plural, grouped the same
// way every other number in the program is.
func countOf(n int64, one, many string) string {
	noun := many
	if n == 1 {
		noun = one
	}
	return progress.FormatCount(n) + " " + noun
}

// --- Another computer's backups (`restore --machine`) ---

func (m *Model) buildRemoteTable() {
	const whereW = 22
	const padding = 2 * 3
	inner := m.width - 4
	nameW := (inner - whereW - padding) / 2
	if nameW < 10 {
		nameW = 10
	}
	machineW := inner - nameW - whereW - padding
	if machineW < 8 {
		machineW = 8
	}
	rows := make([]table.Row, 0, len(m.remoteRows))
	for _, r := range m.remoteRows {
		where := "in the bucket only"
		if r.Here {
			where = "on this computer"
		}
		rows = append(rows, table.Row{truncate(r.Name, nameW), truncate(r.Machine, machineW), where})
	}
	m.remotes = styledTable(
		[]table.Column{
			{Title: "Folder", Width: nameW},
			{Title: "Backed up from", Width: machineW},
			{Title: "", Width: whereW},
		},
		rows, m.tableHeight(), m.remotes.Cursor())
}

func (m *Model) remoteView() string {
	head := titleStyle.Render("Everything in the bucket") + "\n"
	if len(m.remoteRows) == 0 {
		return head + "\n" + dimStyle.Render("  Nothing is backed up in this bucket yet.") + "\n"
	}
	return head + m.remotes.View() + "\n" +
		dimStyle.Render("enter restores the highlighted one · esc back")
}

// --- Account ---

// humanEvery says how often a scheduled backup runs, in words.
//
// time.Duration.String() renders half an hour as "30m0s", which is a
// programmer's spelling of it: correct, and not what anybody would say out
// loud. This is read in a sentence -- "Automatic, every ..." -- so it has to
// finish that sentence the way a person would.
func humanEvery(d time.Duration) string {
	d = d.Round(time.Minute)
	h, m := int(d/time.Hour), int(d%time.Hour/time.Minute)
	switch {
	case h > 0 && m > 0:
		return fmt.Sprintf("%s %s", plural(h, "hour"), plural(m, "minute"))
	case h > 0:
		return plural(h, "hour")
	case m > 0:
		return plural(m, "minute")
	default:
		return "minute"
	}
}

func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

// accountView is where this computer's standing state is answered in one
// place: what it is pointed at, whether it backs itself up, what that has
// cost this month, and who -- if anyone -- it is signed in as.
//
// The first three of those used to be scattered: the bucket here, automatic
// or not in a permanent footer line on every screen, the operations count
// beside it. Read one at a time they each raise the next question, which is
// why they are read together now.
func (m *Model) accountView() string {
	var b strings.Builder
	b.WriteString(titleStyle.Render("This computer") + "\n\n")
	if m.ov.Configured {
		b.WriteString("  " + goodStyle.Render("● Ready") + dimStyle.Render(" — bucket ") + m.ov.Bucket + "\n")
	} else {
		b.WriteString("  " + badStyle.Render("● No credentials yet") + dimStyle.Render(" — sign in, or press k to enter your R2 keys.") + "\n")
	}
	if m.ov.Scheduled {
		b.WriteString("  " + goodStyle.Render("● Automatic") + dimStyle.Render(" — every "+humanEvery(m.ov.Interval)))
		if !m.ov.NextRun.IsZero() {
			b.WriteString(dimStyle.Render(", next " + m.ov.NextRun.Format("Mon 2 Jan 15:04")))
		}
		b.WriteString("\n")
	} else {
		b.WriteString("  " + warnStyle.Render("○ Manual") + dimStyle.Render(" — backups run only when you run them. 2 Schedule turns this on.") + "\n")
	}
	// The allowance is worth showing even at nought used, because the number
	// people want is not what they have spent -- it is how much room is left
	// before spending anything at all.
	if m.ov.OpsLimit > 0 {
		line := fmt.Sprintf("%s of %s operations this month",
			progress.FormatCount(int64(m.ov.OpsUsed)), progress.FormatCount(int64(m.ov.OpsLimit)))
		if !m.ov.OpsResetAt.IsZero() {
			line += ", resets " + m.ov.OpsResetAt.Format("2 Jan")
		}
		b.WriteString("  " + dimStyle.Render(line) + "\n")
	}

	b.WriteString("\n" + titleStyle.Render("Account") + "\n\n")
	// Signed in is checked before the error, not after. It used to be the
	// other way round, and the two are not alternatives: the device list and
	// the vault are fetched separately, so a perfectly good session whose
	// vault check happened to fail rendered as nothing but a raw error --
	// indistinguishable, to the person reading it, from having been signed
	// out. Whatever went wrong is worth saying, underneath, but it does not
	// get to overwrite the answer to "am I signed in".
	switch {
	case m.acct.SignedIn:
		b.WriteString("  " + goodStyle.Render("● Signed in") + dimStyle.Render(" as ") + m.acct.Email + "\n")
		if m.acct.VaultStored {
			b.WriteString("  " + dimStyle.Render("Your keys are saved for other computers.") + "\n")
		} else {
			b.WriteString("  " + warnStyle.Render("Your keys are not saved yet") + dimStyle.Render(" — press p to save them.") + "\n")
		}
		if m.acct.Err != "" {
			b.WriteString("  " + warnStyle.Render(m.acct.Err) + "\n")
		}
	case m.acct.Err != "":
		b.WriteString("  " + badStyle.Render(m.acct.Err) + "\n")
	default:
		b.WriteString("  " + dimStyle.Render("Not signed in. An account lets your other computers pick up these") + "\n")
		b.WriteString("  " + dimStyle.Render("credentials without you typing them again. It is optional.") + "\n")
	}

	if len(m.acct.Devices) > 0 {
		b.WriteString("\n" + titleStyle.Render("Computers") + "\n")
		for _, d := range m.acct.Devices {
			marker := "  "
			if d.This {
				marker = lipgloss.NewStyle().Foreground(accent).Render("▸ ")
			}
			b.WriteString(marker + pad(d.Name, 22) + dimStyle.Render(pad(d.OS, 10)+"last seen "+d.LastSeen.Format("2 Jan 15:04")) + "\n")
		}
	}

	b.WriteString("\n" + dimStyle.Render("  i sign in · d download saved keys · p save keys for other computers\n  k enter R2 keys · o sign out · u check for an update"))
	return b.String()
}

// --- overlays ---

func (m *Model) browseView() string {
	hidden := ". shows hidden folders"
	if m.browse.ShowHidden {
		hidden = ". hides them again"
	}
	// The hint used to say "enter opens a folder", which is not what enter
	// does: with DirAllowed the filepicker reports a selection on enter, so
	// enter chooses. Navigation is the right arrow. A hint that describes the
	// wrong key is worse than none.
	return titleStyle.Render("Which folder should be backed up?") + "\n" +
		dimStyle.Render(m.browse.CurrentDirectory) + "\n\n" +
		m.browse.View() + "\n" +
		dimStyle.Render("→ opens · enter chooses · ← up · "+hidden+" · t types a path · esc cancels")
}

func (m *Model) pickerView() string {
	if m.picker == nil {
		return ""
	}
	return m.picker.View()
}

func (m *Model) runningView() string {
	var b strings.Builder
	b.WriteString(titleStyle.Render(m.runWhat) + "\n\n")
	b.WriteString(m.spin.View() + " " + m.runPhase + "\n\n")
	s := m.runSnap
	if s.BytesTotal > 0 {
		b.WriteString(m.bar.ViewAs(float64(s.BytesDone)/float64(s.BytesTotal)) + "\n\n")
		b.WriteString(fmt.Sprintf("%s of %s   ·   %s files   ·   %s\n",
			progress.FormatBytes(s.BytesDone), progress.FormatBytes(s.BytesTotal),
			progress.FormatCount(s.FilesDone), etaText(s)))
	}
	b.WriteString("\n" + dimStyle.Render("esc goes back and leaves this running · q stops it"))
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
	h := help.New()
	h.ShowAll = true
	h.Width = m.width - 4
	return "\n" + h.View(keys) + "\n\n" +
		dimStyle.Render("  1-4 or tab move between modes.")
}
