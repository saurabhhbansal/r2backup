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

	// panelStyle holds a screen's body. No border: the rule under the tabs
	// and the one over the status bar already mark where it starts and
	// ends, and a box inside them was a second frame saying the same thing.
	// The padding takes the same two rows and four columns the border and
	// its padding used to, so every size computed from them still holds.
	panelStyle = lipgloss.NewStyle().Padding(1, 2)

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
	case "cancelled":
		// Not badStyle: a run someone stopped on purpose is not a problem
		// the way a failure or a set needing attention is.
		return warnStyle
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
//
// The active tab is marked the way a desktop app marks one: in the accent
// colour, with a heavier rule underneath it (see tabBar), rather than a
// filled block. The digit stays dim so the names read first.
var (
	tabStyle       = lipgloss.NewStyle().Foreground(muted).Padding(0, 1)
	activeTabStyle = lipgloss.NewStyle().Foreground(accent).Bold(true).Padding(0, 1)
	tabDigitStyle  = lipgloss.NewStyle().Foreground(subtle)
	tabGapStyle    = lipgloss.NewStyle().Foreground(subtle)
	ruleStyle      = lipgloss.NewStyle().Foreground(subtle)
	activeRule     = lipgloss.NewStyle().Foreground(accent)

	wordmarkStyle = lipgloss.NewStyle().Foreground(accent).Bold(true)
)

// Glyphs for a folder's state. One shape per meaning, so the list can be
// read without colour.
const (
	iconOK      = "✓"
	iconBad     = "!"
	iconNever   = "○"
	iconStopped = "◌"
)

// stateIcon is the glyph, in its colour, for a set's state.
func stateIcon(s string) string {
	switch s {
	case "ok":
		return goodStyle.Render(iconOK)
	case "needs attention", "failed":
		return badStyle.Bold(true).Render(iconBad)
	case "never run":
		return dimStyle.Render(iconNever)
	default:
		return warnStyle.Render(iconStopped)
	}
}
