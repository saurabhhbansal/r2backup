// Package ui is the full-screen interface r2b opens when it is run with no
// arguments.
//
// Every command still exists and still works on its own -- the scheduler
// invokes `r2b backup` and nothing else, and scripts are unaffected. This is
// the front door for a person: it shows what is being backed up, what
// happened last time, and what is recoverable, and it runs a backup without
// anybody having to remember a subcommand.
//
// It is assembled from the standard Charm components -- bubbles/list,
// bubbles/table, bubbles/progress, bubbles/spinner, bubbles/help and
// bubbles/key -- rather than hand-rolled. Scrolling, paging, filtering,
// keyboard help and cursor behaviour are the parts of a TUI that are tedious
// to write and easy to get subtly wrong, and they are exactly the parts those
// components already do correctly.
package ui

import (
	_ "embed"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

//go:embed banner.txt
var bannerArt string

// bannerWidth is the widest line of the art. Measured once at startup rather
// than assumed, so editing banner.txt cannot quietly break the fallback.
var bannerWidth = widestLine(bannerArt)

// bannerHeight is how many rows the art occupies.
var bannerHeight = len(strings.Split(strings.TrimRight(bannerArt, "\n"), "\n"))

func widestLine(s string) int {
	w := 0
	for _, line := range strings.Split(s, "\n") {
		if n := lipgloss.Width(line); n > w {
			w = n
		}
	}
	return w
}

// minListRows is how much room the rest of the screen needs before the art is
// worth its nineteen rows: the header line, the panel border, the footer, and
// enough of the list left over to be a list.
const minListRows = 16

// Banner renders the wordmark for a terminal of the given width.
//
// It has three forms because one does not fit everywhere. The art is 139
// columns wide, which is wider than a default 80-column terminal and wider
// than most split panes; printing it anyway would wrap every line in the
// middle and turn the logo into noise. So a narrow terminal gets a plain
// title instead, and a very short one gets nothing at all -- on an 8-row
// window the banner would be the entire screen and the list it is supposed to
// introduce would be off the bottom.
func Banner(width, height int) string {
	switch {
	case height < bannerHeight+minListRows || width < bannerWidth:
		return bannerStyle.Render("r2backup")
	default:
		return bannerStyle.Render(strings.TrimRight(bannerArt, "\n"))
	}
}
