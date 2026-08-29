package restore

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestMatchesOnly(t *testing.T) {
	cases := []struct {
		pattern, relPath string
		want             bool
	}{
		{"", "anything/at/all.txt", true},
		{"docs", "docs/readme.md", true},
		{"docs", "docs", true},
		{"docs", "documents/readme.md", false},
		{"docs/**", "docs/a/b/c.txt", true},
		{"docs/**", "other/c.txt", false},
		{"*.txt", "file.txt", true},
		{"*.txt", "dir/file.txt", false}, // path.Match's "*" does not cross "/"
		{"a/*.go", "a/b.go", true},
		{"a/*.go", "a/b/c.go", false},
	}
	for _, tc := range cases {
		if got := matchesOnly(tc.pattern, tc.relPath); got != tc.want {
			t.Errorf("matchesOnly(%q, %q) = %v, want %v", tc.pattern, tc.relPath, got, tc.want)
		}
	}
}

func TestRunRestoresEveryKind(t *testing.T) {
	backend := newFakeBackend()
	backend.put("proj/current/hello.txt", []byte("hello world"), fileMeta())
	backend.put("proj/current/link", []byte("hello.txt"), symlinkMeta("hello.txt"))
	backend.put("proj/current/empty/dir", nil, dirMeta())

	target := t.TempDir()
	rep, err := Run(context.Background(), Options{
		Set:    testSet("proj", target),
		Client: backend,
		Target: target,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !rep.Succeeded() {
		t.Fatalf("Failures: %v", rep.Failures)
	}
	if rep.Downloaded != 3 {
		t.Errorf("Downloaded = %d, want 3", rep.Downloaded)
	}

	data, err := os.ReadFile(filepath.Join(target, "hello.txt"))
	if err != nil || string(data) != "hello world" {
		t.Errorf("hello.txt = %q, %v; want \"hello world\", nil", data, err)
	}

	link, err := os.Readlink(filepath.Join(target, "link"))
	if err != nil || link != "hello.txt" {
		t.Errorf("link target = %q, %v; want \"hello.txt\", nil", link, err)
	}
	if info, err := os.Lstat(filepath.Join(target, "link")); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Errorf("link was not restored as a symlink: %v, %v", info, err)
	}

	info, err := os.Stat(filepath.Join(target, "empty", "dir"))
	if err != nil || !info.IsDir() {
		t.Errorf("empty/dir was not restored as a directory: %v, %v", info, err)
	}
	entries, err := os.ReadDir(filepath.Join(target, "empty", "dir"))
	if err != nil || len(entries) != 0 {
		t.Errorf("empty/dir is not empty: %v, %v", entries, err)
	}
}

func TestOverwriteFalseSkipsAnExistingFile(t *testing.T) {
	backend := newFakeBackend()
	backend.put("proj/current/hello.txt", []byte("from the bucket"), fileMeta())

	target := t.TempDir()
	if err := os.WriteFile(filepath.Join(target, "hello.txt"), []byte("already here"), 0o644); err != nil {
		t.Fatal(err)
	}

	rep, err := Run(context.Background(), Options{
		Set:    testSet("proj", target),
		Client: backend,
		Target: target,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.Downloaded != 0 {
		t.Errorf("Downloaded = %d, want 0 -- overwrite is off", rep.Downloaded)
	}
	if rep.SkippedExisting != 1 || len(rep.Skipped) != 1 || rep.Skipped[0] != "hello.txt" {
		t.Errorf("Skipped = %v, SkippedExisting = %d; want exactly [\"hello.txt\"]", rep.Skipped, rep.SkippedExisting)
	}
	data, err := os.ReadFile(filepath.Join(target, "hello.txt"))
	if err != nil || string(data) != "already here" {
		t.Errorf("the existing file was modified: %q, %v", data, err)
	}
}

func TestOverwriteTrueReplacesAnExistingFile(t *testing.T) {
	backend := newFakeBackend()
	backend.put("proj/current/hello.txt", []byte("from the bucket"), fileMeta())

	target := t.TempDir()
	if err := os.WriteFile(filepath.Join(target, "hello.txt"), []byte("already here"), 0o644); err != nil {
		t.Fatal(err)
	}

	rep, err := Run(context.Background(), Options{
		Set:       testSet("proj", target),
		Client:    backend,
		Target:    target,
		Overwrite: true,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.Downloaded != 1 {
		t.Errorf("Downloaded = %d, want 1 -- overwrite is on", rep.Downloaded)
	}
	if rep.SkippedExisting != 0 {
		t.Errorf("SkippedExisting = %d, want 0", rep.SkippedExisting)
	}
	data, err := os.ReadFile(filepath.Join(target, "hello.txt"))
	if err != nil || string(data) != "from the bucket" {
		t.Errorf("hello.txt = %q, %v; want the bucket's content", data, err)
	}
}

func TestOnlyRestoresJustTheMatchingSubset(t *testing.T) {
	backend := newFakeBackend()
	backend.put("proj/current/docs/a.txt", []byte("a"), fileMeta())
	backend.put("proj/current/docs/b.txt", []byte("b"), fileMeta())
	backend.put("proj/current/src/main.go", []byte("package main"), fileMeta())

	target := t.TempDir()
	rep, err := Run(context.Background(), Options{
		Set:    testSet("proj", target),
		Client: backend,
		Target: target,
		Only:   "docs",
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.Downloaded != 2 {
		t.Errorf("Downloaded = %d, want 2", rep.Downloaded)
	}
	if _, err := os.Stat(filepath.Join(target, "src", "main.go")); err == nil {
		t.Error("src/main.go was restored despite --only docs")
	}
	if _, err := os.Stat(filepath.Join(target, "docs", "a.txt")); err != nil {
		t.Errorf("docs/a.txt was not restored: %v", err)
	}
}

func TestRestoringAnEmptySetReportsEmptyRatherThanErroring(t *testing.T) {
	backend := newFakeBackend()
	target := t.TempDir()

	rep, err := Run(context.Background(), Options{
		Set:    testSet("proj", target),
		Client: backend,
		Target: target,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.Downloaded != 0 || rep.ListedFiles != 0 || !rep.Succeeded() {
		t.Errorf("an empty set produced a non-empty report: %+v", rep)
	}
}

func TestTargetMustBeCreatedIsCreated(t *testing.T) {
	backend := newFakeBackend()
	backend.put("proj/current/a.txt", []byte("a"), fileMeta())

	parent := t.TempDir()
	target := filepath.Join(parent, "does", "not", "exist", "yet")

	rep, err := Run(context.Background(), Options{
		Set:    testSet("proj", "/nonexistent-root-for-this-test"),
		Client: backend,
		Target: target,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.Downloaded != 1 {
		t.Errorf("Downloaded = %d, want 1", rep.Downloaded)
	}
	if info, err := os.Stat(target); err != nil || !info.IsDir() {
		t.Errorf("target directory was not created: %v, %v", info, err)
	}
}

func TestAnUnwritableTargetErrorsClearly(t *testing.T) {
	parent := t.TempDir()
	if err := os.Chmod(parent, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(parent, 0o755) }) // let TempDir clean up afterward

	target := filepath.Join(parent, "sub")
	backend := newFakeBackend()

	_, err := Run(context.Background(), Options{
		Set:    testSet("proj", "/nonexistent-root-for-this-test"),
		Client: backend,
		Target: target,
	})
	if err == nil {
		t.Fatal("Run succeeded against an unwritable target")
	}
}

func TestNoTargetAndNoOriginalRootErrors(t *testing.T) {
	backend := newFakeBackend()
	_, err := Run(context.Background(), Options{
		Set:    testSet("proj", "/definitely/does/not/exist/anywhere"),
		Client: backend,
	})
	if err == nil {
		t.Fatal("Run succeeded with no usable target")
	}
}
