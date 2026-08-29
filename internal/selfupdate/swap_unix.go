//go:build !windows

package selfupdate

import (
	"fmt"
	"os"
)

// swap replaces self with staged.
//
// A running binary on Unix can be unlinked: the kernel keeps the inode alive
// for the process that has it mapped, and rename(2) replaces the directory
// entry atomically. So there is nothing left over and no cleanup to do.
func swap(self, staged string) error {
	if err := os.Rename(staged, self); err != nil {
		return fmt.Errorf("replace %q: %w", self, err)
	}
	return nil
}
