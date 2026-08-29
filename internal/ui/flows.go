package ui

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/saurabhhbansal/r2backup/internal/sets"
	"github.com/saurabhhbansal/r2backup/internal/tui"
)

// This file holds every flow that used to be a sentence telling the user to
// go and type a command. Adding a folder, restoring one, renaming, pointing a
// set at a folder that moved, signing in, storing credentials for another
// computer, entering R2 keys -- all of them happen in the window now.

// startBrowse opens the folder browser for `add`.
//
// A path typed from memory is the wrong way to choose a folder: it is the one
// step where a typo is silent, because a folder that does not exist and a
// folder you did not mean look the same on a prompt. bubbles/filepicker walks
// the disk instead, so what is added is something the user has looked at.
func (m *Model) startBrowse() tea.Cmd {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		home = "."
	}
	m.browse.CurrentDirectory = home
	m.overlay = overlayBrowse
	m.layout()
	return m.browse.Init()
}

// askTypedPath is the escape hatch out of the browser, and on Windows it is
// not optional -- see the comment on the "t" key in overlayKey.
func (m *Model) askTypedPath() {
	f := newForm(
		"Type the folder's path",
		"For a folder the browser cannot reach — another drive, or one you already know the path of.",
		[]field{{Label: "Path", Placeholder: exampleFolder()}},
		func(vals []string) (string, tea.Cmd) {
			p := cleanPath(vals[0])
			info, err := os.Stat(p)
			if err != nil {
				return "There is nothing at that path.", nil
			}
			if !info.IsDir() {
				return "That is a file, not a folder.", nil
			}
			return "", m.scanFolder(p, "")
		})
	f.SetValue(0, m.browse.CurrentDirectory)
	m.showForm(f)
}

func exampleFolder() string {
	if runtime.GOOS == "windows" {
		return `D:\Work`
	}
	return "/mnt/data/work"
}

// scanFolder walks a folder and then opens the tree picker on it. Used for a
// new folder (editing == "") and for changing an existing one.
func (m *Model) scanFolder(root, editing string) tea.Cmd {
	name := filepath.Base(root)
	m.notice = "Scanning " + root + "..."
	m.request++
	req := m.request
	return func() tea.Msg {
		info, err := os.Stat(root)
		if err != nil {
			return errMsg{err}
		}
		if !info.IsDir() {
			return errMsg{errors.New(root + " is not a folder")}
		}
		res, err := m.backend.Scan(m.ctx, root)
		if err != nil {
			return errMsg{err}
		}
		return scannedMsg{root: root, name: name, res: res, editing: editing, req: req}
	}
}

// openPicker shows the tree with everything already selected, or with an
// existing set's choices restored.
//
// The picker is internal/tui's model, embedded here rather than run as its
// own program. Two bubbletea programs would both be reading the same
// terminal; this way it is simply a child model, and choosing what a folder
// includes never leaves the window.
func (m *Model) openPicker(msg scannedMsg) tea.Cmd {
	p := tui.NewModel(msg.root, msg.res)
	if msg.editing != "" {
		if s, ok := m.setByName(msg.editing); ok {
			tui.ApplyExcludes(p.Root(), s.Excludes)
			p.Refresh()
		}
	}
	m.picker = p
	m.pickerFor = msg.editing
	m.pickerRoot = msg.root
	m.overlay = overlayPicker
	m.notice = ""
	m.layout()
	// Give it the terminal, once, before its first frame.
	p.Update(m.pickerSize())
	return nil
}

// finishAdd asks for the one thing the tree cannot say -- what to call it --
// then registers the folder and backs it up.
func (m *Model) finishAdd(root string, excludes []string) tea.Cmd {
	suggested := filepath.Base(root)
	hint := "It will be backed up now, and from then on whenever backups run."
	if other, ok := m.backend.Overlaps(root); ok {
		hint = "Note: this overlaps " + other + ". Files in both are stored twice,\nand cost operations twice on every run."
	}
	f := newForm(
		"Add "+root,
		hint,
		[]field{
			{Label: "Name", Placeholder: suggested},
			{Label: "Keep deleted files for", Placeholder: "30", Optional: true},
		},
		func(v []string) (string, tea.Cmd) {
			name := v[0]
			if name == "" {
				name = suggested
			}
			if _, exists := m.setByName(name); exists {
				return "There is already a folder called " + name + ".", nil
			}
			// Checked here, not only in the backend. sets.ValidName also
			// rejects a slash, a dot, a control character and anything too
			// long -- and reaching that check after the form has closed
			// costs the user the whole flow again: browse, scan, pick, name.
			// Choosing a drive root makes this the common case on Windows,
			// where filepath.Base(`C:\`) is `\`.
			if err := sets.ValidName(name); err != nil {
				return "That name will not work: " + err.Error(), nil
			}
			retention := 30
			if v[1] != "" {
				n, err := strconv.Atoi(v[1])
				if err != nil || n < 0 {
					return "Days has to be a whole number, or 0 to keep nothing.", nil
				}
				retention = n
			}
			req := AddRequest{Name: name, Root: root, Excludes: excludes, Retention: retention}
			// One plain command, not a tea.Sequence. Registering the
			// folder and backing it up are two steps, and the second must
			// not begin unless the first succeeded -- which a Sequence does
			// not promise, since it runs both regardless. An explicit
			// message makes the order something Update can be read for, and
			// tested.
			return "", func() tea.Msg {
				if err := m.backend.Add(m.ctx, req); err != nil {
					return errMsg{err}
				}
				return addedMsg(name)
			}
		})
	f.SetValue(0, suggested)
	f.SetValue(1, "30")
	m.showForm(f)
	return nil
}

func (m *Model) askRestore(v SetView) {
	hint := "Leave blank to put it back where it came from (" + v.Root + ")."
	if v.Root == "" {
		hint = "This folder was backed up from another computer, so it needs a destination."
	}
	m.showForm(newForm(
		"Restore "+v.Name,
		hint,
		[]field{
			{Label: "Into folder", Placeholder: v.Root, Optional: true},
			{Label: "Only paths matching", Placeholder: "everything", Optional: true},
			{Label: "Replace existing files", Placeholder: "no", Optional: true},
		},
		func(vals []string) (string, tea.Cmd) {
			to := cleanPath(vals[0])
			if to == "" && v.Root == "" {
				return "This one needs a destination folder.", nil
			}
			overwrite, err := yesNo(vals[2])
			if err != nil {
				return "Answer yes or no.", nil
			}
			return "", m.startRestore(RestoreRequest{
				Set: v.Name, To: to, Only: vals[1], Overwrite: overwrite,
			})
		}))
}

func (m *Model) askRecover(set string, row TrashRow) {
	m.showForm(newForm(
		"Recover "+row.Key,
		"From "+set+"'s trash. Recoverable until "+row.Expires.Format("2 January 2006")+".",
		[]field{{Label: "Into folder", Placeholder: "where it came from", Optional: true}},
		func(vals []string) (string, tea.Cmd) {
			return "", m.startRestore(RestoreRequest{
				Set: set, To: cleanPath(vals[0]), Deleted: row.Key, Overwrite: true,
			})
		}))
}

func (m *Model) askRename(v SetView) {
	f := newForm(
		"Rename "+v.Name,
		"The name only. The bucket keeps the prefix it was created with, so this is instant and costs nothing.",
		[]field{{Label: "New name"}},
		func(vals []string) (string, tea.Cmd) {
			if vals[0] == v.Name {
				return "That is the name it already has.", nil
			}
			if err := sets.ValidName(vals[0]); err != nil {
				return "That name will not work: " + err.Error(), nil
			}
			from, to := v.Name, vals[0]
			return "", func() tea.Msg {
				if err := m.backend.Rename(m.ctx, from, to); err != nil {
					return errMsg{err}
				}
				return noticeMsg("Renamed to " + to + ".")
			}
		})
	f.SetValue(0, v.Name)
	m.showForm(f)
}

func (m *Model) askRelink(v SetView) {
	f := newForm(
		"Where did "+v.Name+" go?",
		"Use this when the folder was renamed or moved. Nothing is re-uploaded -- what is in the bucket is still correct.",
		[]field{{Label: "New location"}},
		func(vals []string) (string, tea.Cmd) {
			p := cleanPath(vals[0])
			if info, err := os.Stat(p); err != nil || !info.IsDir() {
				return "There is no folder at that path.", nil
			}
			name := v.Name
			return "", func() tea.Msg {
				if err := m.backend.Relink(m.ctx, name, p); err != nil {
					return errMsg{err}
				}
				return noticeMsg(name + " now points at " + p + ". Nothing was re-uploaded.")
			}
		})
	f.SetValue(0, v.Root)
	m.showForm(f)
}

func (m *Model) askInterval() {
	current := 30
	if m.ov.Interval > 0 {
		current = int(m.ov.Interval.Minutes())
	}
	f := newForm(
		"How often should backups run?",
		"A run that finds nothing changed costs nothing, so this can be as often as you like.",
		[]field{{Label: "Every (minutes)"}},
		func(vals []string) (string, tea.Cmd) {
			n, err := strconv.Atoi(vals[0])
			if err != nil || n < 1 {
				return "Give a whole number of minutes, 1 or more.", nil
			}
			return "", m.setSchedule(n, false)
		})
	f.SetValue(0, strconv.Itoa(current))
	m.showForm(f)
}

// askEmail is the first step of signing in, and the only one a person has to
// start themselves. The code and the password follow from it.
func (m *Model) askEmail() {
	m.showForm(newForm(
		"Sign in",
		"An account lets your other computers pick up these credentials without you typing them again.",
		[]field{{Label: "Email", Placeholder: "you@example.com"}},
		func(vals []string) (string, tea.Cmd) {
			email := strings.ToLower(vals[0])
			if !strings.Contains(email, "@") {
				return "That does not look like an email address.", nil
			}
			return "", func() tea.Msg {
				if err := m.backend.SignInStart(m.ctx, email); err != nil {
					return errMsg{err}
				}
				return codeSentMsg(email)
			}
		}))
}

// codeSentMsg carries the address the code went to, so the next form can say
// it back and the verify call can be made with the same one.
type codeSentMsg string

func (m *Model) askCode(email string) {
	m.showForm(newForm(
		"Check your email",
		"A six-digit code is on its way to "+email+". It expires in 10 minutes.",
		[]field{{Label: "Code", Placeholder: "123456"}},
		func(vals []string) (string, tea.Cmd) {
			return "", func() tea.Msg {
				if err := m.backend.SignInVerify(m.ctx, email, vals[0]); err != nil {
					return errMsg{err}
				}
				return signedInMsg{}
			}
		}))
}

type signedInMsg struct{}

// askUnlock runs when signing in finds credentials already stored.
// askUnlock is reachable from the Account tab as well as straight after
// signing in. It was only reachable from the sign-in path, so one mistyped
// password cost a full sign-out and a fresh emailed code.
func (m *Model) askUnlock() {
	m.showForm(newForm(
		"Unlock your saved credentials",
		"The password you chose on the computer that stored them.",
		[]field{{Label: "Password", Secret: true}},
		func(vals []string) (string, tea.Cmd) {
			pw := vals[0]
			return "", func() tea.Msg {
				if err := m.backend.UnlockVault(m.ctx, pw); err != nil {
					return unlockFailedMsg{err}
				}
				return noticeMsg("Unlocked. This computer can reach your bucket.")
			}
		}))
}

// unlockFailedMsg reopens the password form rather than dropping the user
// back on the tab with an error and no way to try again.
type unlockFailedMsg struct{ err error }

func (m *Model) askStorePassword() {
	m.showForm(newForm(
		"Save these keys for your other computers",
		"They are encrypted here, under a password you choose. The server keeps a blob it cannot read.",
		[]field{
			{Label: "Password", Secret: true},
			{Label: "Again", Secret: true},
		},
		func(vals []string) (string, tea.Cmd) {
			if vals[0] != vals[1] {
				return "Those did not match.", nil
			}
			if len(vals[0]) < 8 {
				return "Use at least 8 characters.", nil
			}
			return "", func() tea.Msg {
				if err := m.backend.StoreVault(m.ctx, vals[0]); err != nil {
					return errMsg{err}
				}
				return noticeMsg("Stored. On your next computer, sign in with the same email.")
			}
		}))
}

func (m *Model) askKeys() {
	m.showForm(newForm(
		"Cloudflare R2 keys",
		"From the Cloudflare dashboard: R2 → Manage API tokens. Checked against your bucket before they are saved.",
		[]field{
			{Label: "Account ID"},
			{Label: "Access key ID"},
			{Label: "Secret access key", Secret: true},
			{Label: "Bucket"},
		},
		func(vals []string) (string, tea.Cmd) {
			k := Keys{AccountID: vals[0], AccessKeyID: vals[1], Secret: vals[2], Bucket: vals[3]}
			return "", func() tea.Msg {
				if err := m.backend.SaveKeys(m.ctx, k); err != nil {
					return errMsg{err}
				}
				return noticeMsg("Saved, and the connection works.")
			}
		}))
}

// cleanPath tidies a pasted path: quotes from a file manager's "copy as
// path", stray whitespace, and a trailing separator that is not the root.
func cleanPath(s string) string {
	s = strings.TrimSpace(s)
	s = strings.Trim(s, "\"'")
	s = strings.TrimSpace(s)
	if len(s) > 1 && (strings.HasSuffix(s, "/") || strings.HasSuffix(s, `\`)) {
		trimmed := s[:len(s)-1]
		// Keep "C:\" and "/" whole.
		if trimmed != "" && !strings.HasSuffix(trimmed, ":") {
			s = trimmed
		}
	}
	return s
}

func yesNo(s string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "n", "no", "false":
		return false, nil
	case "y", "yes", "true":
		return true, nil
	}
	return false, errors.New("not a yes or no")
}

// addedMsg is a folder that has just been registered and should now be
// backed up for the first time.
type addedMsg string

// unlockNeededMsg means the account has credentials stored and this computer
// needs the password to open them.
type unlockNeededMsg struct{}

// afterSignIn decides what a fresh sign-in should do next.
//
// Signing in on its own achieves nothing a user can see. If the account
// already holds credentials, the useful next step is to ask for the password
// and finish setting this computer up; if it does not, this is the first
// computer and the keys have to be typed once. Either way the interface takes
// the next step rather than announcing success and stopping.
func (m *Model) afterSignIn() tea.Cmd {
	return func() tea.Msg {
		a, err := m.backend.Account(m.ctx)
		if err != nil {
			return errMsg{err}
		}
		if a.VaultStored {
			return unlockNeededMsg{}
		}
		return noticeMsg("Signed in. This is the first computer on the account -- press k to enter your R2 keys, then p to save them for the next one.")
	}
}
