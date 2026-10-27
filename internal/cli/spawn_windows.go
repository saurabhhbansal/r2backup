//go:build windows

package cli

import (
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
)

// hideWindow keeps a child r2b from opening a console window: the taskbar
// app starts backups the way the scheduler does, out of sight.
func hideWindow(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW}
}

// openInTerminal opens r2b's interface in a console window of its own.
func openInTerminal(exe string) error {
	cmd := exec.Command(exe)
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NEW_CONSOLE}
	return cmd.Start()
}
