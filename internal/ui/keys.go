package ui

import "github.com/charmbracelet/bubbles/key"

// keyMap is the whole keyboard surface, declared once.
//
// bubbles/key rather than a switch on strings: a binding carries its own help
// text, so bubbles/help renders the footer straight from these values and the
// hint the user reads cannot drift away from the key that is actually bound.
// A footer that lies about a shortcut is worse than no footer.
type keyMap struct {
	Up       key.Binding
	Down     key.Binding
	Enter    key.Binding
	Back     key.Binding
	Backup   key.Binding
	All      key.Binding
	Add      key.Binding
	Edit     key.Binding
	Restore  key.Binding
	Remove   key.Binding
	Trash    key.Binding
	Schedule key.Binding
	Refresh  key.Binding
	Help     key.Binding
	Quit     key.Binding
}

var keys = keyMap{
	Up:       key.NewBinding(key.WithKeys("up", "k"), key.WithHelp("↑/k", "up")),
	Down:     key.NewBinding(key.WithKeys("down", "j"), key.WithHelp("↓/j", "down")),
	Enter:    key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "details")),
	Back:     key.NewBinding(key.WithKeys("esc"), key.WithHelp("esc", "back")),
	Backup:   key.NewBinding(key.WithKeys("b"), key.WithHelp("b", "back up this one")),
	All:      key.NewBinding(key.WithKeys("B"), key.WithHelp("B", "back up everything")),
	Add:      key.NewBinding(key.WithKeys("a"), key.WithHelp("a", "add a folder")),
	Edit:     key.NewBinding(key.WithKeys("e"), key.WithHelp("e", "change what is included")),
	Restore:  key.NewBinding(key.WithKeys("r"), key.WithHelp("r", "restore")),
	Remove:   key.NewBinding(key.WithKeys("x"), key.WithHelp("x", "stop backing up")),
	Trash:    key.NewBinding(key.WithKeys("t"), key.WithHelp("t", "trash")),
	Schedule: key.NewBinding(key.WithKeys("s"), key.WithHelp("s", "schedule")),
	Refresh:  key.NewBinding(key.WithKeys("R"), key.WithHelp("R", "refresh")),
	Help:     key.NewBinding(key.WithKeys("?"), key.WithHelp("?", "help")),
	Quit:     key.NewBinding(key.WithKeys("q", "ctrl+c"), key.WithHelp("q", "quit")),
}

// ShortHelp and FullHelp satisfy help.KeyMap.
//
// bubbles/help truncates the short line to the width it is given, tail first,
// so what goes last is what a narrow terminal loses. "q quit" is therefore
// never last by accident: see Model.shortHelp, which drops bindings from the
// middle rather than letting the way out fall off the end.
func (k keyMap) ShortHelp() []key.Binding {
	return []key.Binding{k.Up, k.Down, k.Enter, k.Backup, k.Help, k.Quit}
}

func (k keyMap) FullHelp() [][]key.Binding {
	return [][]key.Binding{
		{k.Up, k.Down, k.Enter, k.Back},
		{k.Backup, k.All, k.Add, k.Edit},
		{k.Restore, k.Remove, k.Trash, k.Schedule},
		{k.Refresh, k.Help, k.Quit},
	}
}
