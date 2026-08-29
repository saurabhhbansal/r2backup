package backup_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/saurabhhbansal/r2backup/internal/backup"
	"github.com/saurabhhbansal/r2backup/internal/index"
	"github.com/saurabhhbansal/r2backup/internal/remote"
	"github.com/saurabhhbansal/r2backup/internal/sets"
	"github.com/saurabhhbansal/r2backup/test/fixtures"
	tminio "github.com/saurabhhbansal/r2backup/test/minio"
)

type harness struct {
	client *remote.Client
	db     *index.DB
	set    sets.Set
	root   string
}

func setup(t *testing.T, spec fixtures.Spec) *harness {
	t.Helper()
	client, cleanup := tminio.Start(t)
	t.Cleanup(cleanup)

	root := t.TempDir()
	if _, err := fixtures.Build(root, spec); err != nil {
		t.Fatal(err)
	}
	db, err := index.Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	return &harness{
		client: client,
		db:     db,
		root:   root,
		set: sets.Set{
			Name:          "Code Projects",
			Prefix:        "machines/test-pc/Code Projects",
			Root:          root,
			Machine:       "test-pc",
			RetentionDays: sets.DefaultRetentionDays,
		},
	}
}

func (h *harness) run(t *testing.T) *backup.Report {
	t.Helper()
	rep, err := backup.Run(context.Background(), backup.Options{
		Set:         h.set,
		Index:       h.db,
		Client:      h.client,
		DetectMoves: true,
	})
	if err != nil {
		t.Fatalf("backup.Run: %v", err)
	}
	return rep
}

func (h *harness) liveKeys(t *testing.T) map[string]int64 {
	t.Helper()
	entries, err := h.client.List(context.Background(), h.set.Prefix+"/current/")
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]int64{}
	for _, e := range entries {
		out[e.Key] = e.Size
	}
	return out
}

func TestFirstBackupUploadsTheWholeTree(t *testing.T) {
	h := setup(t, fixtures.Spec{
		SmallFiles:    120,
		SmallFileSize: 512,
		ZeroByteFiles: 3,
		AwkwardNames:  true,
		UnicodeNames:  true,
		EmptyDirs:     2,
		Symlinks:      true,
		Seed:          21,
	})

	rep := h.run(t)
	if !rep.Succeeded() {
		t.Fatalf("failures: %v", rep.Failures)
	}
	if rep.Uploaded == 0 {
		t.Fatal("nothing was uploaded")
	}

	live := h.liveKeys(t)
	if len(live) != rep.Uploaded {
		t.Errorf("bucket holds %d objects but the run reported %d uploaded", len(live), rep.Uploaded)
	}
	// The fixture writes "résumé.txt" precomposed AND decomposed. Whether that
	// is one file or two is a property of the filesystem, not of this tool:
	// ext4 keeps the bytes it was given and holds both, while APFS normalizes
	// on the way in and holds one. So the expectation is derived from what is
	// actually on disk rather than from runtime.GOOS -- naming the OS would
	// still be a guess about the filesystem underneath it.
	unicodeFiles, err := os.ReadDir(filepath.Join(h.root, "unicode"))
	if err != nil {
		t.Fatal(err)
	}
	if len(unicodeFiles) > 1 {
		// Two files on disk, one possible key. Exactly one can be stored, and
		// the run must say so rather than quietly dropping the other.
		if len(rep.Collisions) == 0 {
			t.Error("the NFC/NFD pair was not reported as a collision; one file would be lost in silence")
		}
		if rep.Complete() {
			t.Error("Complete() should be false when a collision means the bucket lacks a local file")
		}
	} else {
		// The filesystem normalized them into one file, so there is nothing to
		// collide and nothing to report.
		if len(rep.Collisions) != 0 {
			t.Errorf("this filesystem stores the NFC/NFD pair as one file, so there is no collision to report: %v", rep.Collisions)
		}
	}
	// The bucket must be browsable: real folder structure under current/.
	var sawNested bool
	for k := range live {
		if filepath.Base(k) != k {
			sawNested = true
		}
	}
	if !sawNested {
		t.Error("no nested keys; the bucket should mirror the folder structure")
	}
}

// TestSecondBackupOfAnUnchangedTreeIsFree is the pricing argument, executed.
// If this ever fails, the free-tier claim in the design fails with it.
func TestSecondBackupOfAnUnchangedTreeIsFree(t *testing.T) {
	h := setup(t, fixtures.Spec{SmallFiles: 60, SmallFileSize: 256, Seed: 5})

	first := h.run(t)
	if first.Uploaded == 0 {
		t.Fatal("first run uploaded nothing")
	}

	second := h.run(t)
	if second.Uploaded != 0 || second.Moved != 0 || second.Deleted != 0 {
		t.Errorf("second run did work: %d uploaded, %d moved, %d deleted",
			second.Uploaded, second.Moved, second.Deleted)
	}
	if second.Operations != 0 {
		t.Errorf("second run cost %d operations, want 0 -- an idle run must be free", second.Operations)
	}
	if second.Unchanged != first.Uploaded {
		t.Errorf("Unchanged = %d, want %d", second.Unchanged, first.Uploaded)
	}
}

func TestOnlyChangedFilesMoveOnASecondRun(t *testing.T) {
	h := setup(t, fixtures.Spec{SmallFiles: 80, SmallFileSize: 256, Seed: 8})
	h.run(t)

	edited, deleted, added, err := fixtures.Mutate(h.root, 5, 3, 2, 8)
	if err != nil {
		t.Fatal(err)
	}
	// Mutate rewrites files in the same second; nudge mtimes past the
	// two-second tolerance so the change is genuinely detectable, which is
	// what a real edit minutes later would look like.
	future := time.Now().Add(time.Hour)
	for _, rel := range edited {
		p := filepath.Join(h.root, filepath.FromSlash(rel))
		if err := touch(p, future); err != nil {
			t.Fatal(err)
		}
	}

	rep := h.run(t)
	// Deleting a file can leave its directory empty, and an empty directory
	// needs a marker object or it vanishes on restore. So the upload count is
	// the edits, the additions, and any directory that just became empty.
	wantMin := len(edited) + len(added)
	wantMax := wantMin + len(deleted)
	if rep.Uploaded < wantMin || rep.Uploaded > wantMax {
		t.Errorf("Uploaded = %d, want between %d and %d (%d edited + %d added, plus up to %d newly-empty directories)",
			rep.Uploaded, wantMin, wantMax, len(edited), len(added), len(deleted))
	}
	if rep.Deleted != len(deleted) {
		t.Errorf("Deleted = %d, want %d", rep.Deleted, len(deleted))
	}

	live := h.liveKeys(t)
	for _, rel := range deleted {
		if _, still := live[h.set.Prefix+"/current/"+rel]; still {
			t.Errorf("%q was deleted locally but is still in the bucket", rel)
		}
	}
}

func TestAMissingRootIsNeverReadAsDeletion(t *testing.T) {
	// The most destructive bug available to this design. A renamed folder, an
	// unplugged drive or an unmounted share must not empty a backup.
	h := setup(t, fixtures.Spec{SmallFiles: 30, SmallFileSize: 128, Seed: 3})
	first := h.run(t)

	h.set.Root = filepath.Join(t.TempDir(), "definitely-not-here")

	_, err := backup.Run(context.Background(), backup.Options{
		Set: h.set, Index: h.db, Client: h.client,
	})
	if err == nil {
		t.Fatal("a missing root must stop the run, not proceed")
	}
	if !isRootMissing(err) {
		t.Fatalf("got %v, want backup.ErrRootMissing so the caller can offer to relink", err)
	}

	live := h.liveKeys(t)
	if len(live) != first.Uploaded {
		t.Fatalf("the bucket lost objects when the root vanished: %d remain of %d.\n"+
			"A missing folder was treated as a mass deletion.", len(live), first.Uploaded)
	}
}

func TestRenamingAFolderCopiesInsteadOfReuploading(t *testing.T) {
	h := setup(t, fixtures.Spec{SmallFiles: 20, SmallFileSize: 8192, Seed: 13})
	first := h.run(t)

	// Rename the directory the fixture put everything under.
	oldDir := filepath.Join(h.root, "pkg")
	newDir := filepath.Join(h.root, "packages")
	if err := renameDir(oldDir, newDir); err != nil {
		t.Fatal(err)
	}

	rep := h.run(t)
	if rep.Moved == 0 {
		t.Fatalf("a renamed folder produced %d moves and %d uploads; it should be copies, not a re-upload",
			rep.Moved, rep.Uploaded)
	}
	if rep.Bytes != 0 {
		t.Errorf("a rename transferred %d bytes; server-side copies move none", rep.Bytes)
	}
	live := h.liveKeys(t)
	if len(live) != first.Uploaded {
		t.Errorf("object count changed across a rename: %d, want %d", len(live), first.Uploaded)
	}
}
