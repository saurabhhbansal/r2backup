package tui

import (
	"fmt"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/saurabhhbansal/r2backup/internal/scan"
)

// Pick shows the folder tree from scanned (already walked, root included, so
// this can never itself hang re-scanning a folder) and lets the user uncheck
// what they don't want backed up.
//
// This is the only prompt in the entire tool. Everything r2backup does after
// a folder is added -- scheduled runs, retries, conflict handling -- runs to
// completion without asking anything else, so this screen has to get the
// answer right the first time: "I get the whole work tree shown, and then I
// can maybe uncheck a few things ... apart from that, no second prompts."
//
// accepted is false whenever the user backs out (q, esc, ctrl+c) or the
// program itself errors; excludes is only meaningful when accepted is true.
// An immediate enter -- the expected common case -- yields an empty exclude
// list, because everything starts checked.
func Pick(root string, scanned *scan.Result) (excludes []string, accepted bool, err error) {
	m := NewModel(root, scanned)
	p := tea.NewProgram(m, tea.WithAltScreen())

	final, err := p.Run()
	if err != nil {
		return nil, false, fmt.Errorf("run picker: %w", err)
	}

	fm, ok := final.(*Model)
	if !ok {
		return nil, false, fmt.Errorf("tui: unexpected final model type %T", final)
	}
	if !fm.Accepted() {
		return nil, false, nil
	}
	return fm.Excludes(), true, nil
}
