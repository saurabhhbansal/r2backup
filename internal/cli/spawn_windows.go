//go:build windows

package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/windows"
)

// hideWindow keeps a child r2b from opening a console window: the taskbar
// app starts backups the way the scheduler does, out of sight.
func hideWindow(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW}
}

// openInTerminal opens r2b's interface in a console window of its own.
//
// Not exec.Command with CREATE_NEW_CONSOLE: os/exec always hands the child
// standard handles, and with none set they are NUL -- so the new window
// opens, the interface finds no terminal on its input and output, and
// exits before anyone sees it. ShellExecute starts it the way a double-click
// in Explorer does, attached to the new console, in whichever terminal the
// user has chosen as their default.
func openInTerminal(exe string) error {
	verb, _ := windows.UTF16PtrFromString("open")
	file, err := windows.UTF16PtrFromString(exe)
	if err != nil {
		return err
	}
	// Started in the home folder, as a terminal opened from the Start menu is.
	home, err := os.UserHomeDir()
	if err != nil {
		home = filepath.Dir(exe)
	}
	dir, _ := windows.UTF16PtrFromString(home)
	return windows.ShellExecute(0, verb, file, nil, dir, windows.SW_SHOWNORMAL)
}
