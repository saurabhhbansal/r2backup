package ui

import "github.com/charmbracelet/bubbles/key"

// keyMap is the whole keyboard surface, declared once.
//
// bubbles/key rather than a switch on strings: a binding carries its own help
// text, so bubbles/help renders the footer straight from these values and the
// hint the user reads cannot drift from the key that is actually bound.
type keyMap struct {
	Up      key.Binding
	Down    key.Binding
	Enter   key.Binding
	Back    key.Binding
	NextTab key.Binding
	PrevTab key.Binding
	Backup  key.Binding
	All     key.Binding
	Add     key.Binding
	Edit    key.Binding
	Restore key.Binding
	Recover key.Binding
	Remove  key.Binding
	Rename  key.Binding
	Relink  key.Binding
	Toggle  key.Binding
	Every   key.Binding
	SignIn  key.Binding
	SignOut key.Binding
	Share   key.Binding
	Keys    key.Binding
	Update  key.Binding
	Refresh key.Binding
	Help    key.Binding
	Quit    key.Binding
}

var keys = keyMap{
	Up:      key.NewBinding(key.WithKeys("up", "k"), key.WithHelp("↑/k", "up")),
	Down:    key.NewBinding(key.WithKeys("down", "j"), key.WithHelp("↓/j", "down")),
	Enter:   key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "open")),
	Back:    key.NewBinding(key.WithKeys("esc"), key.WithHelp("esc", "back")),
	NextTab: key.NewBinding(key.WithKeys("tab", "right", "l"), key.WithHelp("tab", "next mode")),
	PrevTab: key.NewBinding(key.WithKeys("shift+tab", "left", "h"), key.WithHelp("shift+tab", "previous mode")),

	Backup:  key.NewBinding(key.WithKeys("b"), key.WithHelp("b", "back up this folder")),
	All:     key.NewBinding(key.WithKeys("B"), key.WithHelp("B", "back up everything")),
	Add:     key.NewBinding(key.WithKeys("a"), key.WithHelp("a", "add a folder")),
	Edit:    key.NewBinding(key.WithKeys("e"), key.WithHelp("e", "change what is included")),
	Restore: key.NewBinding(key.WithKeys("r"), key.WithHelp("r", "restore")),
	Recover: key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "recover this file")),
	Remove:  key.NewBinding(key.WithKeys("x"), key.WithHelp("x", "stop backing up")),
	Rename:  key.NewBinding(key.WithKeys("n"), key.WithHelp("n", "rename")),
	Relink:  key.NewBinding(key.WithKeys("m"), key.WithHelp("m", "folder moved")),

	Toggle: key.NewBinding(key.WithKeys("s"), key.WithHelp("s", "turn automatic backups on/off")),
	Every:  key.NewBinding(key.WithKeys("e"), key.WithHelp("e", "change how often")),

	SignIn:  key.NewBinding(key.WithKeys("i"), key.WithHelp("i", "sign in")),
	SignOut: key.NewBinding(key.WithKeys("o"), key.WithHelp("o", "sign out")),
	Share:   key.NewBinding(key.WithKeys("p"), key.WithHelp("p", "save keys for other computers")),
	Keys:    key.NewBinding(key.WithKeys("k"), key.WithHelp("k", "enter R2 keys")),
	Update:  key.NewBinding(key.WithKeys("u"), key.WithHelp("u", "update")),

	Refresh: key.NewBinding(key.WithKeys("R"), key.WithHelp("R", "refresh")),
	Help:    key.NewBinding(key.WithKeys("?"), key.WithHelp("?", "keys")),
	Quit:    key.NewBinding(key.WithKeys("q", "ctrl+c"), key.WithHelp("q", "quit")),
}

// tabKeys is the per-mode help, so the footer only ever offers what the mode
// in front of you actually does.
func tabKeys(t tab) []key.Binding {
	switch t {
	case tabFolders:
		return []key.Binding{keys.Add, keys.Backup, keys.Restore, keys.Edit, keys.Remove}
	case tabSchedule:
		return []key.Binding{keys.Toggle, keys.Every}
	case tabTrash:
		return []key.Binding{keys.Recover}
	case tabAccount:
		return []key.Binding{keys.SignIn, keys.Share, keys.Keys, keys.SignOut, keys.Update}
	}
	return nil
}

// ShortHelp and FullHelp satisfy help.KeyMap.
//
// bubbles/help truncates the short line tail-first, so what goes last is what
// a narrow terminal loses. "q quit" is never last by accident: see
// Model.shortHelp, which drops from the middle rather than letting the way
// out fall off the end.
func (k keyMap) ShortHelp() []key.Binding {
	return []key.Binding{k.NextTab, k.Enter, k.Help, k.Quit}
}

func (k keyMap) FullHelp() [][]key.Binding {
	return [][]key.Binding{
		{k.Up, k.Down, k.Enter, k.Back, k.NextTab},
		{k.Add, k.Backup, k.All, k.Edit, k.Restore},
		{k.Rename, k.Relink, k.Remove},
		{k.Toggle, k.Every},
		{k.SignIn, k.Share, k.Keys, k.SignOut},
		{k.Update, k.Refresh, k.Help, k.Quit},
	}
}
