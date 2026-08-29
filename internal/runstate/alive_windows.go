//go:build windows

package runstate

import "golang.org/x/sys/windows"

// processAlive reports whether a PID is still running. Windows has no signals,
// so this opens the process and asks for its exit code: STILL_ACTIVE means it
// is running.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return false
	}
	defer windows.CloseHandle(h)
	var code uint32
	if err := windows.GetExitCodeProcess(h, &code); err != nil {
		return false
	}
	const stillActive = 259
	return code == stillActive
}
