package ui

import (
	"strings"
	"testing"
)

// x and X (Remove and Purge) must be refused the same way b, B, a and r
// already are: before the confirmation form opens, not after it. Removing a
// set out from under a running backup drops the index bucket that
// backup.Run is still filling in as it goes -- see startsWork's comment --
// so this deserves exactly the busy guard that already protects Add,
// Backup, All and Restore, and it must fire early enough that no question
// gets asked about a folder that was never going anywhere.
func TestRemoveAndPurgeAreRefusedWhileARunIsInProgress(t *testing.T) {
	for _, key := range []string{"x", "X"} {
		b := twoSets()
		m := sized(b, 120, 40)
		press(m, "b")
		if !m.running {
			t.Fatalf("b should have started a backup (setting up %q)", key)
		}
		// Back to the tabs with the run still going, exactly the way
		// TestASecondJobCannotStartOnTopOfARunningOne checks the guard for
		// a, B and r.
		press(m, "esc")

		press(m, key)
		if m.overlay == overlayConfirm {
			t.Fatalf("%s opened the confirmation form while a backup was running", key)
		}
		if !strings.Contains(m.notice, "already running") {
			t.Errorf("%s: the user should be told why nothing happened, got %q", key, m.notice)
		}
		if len(b.removed) != 0 {
			t.Errorf("%s removed a set while a backup was running", key)
		}
	}
}
