//go:build !windows

package runstate

import (
	"os"
	"syscall"
)

// processAlive reports whether a PID is still running. Signal 0 performs the
// permission and existence checks without delivering anything.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid) // never fails on Unix
	if err != nil {
		return false
	}
	return p.Signal(syscall.Signal(0)) == nil
}
