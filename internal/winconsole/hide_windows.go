//go:build windows

package winconsole

import "golang.org/x/sys/windows"

// swHide is ShowWindow's nCmdShow for "hide the window and activate another".
const swHide = 0

// Resolved lazily: none of these three has a wrapper in x/sys/windows, and a
// lazy DLL costs nothing until the one code path that needs it runs.
// NewLazySystemDLL rather than NewLazyDLL: it resolves out of System32 only,
// so a stray user32.dll beside the binary cannot be loaded instead.
var (
	kernel32               = windows.NewLazySystemDLL("kernel32.dll")
	user32                 = windows.NewLazySystemDLL("user32.dll")
	procGetConsoleWindow   = kernel32.NewProc("GetConsoleWindow")
	procGetConsoleProcList = kernel32.NewProc("GetConsoleProcessList")
	procShowWindow         = user32.NewProc("ShowWindow")
)

// Hide hides this process's console window and reports whether it did.
//
// It refuses unless this process is the only one attached to the console.
// Running `r2backup backup --hidden` by hand from PowerShell would otherwise
// hide *PowerShell's* window -- GetConsoleWindow returns the console the
// process is attached to, not one it owns, and a child inherits its parent's.
// Taking a user's terminal off the screen while fixing a window that flashes
// would be a strictly worse bug than the one being fixed, so ownership is
// checked rather than assumed.
func Hide() bool {
	hwnd, _, _ := procGetConsoleWindow.Call()
	if hwnd == 0 {
		return false // no console at all: nothing to hide
	}
	if !ownsConsole() {
		return false
	}
	procShowWindow.Call(hwnd, uintptr(swHide))
	return true
}

// ownsConsole reports whether this process is the only one attached to its
// console.
//
// GetConsoleProcessList counts attached client processes, and conhost.exe is
// not one of them: a console created for this process alone reports exactly 1,
// while anything launched from a shell reports at least 2. Two slots is all
// this needs -- the question is "exactly one or more than one", and the call
// still returns the true count when the buffer is too small to hold every pid.
func ownsConsole() bool {
	var pids [2]uint32
	n, _, _ := procGetConsoleProcList.Call(uintptr(unsafePtr(&pids[0])), uintptr(len(pids)))
	return n == 1
}
