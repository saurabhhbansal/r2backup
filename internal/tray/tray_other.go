//go:build !windows

package tray

import (
	"context"
	"errors"
)

// Run is Windows-only for now. macOS and Linux have r2b's terminal
// interface, and their own notification areas differ enough that each would
// be its own piece of work.
func Run(ctx context.Context, be Backend) error {
	return errors.New("the taskbar app is Windows-only for now; run r2b for the interface")
}

// Fail does nothing here: the command prints Run's error, and there is no
// taskbar app to have failed.
func Fail(error) {}
