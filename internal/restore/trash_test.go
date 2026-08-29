package restore

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/saurabhhbansal/r2backup/internal/sets"
)

func TestSourcePrefix(t *testing.T) {
	set := sets.Set{Name: "Code", Prefix: "machines/pc-1/Code", Machine: "pc-1"}

	if got, err := sourcePrefix(set, ""); err != nil || got != "machines/pc-1/Code" {
		t.Errorf("sourcePrefix(_, \"\") = %q, %v; want the set's own prefix", got, err)
	}
	if got, err := sourcePrefix(set, "pc-1"); err != nil || got != "machines/pc-1/Code" {
		t.Errorf("sourcePrefix(_, own machine) = %q, %v; want the set's own prefix", got, err)
	}
	if got, err := sourcePrefix(set, "pc-2"); err != nil || got != "machines/pc-2/Code" {
		t.Errorf("sourcePrefix(_, \"pc-2\") = %q, %v; want machines/pc-2/Code", got, err)
	}

	oddSet := sets.Set{Name: "Code", Prefix: "just-a-name", Machine: "pc-1"}
	if _, err := sourcePrefix(oddSet, "pc-2"); err == nil {
		t.Error("sourcePrefix should refuse a prefix that isn't machines/<machine>/<name> shaped")
	}
}

func TestFindInTrashPicksTheNewestVersion(t *testing.T) {
	backend := newFakeBackend()
	backend.put("proj/trash/2024-01-01/report~090000-aaaaaa.pdf", []byte("oldest"), fileMeta())
	backend.put("proj/trash/2024-01-02/report~080000-bbbbbb.pdf", []byte("newer day, earlier time"), fileMeta())
	backend.put("proj/trash/2024-01-02/report~150000-cccccc.pdf", []byte("newest"), fileMeta())
	backend.put("proj/trash/2024-01-01/unrelated~090000-dddddd.txt", []byte("ignore me"), fileMeta())

	key, size, err := findInTrash(context.Background(), backend, "proj", "report.pdf")
	if err != nil {
		t.Fatal(err)
	}
	if key != "proj/trash/2024-01-02/report~150000-cccccc.pdf" {
		t.Errorf("key = %q, want the 15:00:00 entry from 2024-01-02", key)
	}
	if size != int64(len("newest")) {
		t.Errorf("size = %d, want %d", size, len("newest"))
	}
}

func TestFindInTrashReportsNotFound(t *testing.T) {
	backend := newFakeBackend()
	backend.put("proj/trash/2024-01-01/other~090000-aaaaaa.txt", []byte("x"), fileMeta())

	_, _, err := findInTrash(context.Background(), backend, "proj", "report.pdf")
	if err == nil {
		t.Fatal("expected an error for a path never trashed")
	}
}

func TestRestoreDeletedRecoversOneFile(t *testing.T) {
	backend := newFakeBackend()
	backend.put("proj/trash/2024-01-01/report~090000-aaaaaa.pdf", []byte("old content"), fileMeta())
	backend.put("proj/trash/2024-01-05/report~120000-bbbbbb.pdf", []byte("latest content"), fileMeta())

	target := t.TempDir()
	rep, err := Run(context.Background(), Options{
		Set:     testSet("proj", target),
		Client:  backend,
		Target:  target,
		Deleted: "report.pdf",
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.Downloaded != 1 {
		t.Errorf("Downloaded = %d, want 1", rep.Downloaded)
	}
	data, err := os.ReadFile(filepath.Join(target, "report.pdf"))
	if err != nil || string(data) != "latest content" {
		t.Errorf("report.pdf = %q, %v; want the most recent trashed content", data, err)
	}
}

func TestRestoreDeletedNoVersionFoundErrors(t *testing.T) {
	backend := newFakeBackend()
	target := t.TempDir()
	_, err := Run(context.Background(), Options{
		Set:     testSet("proj", target),
		Client:  backend,
		Target:  target,
		Deleted: "never-existed.pdf",
	})
	if err == nil {
		t.Fatal("expected an error when nothing matches the requested path in trash")
	}
}
