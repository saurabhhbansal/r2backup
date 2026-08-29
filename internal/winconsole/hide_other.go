//go:build !windows

package winconsole

// Hide does nothing off Windows. systemd and launchd start a process with no
// controlling terminal, so there is no window to hide and never was.
func Hide() bool { return false }
