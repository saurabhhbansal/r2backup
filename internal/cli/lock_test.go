package cli

import (
	"bytes"
	"testing"
	"time"

	"github.com/saurabhhbansal/r2backup/internal/config"
	"github.com/saurabhhbansal/r2backup/internal/index"
)

// TestStatusWatchOpensNoIndex is the regression for the first of the three
// ways a second r2b process used to fail while another one held index.db:
// `status --watch` used to call openApp() before ever looking at the watch
// flag, so following a backup that was already running -- the one thing this
// flag exists for -- opened the very lock that backup was holding, and
// failed after bbolt's 5s timeout instead of following anything.
//
// watchProgress reads only progress.json, and a run in another process
// writes that file directly, so nothing about following it needs the index.
// This proves that in the one way that actually matters: with the index
// held open elsewhere for the whole test, `status --watch` must still return
// promptly rather than blocking on a lock it was never supposed to want.
func TestStatusWatchOpensNoIndex(t *testing.T) {
	t.Setenv("R2BACKUP_DATA_DIR", t.TempDir())

	idxPath, err := config.IndexPath()
	if err != nil {
		t.Fatal(err)
	}
	holder, err := index.Open(idxPath)
	if err != nil {
		t.Fatalf("Open (holder): %v", err)
	}
	defer holder.Close()

	var out, errOut bytes.Buffer
	root := NewRoot(&Options{Out: &out, Err: &errOut})
	root.SetOut(&out)
	root.SetErr(&errOut)
	// No progress.json exists, so watchProgress prints "No backup is
	// running." and returns on its first pass -- it never needs the ticker,
	// let alone the index.
	root.SetArgs([]string{"status", "--watch"})

	done := make(chan error, 1)
	start := time.Now()
	go func() { done <- root.Execute() }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("status --watch: %v\n--- output ---\n%s%s", err, out.String(), errOut.String())
		}
	case <-time.After(2 * time.Second):
		// bbolt's own lock timeout is 5s: if this command were opening the
		// index, it would still be blocked here, not merely slow.
		t.Fatal("status --watch did not return while the index was held elsewhere -- it must not be opening the index at all")
	}

	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("status --watch took %s while the index was held elsewhere, want near-instant", elapsed)
	}
	if got := out.String(); got == "" {
		t.Error("status --watch printed nothing")
	}
}
