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

// The mark is drawn at five sizes rather than one. A single 139-column,
// 19-row piece of art only fits a maximised terminal -- everything smaller
// used to fall straight through to the bare word "r2backup", which is what
// happened on nearly every real window.
//
// Every rung is the same logo: the stacked cube from assets/logo.png with
// the shell prompt knocked out of it, set beside the wordmark. None of them
// is a mechanical shrink of the one above. Sampling a 139-column drawing
// down to 50 destroys exactly the parts that carry the likeness -- the
// counters inside the letters, the prompt's negative space, the gaps
// between the cube's layers -- and what survives is speckle, so each rung
// was drawn at the size it is actually shown at and then read back line by
// line. Two details fall away as the mark shrinks, deliberately: below
// twenty columns the prompt's underscore lands on the hexagon's bottom
// taper and reads as a nick in the edge rather than as part of the mark, so
// the narrow rungs carry the chevron alone; and at three rows there is no
// mark left to draw, so the smallest rung shows the silhouette beside the
// name set as plain text. That is still a mark beside a name, which is the
// thing the fallback word alone is missing.
//
//go:embed banner.txt
var bannerArtXL string

//go:embed banner_l.txt
var bannerArtL string

//go:embed banner_m.txt
var bannerArtM string

//go:embed banner_s.txt
var bannerArtS string

//go:embed banner_xs.txt
var bannerArtXS string

// variant is one rung of the banner ladder.
type variant struct {
	name          string // for test failure messages, not shown to a user
	art           string // trimmed of its own trailing newline
	width, height int

	// minListRows is the same idea the single-size banner used to encode in
	// one constant: how much room the rest of the screen needs on top of the
	// art itself before that art is worth showing -- the header's other two
	// lines (the tab bar and the "where" line), the panel's own top and
	// bottom border, the footer, and enough of the list left over to still
	// read as a list rather than a sliver. A taller rung needs a bigger
	// number here for the same reason the original nineteen-row art did: a
	// five-row mark costs the rest of the screen almost nothing, so it can
	// afford to show up in far tighter quarters than the big one.
	minListRows int
}

func widestLine(s string) int {
	w := 0
	for _, line := range strings.Split(s, "\n") {
		if n := lipgloss.Width(line); n > w {
			w = n
		}
	}
	return w
}

func newVariant(name, art string, minListRows int) variant {
	art = strings.TrimRight(art, "\n")
	return variant{
		name:        name,
		art:         art,
		width:       widestLine(art),
		height:      len(strings.Split(art, "\n")),
		minListRows: minListRows,
	}
}

// ladder is every rung, largest first. Banner walks it in this order and
// takes the first one that fits, so it depends on both columns -- width and
// height + minListRows -- shrinking together from top to bottom. They do:
// each rung here is smaller than the one above it in every dimension, which
// is also asserted in banner_test.go so a future edit cannot quietly break
// the ordering this loop relies on.
var ladder = []variant{
	newVariant("xl", bannerArtXL, 16),
	newVariant("l", bannerArtL, 14),
	newVariant("m", bannerArtM, 12),
	newVariant("s", bannerArtS, 11),
	newVariant("xs", bannerArtXS, 9),
}

// Banner renders the mark for a terminal of the given size, choosing the
// largest rung of the ladder above that fits both dimensions and falling
// back to the plain styled word only when even the smallest rung does not.
//
// "Fits" means two things at once: the rung's widest line is no wider than
// the terminal, and the terminal is tall enough to give the art its rows
// *and* leave minListRows behind for the header's other lines, the panel
// border, the footer and a usable list. Checking only width is how the old,
// single-size banner ended up printing over the list on a wide-but-short
// window (a maximised terminal at a small font, or a short split pane) --
// it had the columns but not the rows, and the last several lines of the
// list scrolled off under the art with nothing on screen saying why.
//
// When a rung is shown and the terminal has at least one row more than that
// rung strictly needs, a blank line is appended after the art. That is the
// breathing room between the mark and whatever is drawn under it -- on the
// original art it did not matter because thirty-five rows of the "you
// clearly have a big terminal" budget covered it by accident, but the small
// rungs are chosen specifically for terminals that do not have a row to
// spare, so the gap is only spent when there genuinely is one. It is never
// added to the bare-word fallback: that one line is not art, and giving it
// a blank line under it would just be one more row taken from the list for
// no visual gain.
func Banner(width, height int) string {
	for _, v := range ladder {
		if width < v.width || height < v.height+v.minListRows {
			continue
		}
		art := v.art
		if height > v.height+v.minListRows {
			art += "\n"
		}
		return bannerStyle.Render(art)
	}
	return bannerStyle.Render("r2backup")
}
