package ui

import "github.com/charmbracelet/lipgloss"

// Every colour here is an AdaptiveColor pair. A terminal's background is not
// knowable in advance and a fixed palette is unreadable on half of them --
// the same reason the picker in internal/tui has never used a bare colour.
var (
	accent  = lipgloss.AdaptiveColor{Light: "27", Dark: "39"}
	subtle  = lipgloss.AdaptiveColor{Light: "245", Dark: "241"}
	muted   = lipgloss.AdaptiveColor{Light: "240", Dark: "245"}
	good    = lipgloss.AdaptiveColor{Light: "28", Dark: "42"}
	warn    = lipgloss.AdaptiveColor{Light: "130", Dark: "214"}
	bad     = lipgloss.AdaptiveColor{Light: "160", Dark: "203"}
	heading = lipgloss.AdaptiveColor{Light: "235", Dark: "252"}
)

var (
	bannerStyle = lipgloss.NewStyle().Foreground(accent).Bold(true)

	titleStyle = lipgloss.NewStyle().Foreground(heading).Bold(true)
	dimStyle   = lipgloss.NewStyle().Foreground(muted)
	goodStyle  = lipgloss.NewStyle().Foreground(good)
	warnStyle  = lipgloss.NewStyle().Foreground(warn)
	badStyle   = lipgloss.NewStyle().Foreground(bad)

	// panelStyle frames a screen's body. The border is rounded and in the
	// subtle colour so it reads as a boundary and not as content.
	panelStyle = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(subtle).
			Padding(0, 1)

	labelStyle = lipgloss.NewStyle().Foreground(muted).Width(14)

	statusStyle = lipgloss.NewStyle().Foreground(muted)
	errorStyle  = lipgloss.NewStyle().Foreground(bad).Bold(true)
)

// stateStyle picks the colour for a set's one-word state.
func stateStyle(s string) lipgloss.Style {
	switch s {
	case "ok":
		return goodStyle
	case "needs attention", "failed":
		return badStyle
	case "never run":
		return dimStyle
	default:
		return warnStyle
	}
}

// Tab styling.
//
// No borders. A bordered lipgloss style renders as several lines, and the bar
// is built by placing tabs side by side -- so a bottom border turned every tab
// into its own stacked block with the rules misaligned underneath. The active
// mode is filled instead, which also survives a terminal with a small palette
// better than a colour difference alone.
var (
	tabStyle = lipgloss.NewStyle().Foreground(muted).Padding(0, 1)

	activeTabStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("0")).
			Background(accent).
			Bold(true).
			Padding(0, 1)

	tabGapStyle = lipgloss.NewStyle().Foreground(subtle)
)
