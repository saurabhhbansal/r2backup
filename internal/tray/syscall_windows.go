//go:build windows

package tray

import "syscall"

func syscallN(fn uintptr, args ...uintptr) (uintptr, uintptr, error) {
	r1, r2, errno := syscall.SyscallN(fn, args...)
	return r1, r2, errno
}
