package ui

import (
	"context"
	"testing"
	"time"
)

// TestWaitBackgroundOutlivesAQuitDuringABackup is the M2(b) regression: q
// used to cancel the running job's context and quit without ever finding
// out whether backend.Backup had actually returned, so internal/cli's
// dashboard_cmd.go closed the index right underneath a backup goroutine
// that might still be writing to it.
//
// The real race is between that goroutine and app.close() in a different
// package, over a *bbolt.DB this package knows nothing about. This test
// reproduces the same shape without either of those: the fake backend's
// Backup call writes an ordinary, unsynchronized int the instant it
// returns, and the test reads that same int the instant WaitBackground
// returns. Those two accesses have no synchronization of their own -- only
// WaitBackground's WaitGroup can put a happens-before edge between them.
// So run this under -race: if WaitBackground were a no-op (the bug), the
// write and the read would be free to run concurrently whenever quitNow
// returns before the goroutine does, and the race detector would catch it.
// If WaitBackground genuinely waits, there is nothing to catch.
func TestWaitBackgroundOutlivesAQuitDuringABackup(t *testing.T) {
	var wroteAfterBackupReturned int
	holdUntilCancelled := make(chan struct{})
	releaseBackup := make(chan struct{})

	b := &fakeBackend{}
	b.backupHook = func(ctx context.Context) error {
		close(holdUntilCancelled)
		<-ctx.Done() // held open until quitNow cancels it, like a real transfer mid-flight
		<-releaseBackup
		// The plain write a broken wait would race against. In the real
		// bug this stands in for the index writes backup.Run makes while
		// draining what the engine had in flight.
		wroteAfterBackupReturned = 1
		return ctx.Err()
	}

	m := New(context.Background(), b)
	m.Update(loadedMsg{sets: nil, ov: Overview{}})
	if cmd := m.startBackup([]string{"Documents"}); cmd == nil {
		t.Fatal("startBackup returned no command")
	}
	if !m.running || m.runCancel == nil {
		t.Fatal("startBackup did not mark a run in flight")
	}

	select {
	case <-holdUntilCancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("the fake backup never started")
	}

	// Release the backend only after quitNow has had a chance to cancel and
	// return, so there is a real window in which the backup goroutine is
	// still running after quitNow is done -- the exact window app.close()
	// used to run in.
	go func() {
		time.Sleep(20 * time.Millisecond)
		close(releaseBackup)
	}()

	m.quitNow()
	m.WaitBackground()

	if wroteAfterBackupReturned != 1 {
		t.Fatal("WaitBackground returned before the backup goroutine finished")
	}
}
