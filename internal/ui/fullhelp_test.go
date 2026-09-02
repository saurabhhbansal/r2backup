package ui

import (
	"strings"
	"testing"
)

// TestFullHelpShowsEveryKeyAtRealisticWidths is the test that would have
// caught finding M3: bubbles/help lays FullHelp's columns out side by side
// and drops whichever ones do not fit, replacing them with an ellipsis. At
// 120 columns the old seven-column grouping lost its last two columns --
// Watch/Update/Refresh/Help/Quit among them, meaning the overlay opened with
// "?" could not tell you how to close it. This renders the same overlay the
// "?" key opens, through the same path (a real key press and the model's
// real View()), at 80 and 120 columns, and checks that every key FullHelp
// lists is actually visible and that bubbles/help never had to reach for its
// ellipsis.
func TestFullHelpShowsEveryKeyAtRealisticWidths(t *testing.T) {
	for _, width := range []int{80, 120} {
		m := sized(twoSets(), width, 40)
		press(m, "?")
		if m.overlay != overlayHelp {
			t.Fatalf("%d cols: \"?\" should open the help overlay", width)
		}
		got := m.View()

		if strings.Contains(got, "…") {
			t.Errorf("%d cols: help view was truncated with an ellipsis:\n%s", width, got)
		}

		// BoundKeys walks keys.FullHelp() the same way the coverage test in
		// internal/cli does, so this is exactly the set that regrouping
		// FullHelp must not be allowed to quietly drop from view.
		for _, row := range keys.FullHelp() {
			for _, b := range row {
				help := b.Help()
				if !strings.Contains(got, help.Key) {
					t.Errorf("%d cols: key %q (%s) missing from the help overlay:\n%s", width, help.Key, help.Desc, got)
				}
				if !strings.Contains(got, help.Desc) {
					t.Errorf("%d cols: description %q for key %q missing from the help overlay:\n%s", width, help.Desc, help.Key, got)
				}
			}
		}
	}
}
