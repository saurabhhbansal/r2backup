//go:build !windows

package cli

import (
	"errors"
	"os/exec"
)

func hideWindow(cmd *exec.Cmd) {}

func openInTerminal(exe string) error {
	return errors.New("opening a terminal is only done from the Windows taskbar app")
}
