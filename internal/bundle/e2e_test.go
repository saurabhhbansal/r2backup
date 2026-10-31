package bundle

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/saurabhhbansal/r2backup/internal/backup"
	"github.com/saurabhhbansal/r2backup/internal/index"
	"github.com/saurabhhbansal/r2backup/internal/mirror"
	"github.com/saurabhhbansal/r2backup/internal/restore"
	"github.com/saurabhhbansal/r2backup/internal/sets"
)

// The real backup, trash, restore and mirror code, run against a bundling
// store: bundling has to be invisible to all of them.
func TestBackupTrashRestoreAndMirrorThroughBundles(t *testing.T) {
	ctx := context.Background()
	base := newMem()
	root := t.TempDir()
	write := func(rel, body string) {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 120; i++ {
		write(fmt.Sprintf("notes/n%03d.txt", i), fmt.Sprintf("note %d\n", i))
	}
	write("big.bin", noise(7, 6<<20)) // over SmallMax: its own object

	db, err := index.Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	set := sets.Set{Name: "Docs", Prefix: "machines/pc/Docs", Root: root, Machine: "pc", RetentionDays: 30}

	run := func() *backup.Report {
		t.Helper()
		s := New(rangeMem{base}, Options{Pack: true, TempDir: t.TempDir()})
		rep, err := backup.Run(ctx, backup.Options{Set: set, Index: db, Client: s,
			Trash: backup.NewTrash(s, set.RetentionDays), DetectMoves: true})
		if err != nil {
			t.Fatalf("backup: %v", err)
		}
		return rep
	}

	rep := run()
	if rep.Uploaded != 121 {
		t.Fatalf("uploaded %d, want 121", rep.Uploaded)
	}
	if base.puts > 10 {
		t.Errorf("121 files took %d writes", base.puts)
	}
	if rep.Operations > 10 {
		t.Errorf("the run reported %d operations; bundling should report what it sent", rep.Operations)
	}

	// Change one file and delete another: the old versions go to trash.
	write("notes/n005.txt", "changed\n")
	if err := os.Remove(filepath.Join(root, "notes", "n006.txt")); err != nil {
		t.Fatal(err)
	}
	before := base.puts
	rep = run()
	if rep.Uploaded != 1 {
		t.Fatalf("second run uploaded %d, want 1", rep.Uploaded)
	}
	if base.puts-before > 3 {
		t.Errorf("one changed file took %d writes", base.puts-before)
	}

	// Restore everything elsewhere, through a fresh store.
	reader := New(rangeMem{base}, Options{TempDir: t.TempDir()})
	target := t.TempDir()
	if _, err := restore.Run(ctx, restore.Options{Set: set, Client: reader, Target: target}); err != nil {
		t.Fatalf("restore: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(target, "notes", "n005.txt"))
	if err != nil || string(got) != "changed\n" {
		t.Fatalf("restored n005 = %q, %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(target, "notes", "n006.txt")); !os.IsNotExist(err) {
		t.Fatalf("a deleted file came back: %v", err)
	}
	big, err := os.ReadFile(filepath.Join(target, "big.bin"))
	if err != nil || string(big) != noise(7, 6<<20) {
		t.Fatal("big file did not restore")
	}

	// The old version of n005 and all of n006 are in trash.
	trashed, err := reader.List(ctx, set.KeyScope()+"trash/")
	if err != nil {
		t.Fatal(err)
	}
	if len(trashed) != 2 {
		t.Fatalf("trash holds %d, want 2: %+v", len(trashed), trashed)
	}

	// A mirror to a plain store copies files, not packs.
	dst := newMem()
	if _, err := mirror.Run(ctx, mirror.Options{Src: reader, Dst: dst, Prefixes: []string{set.KeyScope()}, Delete: true}); err != nil {
		t.Fatalf("mirror: %v", err)
	}
	if !dst.has(set.KeyScope()+"current/notes/n000.txt") || dst.count(set.KeyScope()+Dir) != 0 {
		t.Fatalf("mirror did not copy plain files: %d bundling objects", dst.count(set.KeyScope()+Dir))
	}
}
