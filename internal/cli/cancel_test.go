package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/saurabhhbansal/r2backup/internal/progress"
	"github.com/saurabhhbansal/r2backup/internal/sets"
)

// TestACancelledBackupDoesNotWriteACleanHistoryEntry is the M2 regression at
// the layer a person actually sees it from: the dashboard's history.
//
// Before the fix, eng.Run reported a cancelled run as nil-error success, so
// backup.Run's caller here (recordRun) had no way to tell a run someone
// stopped apart from one that genuinely finished, and wrote a clean entry --
// uploaded/moved/deleted counts and all -- for a run that did not do
// everything it was asked. The cancel fires from the phase callback the
// moment the transfer phase is announced, which is after scanning and
// planning have already finished, so this is cancelling the engine's own
// work, not the separate (and already correct) cancellation scan.Walk does
// for itself.
func TestACancelledBackupDoesNotWriteACleanHistoryEntry(t *testing.T) {
	_, c := bucketWithABackupInIt(t)
	dataDir := t.TempDir()
	t.Setenv("R2BACKUP_DATA_DIR", dataDir)

	root := t.TempDir()
	for i := 0; i < 40; i++ {
		name := filepath.Join(root, "f"+string(rune('a'+i%26))+".txt")
		if err := os.WriteFile(name, []byte(strings.Repeat("x", 256)), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	setup, err := openApp()
	if err != nil {
		t.Fatal(err)
	}
	if err := setup.creds.Save(c); err != nil {
		t.Fatal(err)
	}
	if err := setup.sets.Add(sets.Set{
		Name: "Notes", Root: root, Machine: "testpc",
		Prefix: "machines/testpc/Notes", RetentionDays: 30,
	}); err != nil {
		t.Fatal(err)
	}
	setup.close()

	d, err := openDashboard(&Options{Out: os.Stderr, Err: os.Stderr})
	if err != nil {
		t.Fatal(err)
	}
	defer d.close()

	ctx, cancel := context.WithCancel(context.Background())
	backupErr := d.Backup(ctx, "Notes",
		func(p string) {
			if strings.Contains(p, "uploading") {
				cancel()
			}
		},
		func(progress.Snapshot) {},
	)
	if backupErr == nil {
		t.Fatal("Backup: want an error for a run cancelled mid-flight, got nil")
	}

	// A fresh, uncancelled context: the history file is read back exactly as
	// `status` or the next window opening it would.
	views, _, err := d.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(views) != 1 {
		t.Fatalf("views = %d, want 1", len(views))
	}
	if views[0].State == "ok" {
		t.Fatalf("a cancelled run was recorded as %q -- want anything but a clean run", views[0].State)
	}
	if views[0].Note == "" {
		t.Error("a cancelled run left no note explaining why it is not ok")
	}
}
