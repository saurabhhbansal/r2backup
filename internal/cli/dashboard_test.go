package cli

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/saurabhhbansal/r2backup/internal/progress"
	"github.com/saurabhhbansal/r2backup/internal/sets"
	"github.com/saurabhhbansal/r2backup/internal/ui"
)

// TestTheDashboardSeesAndDoesRealWork drives the interface's backend against a
// real object store. The screens are tested against a fake in internal/ui;
// this is the other half -- that what the screens are handed is true, and that
// a backup started from the interface really uploads and is really recorded.
func TestTheDashboardSeesAndDoesRealWork(t *testing.T) {
	_, c := bucketWithABackupInIt(t)
	dataDir := t.TempDir()
	t.Setenv("R2BACKUP_DATA_DIR", dataDir)

	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}

	a, err := openApp()
	if err != nil {
		t.Fatal(err)
	}
	if err := a.creds.Save(c); err != nil {
		t.Fatal(err)
	}
	if err := a.sets.Add(sets.Set{
		Name: "Notes", Root: root, Machine: "testpc",
		Prefix: "machines/testpc/Notes", RetentionDays: 30,
	}); err != nil {
		t.Fatal(err)
	}
	a.close()

	d, err := openDashboard(&Options{Out: os.Stderr, Err: os.Stderr})
	if err != nil {
		t.Fatal(err)
	}
	defer d.close()
	ctx := context.Background()

	views, ov, err := d.Load(ctx)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(views) != 1 || views[0].Name != "Notes" {
		t.Fatalf("Load returned %+v, want one set called Notes", views)
	}
	if views[0].State != "never run" {
		t.Errorf("a set that has never run should say so, got %q", views[0].State)
	}
	if ov.Bucket != c.Bucket {
		t.Errorf("overview bucket = %q, want %q", ov.Bucket, c.Bucket)
	}
	// Load runs once a second while the window is open. If it ever starts
	// reaching the network, an idle dashboard burns operations all day.
	if ov.OpsLimit == 0 {
		t.Error("the free-tier limit should be reported so the footer can show it")
	}

	var phases []string
	var lastSnap progress.Snapshot
	if err := d.Backup(ctx, "Notes",
		func(p string) { phases = append(phases, p) },
		func(s progress.Snapshot) { lastSnap = s },
	); err != nil {
		t.Fatalf("Backup: %v", err)
	}
	if len(phases) == 0 {
		t.Error("the running screen was never told what stage it was at")
	}
	if lastSnap.BytesTotal == 0 {
		t.Error("the progress bar was never given a total to draw against")
	}

	// The run has to be visible afterwards, and to `r2b status` as well: the
	// interface and the command read the same history file, and a run only
	// one of them knows about is the kind of disagreement that makes a person
	// stop trusting both.
	views, _, err = d.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if views[0].State != "ok" || !views[0].HasRun {
		t.Fatalf("after a backup the set reads %q hasRun=%v, want ok/true", views[0].State, views[0].HasRun)
	}
	if views[0].Uploaded != 1 {
		t.Errorf("uploaded = %d, want 1", views[0].Uploaded)
	}
	if views[0].Objects != 1 {
		t.Errorf("objects = %d, want 1", views[0].Objects)
	}

	rows, err := d.Trash(ctx, "Notes")
	if err != nil {
		t.Fatalf("Trash: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("nothing has been deleted yet, so trash should be empty, got %d", len(rows))
	}

	if err := d.Remove(ctx, "Notes", false); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	views, _, err = d.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(views) != 0 {
		t.Errorf("the set is still listed after being removed: %+v", views)
	}
}

// The interface must satisfy the seam it is written against.
var _ ui.Backend = (*dashboard)(nil)

// TestTheDashboardServesTwoCallersAtOnce is the regression for the defect
// that made the whole interface unusable during a backup.
//
// Every method used to call openApp() for itself. bbolt takes a file lock
// that contends with itself inside one process, so while a backup held the
// index, the interface's once-a-second refresh blocked for bbolt's five
// second timeout and then failed with `open index at "...": timeout`. The
// progress screen was replaced by an error about a file, the model stopped
// believing a backup was running, and a second one could be started on top of
// the first.
//
// Measured before the fix: a second openApp returns an error after 4.99s.
func TestTheDashboardServesTwoCallersAtOnce(t *testing.T) {
	_, c := bucketWithABackupInIt(t)
	dataDir := t.TempDir()
	t.Setenv("R2BACKUP_DATA_DIR", dataDir)

	root := t.TempDir()
	for _, n := range []string{"a.txt", "b.txt", "c.txt"} {
		if err := os.WriteFile(filepath.Join(root, n), []byte(n), 0o644); err != nil {
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
	ctx := context.Background()

	// Refresh on a ticker for as long as the backup runs, exactly as the
	// interface does, and fail on the first error rather than at the end.
	stop := make(chan struct{})
	loadErr := make(chan error, 1)
	go func() {
		for {
			select {
			case <-stop:
				loadErr <- nil
				return
			default:
			}
			if _, _, err := d.Load(ctx); err != nil {
				loadErr <- err
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
	}()

	backupErr := d.Backup(ctx, "Notes", func(string) {}, func(progress.Snapshot) {})
	close(stop)

	if backupErr != nil {
		t.Fatalf("Backup: %v", backupErr)
	}
	if err := <-loadErr; err != nil {
		t.Fatalf("a refresh during a backup failed: %v", err)
	}
}

// A folder that has moved must be parked, exactly as `r2b backup` parks it.
// Without it the set stays StatusOK, the interface never says "needs
// attention", and the scheduled run keeps failing with nobody told.
func TestAMovedFolderIsParkedByTheInterfaceToo(t *testing.T) {
	_, c := bucketWithABackupInIt(t)
	t.Setenv("R2BACKUP_DATA_DIR", t.TempDir())

	gone := filepath.Join(t.TempDir(), "not-there")
	setup, err := openApp()
	if err != nil {
		t.Fatal(err)
	}
	if err := setup.creds.Save(c); err != nil {
		t.Fatal(err)
	}
	if err := setup.sets.Add(sets.Set{
		Name: "Gone", Root: gone, Machine: "testpc",
		Prefix: "machines/testpc/Gone", RetentionDays: 30,
	}); err != nil {
		t.Fatal(err)
	}
	setup.close()

	d, err := openDashboard(&Options{Out: os.Stderr, Err: os.Stderr})
	if err != nil {
		t.Fatal(err)
	}
	defer d.close()
	ctx := context.Background()

	if err := d.Backup(ctx, "Gone", func(string) {}, func(progress.Snapshot) {}); err == nil {
		t.Fatal("backing up a folder that is not there should fail")
	}
	views, _, err := d.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(views) != 1 || views[0].State != "needs attention" {
		t.Fatalf("state = %q, want \"needs attention\"", views[0].State)
	}
	if views[0].Note == "" {
		t.Error("nothing says what is wrong with it")
	}
}
