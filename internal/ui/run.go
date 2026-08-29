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
	if _, err := p.Run(); err != nil {
		return fmt.Errorf("ui: %w", err)
	}
	return nil
}
