package fixtures

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestBuildCreatesRequestedShape(t *testing.T) {
	root := t.TempDir()
	m, err := Build(root, Spec{
		SmallFiles:    200,
		SmallFileSize: 100,
		ZeroByteFiles: 3,
		EmptyDirs:     2,
		Seed:          7,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Files) != 203 {
		t.Errorf("Files = %d, want 203", len(m.Files))
	}
	if len(m.Dirs) != 2 {
		t.Errorf("Dirs = %d, want 2", len(m.Dirs))
	}
	if m.Bytes != 200*100 {
		t.Errorf("Bytes = %d, want %d", m.Bytes, 200*100)
	}
	for _, rel := range m.Files {
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(rel))); err != nil {
			t.Fatalf("manifest lists %q but it is not on disk: %v", rel, err)
		}
	}
}

func TestBuildIsDeterministicForASeed(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	spec := Spec{SmallFiles: 50, SmallFileSize: 64, Seed: 42}
	if _, err := Build(a, spec); err != nil {
		t.Fatal(err)
	}
	if _, err := Build(b, spec); err != nil {
		t.Fatal(err)
	}
	diffs, err := Compare(a, b, 0)
	if err != nil {
		t.Fatal(err)
	}
	// Timestamps will differ; contents must not.
	for _, d := range diffs {
		if d.Reason == "contents differ" || d.Reason == "missing from the restored tree" {
			t.Errorf("same seed produced different trees: %s", d)
		}
	}
}

func TestCompareFindsAnIdenticalCopyClean(t *testing.T) {
	src := t.TempDir()
	if _, err := Build(src, Spec{SmallFiles: 30, AwkwardNames: true, EmptyDirs: 1, Seed: 3}); err != nil {
		t.Fatal(err)
	}
	dst := t.TempDir()
	if err := copyTree(src, dst); err != nil {
		t.Fatal(err)
	}
	diffs, err := Compare(src, dst, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if len(diffs) != 0 {
		t.Fatalf("an identical copy reported %d differences: %v", len(diffs), diffs)
	}
}

func TestCompareDetectsEveryKindOfDrift(t *testing.T) {
	src := t.TempDir()
	if _, err := Build(src, Spec{SmallFiles: 10, SmallFileSize: 50, Seed: 5}); err != nil {
		t.Fatal(err)
	}
	dst := t.TempDir()
	if err := copyTree(src, dst); err != nil {
		t.Fatal(err)
	}

	// Corrupt one file, delete another, invent a third.
	var files []string
	for _, f := range mustList(t, dst) {
		files = append(files, f)
	}
	if len(files) < 3 {
		t.Fatal("need at least 3 files")
	}
	if err := os.WriteFile(filepath.Join(dst, filepath.FromSlash(files[0])), []byte("corrupted"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dst, filepath.FromSlash(files[1]))); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dst, "invented.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	diffs, err := Compare(src, dst, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	reasons := map[string]bool{}
	for _, d := range diffs {
		reasons[d.Reason] = true
	}
	for _, want := range []string{"size differ", "missing from the restored tree", "unexpected: not in the original tree"} {
		found := false
		for r := range reasons {
			if len(r) >= 4 && (r == want || (want == "size differ" && r[:4] == "size")) {
				found = true
			}
		}
		if !found {
			t.Errorf("Compare missed %q; it reported %v", want, reasons)
		}
	}
}

func TestSymlinkFarmIsNotFollowed(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need Developer Mode on Windows")
	}
	root := t.TempDir()
	m, err := Build(root, Spec{Symlinks: true, Seed: 9})
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Symlinks) != 8 {
		t.Fatalf("Symlinks = %d, want 8", len(m.Symlinks))
	}
	// The store holds the real bytes once; the links must not duplicate them.
	if m.Bytes != 8*512 {
		t.Errorf("Bytes = %d, want %d -- links must not add their target's size", m.Bytes, 8*512)
	}
	for _, l := range m.Symlinks {
		fi, err := os.Lstat(filepath.Join(root, filepath.FromSlash(l)))
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode()&os.ModeSymlink == 0 {
			t.Errorf("%q is not a symlink", l)
		}
	}
}

func TestDeepPathExceedsWindowsLimit(t *testing.T) {
	root := t.TempDir()
	m, err := Build(root, Spec{DeepPath: true, Seed: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Files) == 0 && len(m.Skipped) == 0 {
		t.Fatal("neither created nor skipped the deep path")
	}
	for _, f := range m.Files {
		if len(f) > 260 {
			return
		}
	}
	if len(m.Skipped) == 0 {
		t.Errorf("deep path was only %d chars; it must exceed 260 to be a real test", len(m.Files[0]))
	}
}

func TestMutateReportsExactlyWhatItChanged(t *testing.T) {
	root := t.TempDir()
	if _, err := Build(root, Spec{SmallFiles: 40, SmallFileSize: 64, Seed: 11}); err != nil {
		t.Fatal(err)
	}
	edited, deleted, added, err := Mutate(root, 5, 3, 2, 11)
	if err != nil {
		t.Fatal(err)
	}
	if len(edited) != 5 || len(deleted) != 3 || len(added) != 2 {
		t.Fatalf("Mutate reported %d/%d/%d, want 5/3/2", len(edited), len(deleted), len(added))
	}
	for _, rel := range deleted {
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(rel))); !os.IsNotExist(err) {
			t.Errorf("%q was reported deleted but is still there", rel)
		}
	}
	for _, rel := range added {
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(rel))); err != nil {
			t.Errorf("%q was reported added but is not there", rel)
		}
	}
	// Edited and deleted must be disjoint, or the counts lie.
	seen := map[string]bool{}
	for _, r := range edited {
		seen[r] = true
	}
	for _, r := range deleted {
		if seen[r] {
			t.Errorf("%q was reported as both edited and deleted", r)
		}
	}
}

func mustList(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	err := filepath.Walk(root, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if fi.Mode().IsRegular() {
			rel, _ := filepath.Rel(root, p)
			out = append(out, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func copyTree(src, dst string) error {
	return filepath.Walk(src, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		switch {
		case fi.Mode()&os.ModeSymlink != 0:
			t, err := os.Readlink(p)
			if err != nil {
				return err
			}
			return os.Symlink(t, target)
		case fi.IsDir():
			return os.MkdirAll(target, fi.Mode())
		default:
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			if err := os.WriteFile(target, b, fi.Mode()); err != nil {
				return err
			}
			return os.Chtimes(target, fi.ModTime(), fi.ModTime())
		}
	})
}
