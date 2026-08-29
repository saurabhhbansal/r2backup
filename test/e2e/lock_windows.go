//go:build windows

package e2e

import (
	"fmt"

	"golang.org/x/sys/windows"
)

// lockFile opens path with no sharing at all, so any other attempt to open
// it -- including this same process's own backup run reading it a moment
// later -- fails with a sharing violation exactly like a file held open by
// an antivirus scanner or another application would. This is the Windows
// shape of "a file locked/unreadable during the run"; unlike the Unix
// permission-based version, it is never ineffective for a privileged
// process, since Windows' share-mode lock is enforced by the kernel
// regardless of who holds the handle.
func lockFile(path string) (unlock func(), ineffective bool, err error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, false, fmt.Errorf("lockFile: %w", err)
	}
	h, err := windows.CreateFile(
		p,
		windows.GENERIC_READ,
		0, // no sharing: FILE_SHARE_READ/WRITE/DELETE all withheld
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL,
		0,
	)
	if err != nil {
		return nil, false, fmt.Errorf("lockFile: CreateFile %s: %w", path, err)
	}
	return func() { windows.CloseHandle(h) }, false, nil
}
