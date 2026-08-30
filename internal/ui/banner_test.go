package ui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
)

// widestRenderedLine and renderedHeight measure a rendered banner the same
// way a terminal would, independently of anything in banner.go -- they do
// not call widestLine or read variant.height, because a test that reuses the
// code it is checking cannot catch a bug in that code.
func widestRenderedLine(s string) int {
	w := 0
	for _, line := range strings.Split(s, "\n") {
		if n := lipgloss.Width(line); n > w {
			w = n
		}
	}
	return w
}

func renderedHeight(s string) int {
	return len(strings.Split(s, "\n"))
}

func isArt(s string) bool {
	return strings.Contains(s, "#")
}

// TestBannerFitsEveryRealisticSize is the ladder's central promise: at every
// size in this table, whatever Banner returns fits inside the terminal, and
// leaves the body enough rows to still be a usable list rather than a
// sliver under a logo. bodyRows mirrors the arithmetic in view.go's
// chromeHeight/layout -- the tab bar and "where" line (2), the panel's own
// top and bottom border (2), and the footer (2) -- so a regression that
// shrinks the list below three rows shows up here, not just as a visual bug
// a person would have to notice.
func TestBannerFitsEveryRealisticSize(t *testing.T) {
	const fixedChrome = 2 /* tab bar + where line */ + 2 /* panel border */ + 2 /* footer */

	cases := []struct {
		w, h int
		// wantArt is nil when either size is plausible (the table is not
		// trying to pin down the exact threshold everywhere); set it true or
		// false where the outcome matters enough to assert on.
		wantArt *bool
	}{
		{80, 24, boolPtr(true)},
		{80, 40, boolPtr(true)},
		{100, 30, boolPtr(true)},
		{120, 40, boolPtr(true)},
		{132, 43, boolPtr(true)},
		{160, 50, boolPtr(true)},
		{200, 60, boolPtr(true)},
		{60, 20, boolPtr(true)},
		{40, 12, nil},
	}

	for _, c := range cases {
		out := Banner(c.w, c.h)
		if w := widestRenderedLine(out); w > c.w {
			t.Errorf("%dx%d: banner is %d columns wide, wider than the terminal\n%s", c.w, c.h, w, out)
		}

		bodyRows := c.h - renderedHeight(out) - fixedChrome
		if bodyRows < 3 {
			t.Errorf("%dx%d: only %d body rows would be left for the list (banner is %d rows)",
				c.w, c.h, bodyRows, renderedHeight(out))
		}

		if c.wantArt != nil {
			got := isArt(out)
			if got != *c.wantArt {
				t.Errorf("%dx%d: got art=%v, want %v\n%s", c.w, c.h, got, *c.wantArt, out)
			}
		}
	}
}

func boolPtr(b bool) *bool { return &b }

// TestBannerFallsBackOnlyWhenGenuinelyTiny checks the other end: the ladder
// has a rung for nearly everything, so the bare word should be rare, not the
// common case a real 80-column terminal used to hit.
func TestBannerFallsBackOnlyWhenGenuinelyTiny(t *testing.T) {
	roomy := [][2]int{{80, 24}, {60, 20}, {100, 40}, {200, 60}}
	for _, size := range roomy {
		if out := Banner(size[0], size[1]); !isArt(out) {
			t.Errorf("%dx%d is roomy enough for some rung of the ladder, got the fallback word", size[0], size[1])
		}
	}

	tiny := [][2]int{{10, 6}, {20, 5}, {15, 40}, {200, 5}}
	for _, size := range tiny {
		out := Banner(size[0], size[1])
		if isArt(out) {
			t.Errorf("%dx%d should be too small for even the smallest rung, got art:\n%s", size[0], size[1], out)
		}
		if !strings.Contains(out, "r2backup") {
			t.Errorf("%dx%d: the fallback should still say what this is", size[0], size[1])
		}
	}
}

// TestLadderRungsShrinkTogether guards the assumption Banner's selection
// loop depends on: walking the ladder from largest to smallest and returning
// the first rung that fits only finds the *largest* fit if width, height and
// minListRows all decrease together. If a future edit widened a smaller rung
// past a bigger one, or gave a small rung a larger minListRows than the rung
// above it, the loop would still return "a" fit -- just not necessarily the
// best one -- and nothing else would notice.
//
// It also re-derives width and height from the embedded text directly,
// rather than trusting variant.width/height, so a bug in newVariant's own
// measurement cannot hide from it.
func TestLadderRungsShrinkTogether(t *testing.T) {
	if len(ladder) < 2 {
		t.Fatal("a ladder of one rung is not a ladder")
	}
	for i, v := range ladder {
		lines := strings.Split(v.art, "\n")
		w, h := 0, len(lines)
		for _, line := range lines {
			if n := lipgloss.Width(line); n > w {
				w = n
			}
		}
		if w != v.width {
			t.Errorf("rung %q: widestLine measured %d, variant.width says %d", v.name, w, v.width)
		}
		if h != v.height {
			t.Errorf("rung %q: measured %d rows, variant.height says %d", v.name, h, v.height)
		}
		if strings.TrimRight(v.art, "\n") != v.art {
			t.Errorf("rung %q: art still has a trailing newline after newVariant should have trimmed it", v.name)
		}

		if i == 0 {
			continue
		}
		prev := ladder[i-1]
		if v.width >= prev.width {
			t.Errorf("rung %q (%d wide) is not narrower than %q (%d wide) above it",
				v.name, v.width, prev.name, prev.width)
		}
		if v.height >= prev.height {
			t.Errorf("rung %q (%d rows) is not shorter than %q (%d rows) above it",
				v.name, v.height, prev.name, prev.height)
		}
		if v.height+v.minListRows >= prev.height+prev.minListRows {
			t.Errorf("rung %q needs %d total rows, not fewer than %q's %d -- the largest-fit loop in Banner would stop too early",
				v.name, v.height+v.minListRows, prev.name, prev.height+prev.minListRows)
		}
	}
}

// TestEveryVariantUsesOnlyHashAndText guards the character vocabulary. Every
// rung down to S is drawn from '#' and space, matching the original
// hand-drawn art; XS is the one deliberate exception described in
// banner.go's package comment, where the wordmark is set as plain text next
// to a small '#' badge because there is not enough room left to draw block
// letters that would still read as letters. This test would fail loudly if
// a future edit introduced stray control characters or non-ASCII art into
// any rung, hash-based or not.
func TestEveryVariantUsesOnlyHashAndText(t *testing.T) {
	for _, v := range ladder {
		for _, r := range v.art {
			switch {
			case r == '#' || r == ' ' || r == '\n':
			case v.name == "xs" && (r >= 'a' && r <= 'z' || r >= '0' && r <= '9'):
				// XS spells the word out as plain text; see banner.go.
			default:
				t.Errorf("rung %q contains unexpected character %q", v.name, r)
			}
		}
	}
}

// TestHeaderShowsABannerOnEveryOverlayFreeTab is the integration half of the
// ladder: header() used to call Banner only on the Folders tab because
// nineteen rows was too much to spend on every screen. With a ladder that
// reason is gone, so every tab without an overlay open should show some
// rung once the terminal has room for one -- this is what actually answers
// "the banner only shows up on the folders page" from the original report.
func TestHeaderShowsABannerOnEveryOverlayFreeTab(t *testing.T) {
	m := sized(twoSets(), 150, 44)
	for tb := tab(0); tb < numTabs; tb++ {
		m.tab, m.overlay = tb, overlayNone
		m.layout()
		if h := m.header(); !isArt(h) {
			t.Errorf("tab %v at 150x44 should show a banner rung, got the plain word:\n%s", tb, h)
		}
	}
}

// TestOverlaysKeepThePlainWord is the other half: an overlay is a focused,
// momentary screen, and header() deliberately keeps it out of the ladder's
// way regardless of how much room there is.
func TestOverlaysKeepThePlainWord(t *testing.T) {
	m := sized(twoSets(), 150, 44)
	m.tab = tabFolders
	m.overlay = overlayHelp
	m.layout()
	if h := m.header(); isArt(h) {
		t.Errorf("an overlay should not show ladder art:\n%s", h)
	}
	if !strings.Contains(m.header(), "r2backup") {
		t.Error("an overlay's header should still say what this is")
	}
}

// TestBannerLeavesBreathingRoomOnlyWhenThereIsRoomToSpare checks the blank
// line described in Banner's own comment: it separates the art from
// whatever is drawn under it, but only when the terminal has a row beyond
// what the chosen rung strictly needs -- an exact fit must not lose a line
// of the list just to add whitespace above the tab bar, and the bare-word
// fallback (which is not art) never gets one at all.
func TestBannerLeavesBreathingRoomOnlyWhenThereIsRoomToSpare(t *testing.T) {
	// M is the second-smallest art rung with room to spare in a plausible
	// terminal; give it exactly what it needs and then one extra row.
	var m variant
	for _, v := range ladder {
		if v.name == "m" {
			m = v
		}
	}
	if m.name == "" {
		t.Fatal("no rung named \"m\" in the ladder")
	}

	exact := Banner(m.width, m.height+m.minListRows)
	if renderedHeight(exact) != m.height {
		t.Errorf("an exact fit should not add a blank line: got %d rows, want %d\n%s",
			renderedHeight(exact), m.height, exact)
	}

	spare := Banner(m.width, m.height+m.minListRows+1)
	if renderedHeight(spare) != m.height+1 {
		t.Errorf("a spare row should be spent on a blank line: got %d rows, want %d\n%s",
			renderedHeight(spare), m.height+1, spare)
	}

	fallback := Banner(1, 1)
	if strings.Contains(fallback, "\n") {
		t.Errorf("the fallback word should never grow a blank line:\n%q", fallback)
	}
}
