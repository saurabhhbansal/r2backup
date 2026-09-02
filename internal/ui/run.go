package ui

import (
	"context"
	"fmt"

	tea "github.com/charmbracelet/bubbletea"
)

// Run opens the interface and blocks until the user leaves it.
//
// It returns nothing but an error now. The first version handed jobs back to
// the command line -- adding a folder, changing what one includes, restoring
// one -- because each needed the folder picker or a line of typed input, and
// it closed itself to let a command do them. That was the interface telling
// the user to go and use the thing it exists to replace. The picker is a
// child model here and the typed input is a form, so there is nothing left to
// hand back.
func Run(ctx context.Context, b Backend) error {
	m := New(ctx, b)
	p := tea.NewProgram(m, tea.WithAltScreen(), tea.WithContext(ctx))
	_, runErr := p.Run()
	// p.Run returning is not the same as a running backup or restore having
	// stopped: quitting cancels its context but does not wait for it to
	// notice, and neither does the outer ctx being cancelled out from under
	// the whole program. The caller opened whatever the backend touches --
	// here, the on-disk index -- and is about to close it the moment this
	// function returns, so that must not happen while that goroutine could
	// still be mid-write to it. See Model.WaitBackground.
	m.WaitBackground()
	if runErr != nil {
		return fmt.Errorf("ui: %w", runErr)
	}
	return nil
}
