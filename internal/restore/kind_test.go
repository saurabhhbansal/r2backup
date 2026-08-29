package restore_test

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/saurabhhbansal/r2backup/internal/backup"
	"github.com/saurabhhbansal/r2backup/internal/index"
	"github.com/saurabhhbansal/r2backup/internal/restore"
	"github.com/saurabhhbansal/r2backup/internal/sets"
	tminio "github.com/saurabhhbansal/r2backup/test/minio"
)

// An empty executable file and an empty-directory marker are the same size and
// carry the same permission bits. Before objects recorded their kind, restore
// had to infer one from the other, and this file came back as a directory --
// silently, and only for people who happen to keep empty shell scripts.
//
// The wire format now says what each object is. This asserts it.
func TestEmptyExecutableFileComesBackAsAFileNotADirectory(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix permission bits are what made these two indistinguishable")
	}
	client, cleanup := tminio.Start(t)
	t.Cleanup(cleanup)

	root := t.TempDir()
	script := filepath.Join(root, "run.sh")
	if err := os.WriteFile(script, nil, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "genuinely-empty"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A plain empty file too, so the fallback path is not the only thing
	// keeping this honest.
	if err := os.WriteFile(filepath.Join(root, "plain-empty.txt"), nil, 0o644); err != nil {
		t.Fatal(err)
	}

	db, err := index.Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	set := sets.Set{
		Name: "kinds", Prefix: "machines/test-pc/kinds",
		Root: root, Machine: "test-pc", RetentionDays: 30,
	}
	if _, err := backup.Run(context.Background(), backup.Options{
		Set: set, Index: db, Client: client,
	}); err != nil {
		t.Fatalf("backup: %v", err)
	}

	target := t.TempDir()
	if _, err := restore.Run(context.Background(), restore.Options{
		Set: set, Client: client, Target: target, Overwrite: true,
	}); err != nil {
		t.Fatalf("restore: %v", err)
	}

	cases := []struct {
		rel   string
		isDir bool
		mode  os.FileMode
	}{
		{"run.sh", false, 0o755},
		{"plain-empty.txt", false, 0o644},
		{"genuinely-empty", true, 0},
	}
	for _, tc := range cases {
		info, err := os.Lstat(filepath.Join(target, tc.rel))
		if err != nil {
			t.Errorf("%s was not restored at all: %v", tc.rel, err)
			continue
		}
		if info.IsDir() != tc.isDir {
			what := "a file"
			if info.IsDir() {
				what = "a directory"
			}
			t.Errorf("%s came back as %s; the object's recorded kind was ignored", tc.rel, what)
			continue
		}
		if !tc.isDir && info.Size() != 0 {
			t.Errorf("%s came back with %d bytes, want 0", tc.rel, info.Size())
		}
		if !tc.isDir && info.Mode().Perm() != tc.mode {
			t.Errorf("%s came back mode %o, want %o", tc.rel, info.Mode().Perm(), tc.mode)
		}
	}
}
