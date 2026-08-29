//go:build windows

package selfupdate

import (
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

// swap replaces self with staged.
//
// Windows will not let a running executable be overwritten, but it will let it
// be renamed -- the file handle follows the file, not the name. So: move the
// running binary aside, move the new one into place, and arrange for the old
// one to disappear.
//
// Two independent things remove it, because neither alone is sufficient. The
// next run calls Cleanup, which is at most thirty minutes away given the
// scheduler. And MoveFileEx with MOVEFILE_DELAY_UNTIL_REBOOT tells Windows to
// delete it during boot, which covers the case where r2backup is never run
// again. There is at most one such file, ever, because the name is fixed.
func swap(self, staged string) error {
	old := self + OldSuffix
	// A leftover from a previous update would block the rename below.
	_ = os.Remove(old)

	if err := os.Rename(self, old); err != nil {
		return fmt.Errorf("move the running binary aside to %q: %w", old, err)
	}
	if err := os.Rename(staged, self); err != nil {
		// Put it back: better to still be running the old version than to
		// have no binary at all.
		if restoreErr := os.Rename(old, self); restoreErr != nil {
			return fmt.Errorf("install %q failed (%w) AND restoring the previous binary failed (%v); the previous version is at %q",
				self, err, restoreErr, old)
		}
		return fmt.Errorf("install the new binary at %q: %w", self, err)
	}
	scheduleDeleteAtReboot(old)
	return nil
}

// scheduleDeleteAtReboot asks Windows to remove path during the next boot. It
// is a backstop for a machine where r2backup is never launched again, so a
// failure is not worth reporting to the user.
func scheduleDeleteAtReboot(path string) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return
	}
	_ = windows.MoveFileEx(p, nil, windows.MOVEFILE_DELAY_UNTIL_REBOOT)
}
