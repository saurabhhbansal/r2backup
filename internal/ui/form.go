package ui

import (
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// field is one question in a form.
type field struct {
	Label       string
	Placeholder string
	Secret      bool
	// Optional fields may be left blank; the form will not stop on them.
	Optional bool
	input    textinput.Model
}

// form is a short sequence of questions asked on one screen.
//
// It is built on bubbles/textinput rather than a forms library because what
// is needed here is four labelled lines and a submit -- the cursor,
// selection, paste handling and password masking are the parts that are
// tedious and easy to get wrong, and textinput already has all of them.
//
// Every prompt the command line used to ask -- an email address, a six-digit
// code, a passphrase, a destination folder, four R2 keys -- is one of these
// now, so none of them is a reason to leave the interface.
type form struct {
	title  string
	hint   string
	fields []field
	focus  int
	err    string
	// submit is what the caller does with the answers. It returns an error
	// message to show in place, or "" and a command to run on success.
	submit func(values []string) (string, tea.Cmd)
}

func newForm(title, hint string, fields []field, submit func([]string) (string, tea.Cmd)) *form {
	f := &form{title: title, hint: hint, fields: fields, submit: submit}
	for i := range f.fields {
		in := textinput.New()
		in.Placeholder = f.fields[i].Placeholder
		in.Prompt = ""
		in.CharLimit = 512
		if f.fields[i].Secret {
			in.EchoMode = textinput.EchoPassword
			in.EchoCharacter = '•'
		}
		f.fields[i].input = in
	}
	f.focusField(0)
	return f
}

func (f *form) focusField(i int) tea.Cmd {
	for j := range f.fields {
		f.fields[j].input.Blur()
	}
	if i < 0 {
		i = 0
	}
	if i > len(f.fields)-1 {
		i = len(f.fields) - 1
	}
	f.focus = i
	return f.fields[i].input.Focus()
}

func (f *form) values() []string {
	out := make([]string, len(f.fields))
	for i := range f.fields {
		out[i] = strings.TrimSpace(f.fields[i].input.Value())
	}
	return out
}

// SetValue prefills a field, for a form opened on something that already has
// an answer -- renaming a set starts on its current name.
func (f *form) SetValue(i int, v string) {
	if i >= 0 && i < len(f.fields) {
		f.fields[i].input.SetValue(v)
		f.fields[i].input.CursorEnd()
	}
}

// Update handles one key. It returns done=true when the form has been
// submitted successfully and the caller should move on.
func (f *form) Update(msg tea.Msg) (cmd tea.Cmd, done bool) {
	if key, ok := msg.(tea.KeyMsg); ok {
		switch key.Type {
		case tea.KeyTab, tea.KeyDown:
			return f.focusField(f.focus + 1), false
		case tea.KeyShiftTab, tea.KeyUp:
			return f.focusField(f.focus - 1), false
		case tea.KeyEnter:
			// Enter on any field but the last moves on rather than
			// submitting a half-filled form.
			if f.focus < len(f.fields)-1 {
				return f.focusField(f.focus + 1), false
			}
			for i, v := range f.values() {
				if v == "" && !f.fields[i].Optional {
					f.err = f.fields[i].Label + " is needed."
					return f.focusField(i), false
				}
			}
			msg, cmd := f.submit(f.values())
			f.err = msg
			return cmd, msg == ""
		}
	}
	var c tea.Cmd
	f.fields[f.focus].input, c = f.fields[f.focus].input.Update(msg)
	f.err = ""
	return c, false
}

func (f *form) View(width int) string {
	var b strings.Builder
	b.WriteString(titleStyle.Render(f.title) + "\n")
	if f.hint != "" {
		b.WriteString(dimStyle.Render(f.hint) + "\n")
	}
	b.WriteString("\n")

	labelW := 0
	for _, fl := range f.fields {
		if n := lipgloss.Width(fl.Label); n > labelW {
			labelW = n
		}
	}
	for i, fl := range f.fields {
		marker := "  "
		label := dimStyle.Render(pad(fl.Label, labelW))
		if i == f.focus {
			marker = lipgloss.NewStyle().Foreground(accent).Render("▸ ")
			label = lipgloss.NewStyle().Foreground(accent).Render(pad(fl.Label, labelW))
		}
		b.WriteString(marker + label + "  " + fl.input.View() + "\n")
	}
	if f.err != "" {
		b.WriteString("\n" + badStyle.Render("  "+f.err) + "\n")
	}
	b.WriteString("\n" + dimStyle.Render("tab moves · enter continues · esc cancels"))
	return b.String()
}

func pad(s string, w int) string {
	if n := lipgloss.Width(s); n < w {
		return s + strings.Repeat(" ", w-n)
	}
	return s
}
