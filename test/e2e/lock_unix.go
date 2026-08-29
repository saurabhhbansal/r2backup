//go:build !windows

package e2e

import "os"

// lockFile makes path unreadable to this process for the duration of the
// test, the Unix shape of "a file locked/unreadable during the run": a
// permission-denied read, not a Windows-style exclusive-open lock.
//
// It reports ineffective=true, rather than failing, when the attempt did not
// actually block a read -- which happens whenever the test runs as root:
// chmod 0000 has no effect on a process that owns the machine's every file.
// That is a fact about how the test was invoked, not a defect in the code
// under test, so the caller should skip rather than fail.
func lockFile(path string) (unlock func(), ineffective bool, err error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, false, err
	}
	original := info.Mode()
	if err := os.Chmod(path, 0o000); err != nil {
		return nil, false, err
	}
	unlock = func() { os.Chmod(path, original) }

	// Prove the lock actually bites before handing it to the caller.
	f, openErr := os.Open(path)
	if openErr == nil {
		f.Close()
		unlock()
		return nil, true, nil
	}
	return unlock, false, nil
}
