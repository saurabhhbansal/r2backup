package ui

import (
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/list"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/saurabhhbansal/r2backup/internal/progress"
)

// setItem adapts a SetView to bubbles/list.
type setItem struct{ v SetView }

func (i setItem) FilterValue() string { return i.v.Name }

// setDelegate draws one row: name and state on the left, last run on the
// right, with the folder underneath.
//
// It is a custom delegate rather than list.DefaultDelegate because the right
// column has to be flush to the terminal's edge, and the default delegate has
// no concept of a right column. Everything else about the list -- paging,
// filtering, the cursor, the status bar -- is the component's.
type setDelegate struct{ width int }

func (d setDelegate) Height() int  { return 2 }
func (d setDelegate) Spacing() int { return 1 }

func (d setDelegate) Update(tea.Msg, *list.Model) tea.Cmd { return nil }

func (d setDelegate) Render(w io.Writer, m list.Model, index int, item list.Item) {
	it, ok := item.(setItem)
	if !ok {
		return
	}
	v := it.v
	selected := index == m.Index()

	cursor := "  "
	name := titleStyle.Render(v.Name)
	if selected {
		cursor = lipgloss.NewStyle().Foreground(accent).Render("▸ ")
		name = lipgloss.NewStyle().Foreground(accent).Bold(true).Render(v.Name)
	}

	left := cursor + name + "  " + stateStyle(v.State).Render(v.State)
	right := dimStyle.Render(lastRunText(v))

	width := d.width
	if width < 20 {
		width = 20
	}
	fmt.Fprintln(w, padBetween(left, right, width))
	fmt.Fprint(w, "  "+dimStyle.Render(truncate(v.Root, width-4)))
}

// lastRunText is the right-hand column: when it ran and what it did.
func lastRunText(v SetView) string {
	if !v.HasRun {
		return "not backed up yet"
	}
	when := humanAgo(time.Since(v.LastRun))
	if v.State == "failed" {
		return when + " · failed"
	}
	if v.State == "cancelled" {
		// Checked before the no-changes/uploaded branches below: a
		// cancelled run's counts are all zero (recordRun never gets to set
		// them), which without this would render as "no changes" -- true
		// of the numbers, but not of what happened.
		return when + " · cancelled"
	}
	if v.Uploaded == 0 && v.Deleted == 0 && v.Moved == 0 {
		return when + " · no changes"
	}
	return fmt.Sprintf("%s · %s uploaded", when, progress.FormatBytes(v.Bytes))
}

func humanAgo(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

// padBetween right-aligns right against left within width, keeping at least
// one space between them so a long folder name never runs into the numbers.
func padBetween(left, right string, width int) string {
	gap := width - lipgloss.Width(left) - lipgloss.Width(right)
	if gap < 1 {
		gap = 1
	}
	return left + strings.Repeat(" ", gap) + right
}

// truncate shortens s to width, keeping the tail of a path rather than the
// head: the last components of "/home/me/work/projects/thing" are the ones
// that identify it.
func truncate(s string, width int) string {
	if width < 4 || lipgloss.Width(s) <= width {
		return s
	}
	r := []rune(s)
	return "…" + string(r[len(r)-(width-1):])
}
