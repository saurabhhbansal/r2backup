package scan

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func walk(t *testing.T, root string, skip func(string, bool) bool) *Result {
	t.Helper()
	res, err := Walk(context.Background(), Options{Root: root, Skip: skip})
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	return res
}

func keys(res *Result) map[string]Entry {
	m := make(map[string]Entry, len(res.Entries))
	for _, e := range res.Entries {
		m[e.Key] = e
	}
	return m
}

func TestWalkFindsFilesWithSizesAndKeys(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "a.txt"), "hello")
	write(t, filepath.Join(root, "sub", "b.txt"), "worldly")
	write(t, filepath.Join(root, "sub", "deep", "c.bin"), "1234567890")

	res := walk(t, root, nil)
	got := keys(res)

	for key, wantSize := range map[string]int64{
		"a.txt":          5,
		"sub/b.txt":      7,
		"sub/deep/c.bin": 10,
	} {
		e, ok := got[key]
		if !ok {
			t.Errorf("missing entry %q; found %v", key, res.Entries)
			continue
		}
		if e.Size != wantSize {
			t.Errorf("%q size = %d, want %d", key, e.Size, wantSize)
		}
		if e.Kind != KindFile {
			t.Errorf("%q kind = %v, want file", key, e.Kind)
		}
	}
	if res.Files != 3 {
		t.Errorf("Files = %d, want 3", res.Files)
	}
	if res.Bytes != 22 {
		t.Errorf("Bytes = %d, want 22", res.Bytes)
	}
}

func TestWalkCountsZeroByteFiles(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "empty.txt"), "")

	res := walk(t, root, nil)
	if res.Files != 1 {
		t.Fatalf("Files = %d, want 1 -- a zero-byte file is still a file", res.Files)
	}
	if res.Bytes != 0 {
		t.Errorf("Bytes = %d, want 0", res.Bytes)
	}
}

func TestWalkRecordsEmptyDirectories(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "empty"), 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(root, "full", "x.txt"), "x")

	got := keys(walk(t, root, nil))
	e, ok := got["empty"]
	if !ok {
		t.Fatal("empty directory was not recorded; it would vanish on restore")
	}
	if e.Kind != KindEmptyDir {
		t.Errorf("kind = %v, want empty-dir", e.Kind)
	}
	if _, ok := got["full"]; ok {
		t.Error("a directory with contents should not get a marker")
	}
}

func TestWalkStoresSymlinksWithoutFollowingThem(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("creating symlinks on Windows needs Developer Mode or elevation")
	}
	root := t.TempDir()
	write(t, filepath.Join(root, "real", "payload.txt"), "0123456789")
	if err := os.Symlink(filepath.Join(root, "real"), filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}

	res := walk(t, root, nil)
	got := keys(res)

	e, ok := got["link"]
	if !ok {
		t.Fatal("symlink was skipped entirely -- a synced folder would arrive incomplete")
	}
	if e.Kind != KindSymlink {
		t.Errorf("kind = %v, want symlink", e.Kind)
	}
	if e.Target == "" {
		t.Error("symlink target was not recorded")
	}
	if _, followed := got["link/payload.txt"]; followed {
		t.Error("the symlink was followed; its target's bytes would be duplicated")
	}
	if res.Symlinks != 1 {
		t.Errorf("Symlinks = %d, want 1", res.Symlinks)
	}
	if res.Bytes != 10 {
		t.Errorf("Bytes = %d, want 10 -- a symlink must not add its target's size", res.Bytes)
	}
}

func TestWalkSkipRulesPruneDirectories(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "src", "index.ts"), "x")
	write(t, filepath.Join(root, "node_modules", "react", "index.js"), "yy")
	write(t, filepath.Join(root, "node_modules", "vue", "index.js"), "zzz")

	res := walk(t, root, func(key string, isDir bool) bool {
		return key == "node_modules"
	})
	got := keys(res)

	if _, ok := got["src/index.ts"]; !ok {
		t.Error("included file was dropped")
	}
	for k := range got {
		if len(k) >= 12 && k[:12] == "node_modules" {
			t.Errorf("skipped directory was descended into: %q", k)
		}
	}
	if res.Files != 1 {
		t.Errorf("Files = %d, want 1", res.Files)
	}
}

func TestWalkNormalizesNFDNamesOnDisk(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, nfdName), "x")

	got := keys(walk(t, root, nil))
	if _, ok := got[nfcName]; !ok {
		var found []string
		for k := range got {
			found = append(found, k)
		}
		t.Fatalf("a file written with an NFD name did not scan as NFC.\n got %q\nwant %q", found, nfcName)
	}
}

func TestWalkReportsUnreadableDirectoriesWithoutAborting(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("chmod 000 does not deny directory listing on Windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("running as root; permission bits do not apply")
	}
	root := t.TempDir()
	write(t, filepath.Join(root, "readable.txt"), "fine")
	locked := filepath.Join(root, "locked")
	write(t, filepath.Join(locked, "secret.txt"), "nope")
	if err := os.Chmod(locked, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })

	res := walk(t, root, nil)

	if _, ok := keys(res)["readable.txt"]; !ok {
		t.Error("an unreadable directory aborted the rest of the scan")
	}
	if len(res.Problems) == 0 {
		t.Error("the unreadable directory was skipped silently; it must be reported")
	}
}

func TestWalkMissingRootIsItsOwnError(t *testing.T) {
	_, err := Walk(context.Background(), Options{Root: filepath.Join(t.TempDir(), "gone")})
	if err != ErrRootMissing {
		t.Fatalf("got %v, want ErrRootMissing.\nReading a missing root as anything else risks treating a renamed folder as a mass deletion.", err)
	}
}

func TestWalkPreservesModTime(t *testing.T) {
	root := t.TempDir()
	p := filepath.Join(root, "dated.txt")
	write(t, p, "x")
	want := time.Date(2020, 3, 4, 5, 6, 7, 0, time.UTC)
	if err := os.Chtimes(p, want, want); err != nil {
		t.Fatal(err)
	}

	e := keys(walk(t, root, nil))["dated.txt"]
	if !e.ModTime.UTC().Equal(want) {
		t.Errorf("ModTime = %v, want %v", e.ModTime.UTC(), want)
	}
}

func TestWalkIsCancellable(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < 50; i++ {
		write(t, filepath.Join(root, "f", string(rune('a'+i%26))+".txt"), "x")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Walk(ctx, Options{Root: root}); err == nil {
		t.Fatal("a cancelled context should stop the walk")
	}
}

func TestWalkOrderIsDeterministic(t *testing.T) {
	root := t.TempDir()
	for _, n := range []string{"z.txt", "a.txt", "m/n.txt", "b.txt"} {
		write(t, filepath.Join(root, filepath.FromSlash(n)), "x")
	}
	first := walk(t, root, nil)
	second := walk(t, root, nil)
	if len(first.Entries) != len(second.Entries) {
		t.Fatal("entry count differed between two scans of the same tree")
	}
	for i := range first.Entries {
		if first.Entries[i].Key != second.Entries[i].Key {
			t.Fatalf("scan order is not stable at %d: %q vs %q", i, first.Entries[i].Key, second.Entries[i].Key)
		}
	}
}
