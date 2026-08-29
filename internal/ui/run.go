package ui

import (
	"context"
	"fmt"

	tea "github.com/charmbracelet/bubbletea"
)

// Run opens the interface and blocks until the user leaves it.
//
// The returned Action is a job the interface deliberately does not do itself
// -- adding a folder, changing what one includes, restoring one -- because
// each needs either the folder picker (a second full-screen program) or a
// line of typed input. The caller performs it and calls Run again. See the
// comment on Action.
func Run(ctx context.Context, b Backend) (Action, error) {
	m := New(ctx, b)
	p := tea.NewProgram(m, tea.WithAltScreen(), tea.WithContext(ctx))
	final, err := p.Run()
	if err != nil {
		return Action{}, fmt.Errorf("ui: %w", err)
	}
	fm, ok := final.(*Model)
	if !ok {
		return Action{}, fmt.Errorf("ui: unexpected final model type %T", final)
	}
	return fm.Action(), nil
}
