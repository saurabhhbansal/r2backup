package tui

import (
	"testing"
	"time"

	"github.com/saurabhhbansal/r2backup/internal/scan"
)

func TestBuildTree_EverythingStartsChecked(t *testing.T) {
	res := newResult(map[string]int64{
		"a.txt":         10,
		"dir/b.txt":     20,
		"dir/c.txt":     30,
		"dir/sub/d.txt": 40,
	})
	root := BuildTree("/tmp/root", res)

	if root.Check != Checked {
		t.Fatalf("root.Check = %v, want Checked", root.Check)
	}
	if root.TotalFiles != 4 || root.TotalSize != 100 {
		t.Fatalf("root totals = %d files, %d bytes; want 4, 100", root.TotalFiles, root.TotalSize)
	}
	if root.SelectedFiles != root.TotalFiles || root.SelectedSize != root.TotalSize {
		t.Fatalf("selected totals should equal totals when everything is checked: got %d/%d want %d/%d",
			root.SelectedFiles, root.SelectedSize, root.TotalFiles, root.TotalSize)
	}

	// The headline behaviour: press enter with no changes, get nothing excluded.
	excludes := ComputeExcludes(root)
	if len(excludes) != 0 {
		t.Fatalf("ComputeExcludes on an untouched tree = %v, want empty", excludes)
	}
}

func TestComputeExcludes_UncheckSingleFile(t *testing.T) {
	res := newResult(map[string]int64{
		"a.txt":     10,
		"dir/a.txt": 15, // a sibling, so unchecking b.txt leaves dir Partial
		"dir/b.txt": 20,
	})
	root := BuildTree("/tmp/root", res)

	n := findNode(root, "dir/b.txt")
	if n == nil {
		t.Fatal("node dir/b.txt not found")
	}
	Toggle(n)

	got := ComputeExcludes(root)
	want := []string{"dir/b.txt"}
	if len(got) != len(want) || got[0] != want[0] {
		t.Fatalf("ComputeExcludes = %v, want %v", got, want)
	}
}

func TestComputeExcludes_UncheckDirectory_IsMinimal(t *testing.T) {
	res := newResult(manyFiles("big", 500))
	root := BuildTree("/tmp/root", res)

	dir := findNode(root, "big")
	if dir == nil {
		t.Fatal("directory node big not found")
	}
	if dir.TotalFiles != 500 {
		t.Fatalf("big.TotalFiles = %d, want 500", dir.TotalFiles)
	}

	Toggle(dir)

	excludes := ComputeExcludes(root)
	if len(excludes) != 1 {
		t.Fatalf("ComputeExcludes length = %d, want 1 (the naive per-file version would produce 500): %v",
			len(excludes), excludes)
	}
	if excludes[0] != "big" {
		t.Fatalf("ComputeExcludes = %v, want [\"big\"]", excludes)
	}
}

func TestPartialState_SomeChildrenUnchecked(t *testing.T) {
	res := newResult(map[string]int64{
		"dir/a.txt": 1,
		"dir/b.txt": 1,
		"dir/c.txt": 1,
	})
	root := BuildTree("/tmp/root", res)
	dir := findNode(root, "dir")
	a := findNode(root, "dir/a.txt")

	Toggle(a)

	if dir.Check != Partial {
		t.Fatalf("dir.Check = %v, want Partial after unchecking one of three children", dir.Check)
	}
}

func TestUncheckAllChildren_ThenRecheckOne(t *testing.T) {
	res := newResult(map[string]int64{
		"dir/a.txt": 1,
		"dir/b.txt": 1,
	})
	root := BuildTree("/tmp/root", res)
	dir := findNode(root, "dir")
	a := findNode(root, "dir/a.txt")
	b := findNode(root, "dir/b.txt")

	Toggle(a)
	Toggle(b)
	if dir.Check != Unchecked {
		t.Fatalf("dir.Check = %v, want Unchecked once every child is unchecked", dir.Check)
	}

	Toggle(a) // recheck just one
	if dir.Check != Partial {
		t.Fatalf("dir.Check = %v, want Partial after rechecking one of two children", dir.Check)
	}
}

func TestToggleDirectory_SetsDescendantsAndPropagatesToAncestors(t *testing.T) {
	res := newResult(map[string]int64{
		"a/b/c/d.txt": 5,
		"a/b/c/e.txt": 7,
		"a/other.txt": 3,
	})
	root := BuildTree("/tmp/root", res)

	a := findNode(root, "a")
	b := findNode(root, "a/b")
	c := findNode(root, "a/b/c")
	d := findNode(root, "a/b/c/d.txt")
	e := findNode(root, "a/b/c/e.txt")
	other := findNode(root, "a/other.txt")

	Toggle(c) // uncheck the whole a/b/c subtree

	for _, n := range []*Node{c, d, e} {
		if n.Check != Unchecked {
			t.Errorf("node %q Check = %v, want Unchecked after toggling ancestor c", n.Key, n.Check)
		}
	}
	// a/other.txt is untouched.
	if other.Check != Checked {
		t.Errorf("other.Check = %v, want Checked (not under c)", other.Check)
	}
	// b has only one child (c), now fully unchecked -> b itself Unchecked.
	if b.Check != Unchecked {
		t.Errorf("b.Check = %v, want Unchecked", b.Check)
	}
	// a has two children: b (unchecked) and other.txt (checked) -> Partial.
	if a.Check != Partial {
		t.Errorf("a.Check = %v, want Partial", a.Check)
	}
	if root.Check != Partial {
		t.Errorf("root.Check = %v, want Partial", root.Check)
	}

	// Re-check d alone: c and b become Partial again, propagating up through
	// several levels.
	Toggle(d)
	if d.Check != Checked {
		t.Fatalf("d.Check = %v, want Checked", d.Check)
	}
	for _, n := range []*Node{c, b, a, root} {
		if n.Check != Partial {
			t.Errorf("node %q Check = %v, want Partial", n.Key, n.Check)
		}
	}
}

func TestLiveTotals_DropAndReturnOnToggle(t *testing.T) {
	res := newResult(map[string]int64{
		"keep/a.txt":   100,
		"drop/b.txt":   200,
		"drop/c.txt":   300,
		"drop/d/e.txt": 400,
	})
	root := BuildTree("/tmp/root", res)

	wantTotalFiles, wantTotalSize := int64(4), int64(1000)
	if root.TotalFiles != wantTotalFiles || root.TotalSize != wantTotalSize {
		t.Fatalf("root totals = %d/%d, want %d/%d", root.TotalFiles, root.TotalSize, wantTotalFiles, wantTotalSize)
	}

	drop := findNode(root, "drop")
	dropFiles, dropSize := drop.TotalFiles, drop.TotalSize // 3 files, 900 bytes

	Toggle(drop)
	if root.SelectedFiles != wantTotalFiles-dropFiles {
		t.Errorf("SelectedFiles after drop = %d, want %d", root.SelectedFiles, wantTotalFiles-dropFiles)
	}
	if root.SelectedSize != wantTotalSize-dropSize {
		t.Errorf("SelectedSize after drop = %d, want %d", root.SelectedSize, wantTotalSize-dropSize)
	}

	Toggle(drop) // re-check
	if root.SelectedFiles != wantTotalFiles {
		t.Errorf("SelectedFiles after re-check = %d, want %d", root.SelectedFiles, wantTotalFiles)
	}
	if root.SelectedSize != wantTotalSize {
		t.Errorf("SelectedSize after re-check = %d, want %d", root.SelectedSize, wantTotalSize)
	}
}

func TestSetAll(t *testing.T) {
	res := newResult(manyFiles("dir", 20))
	root := BuildTree("/tmp/root", res)

	SetAll(root, Unchecked)
	if root.Check != Unchecked || root.SelectedFiles != 0 || root.SelectedSize != 0 {
		t.Fatalf("after SetAll(Unchecked): Check=%v SelectedFiles=%d SelectedSize=%d", root.Check, root.SelectedFiles, root.SelectedSize)
	}
	excludes := ComputeExcludes(root)
	if len(excludes) != 1 || excludes[0] != "dir" {
		t.Fatalf("ComputeExcludes after uncheck-all = %v, want [\"dir\"]", excludes)
	}

	SetAll(root, Checked)
	if root.Check != Checked || root.SelectedFiles != root.TotalFiles || root.SelectedSize != root.TotalSize {
		t.Fatalf("after SetAll(Checked): Check=%v Selected=%d/%d Total=%d/%d",
			root.Check, root.SelectedFiles, root.SelectedSize, root.TotalFiles, root.TotalSize)
	}
	if len(ComputeExcludes(root)) != 0 {
		t.Fatalf("ComputeExcludes after re-checking everything should be empty")
	}
}

func TestEmptyDirEntry_IsALeafThatCountsAsOneItem(t *testing.T) {
	res := &scan.Result{
		Entries: []scan.Entry{
			{Key: "empty", Kind: scan.KindEmptyDir},
			{Key: "file.txt", Kind: scan.KindFile, Size: 5},
		},
		Dirs: 1,
	}
	root := BuildTree("/tmp/root", res)

	empty := findNode(root, "empty")
	if empty == nil {
		t.Fatal("empty dir node not found")
	}
	if !empty.IsDir {
		t.Error("empty dir node should be IsDir")
	}
	if empty.expandable() {
		t.Error("an empty directory has nothing to expand")
	}
	if empty.TotalFiles != 1 {
		t.Errorf("empty.TotalFiles = %d, want 1", empty.TotalFiles)
	}
	if root.TotalFiles != 2 {
		t.Errorf("root.TotalFiles = %d, want 2", root.TotalFiles)
	}
}

func TestBuildTree_EmptyScan_NoPanic(t *testing.T) {
	root := BuildTree("/tmp/root", &scan.Result{})
	if len(root.Children) != 0 {
		t.Fatalf("expected no children, got %d", len(root.Children))
	}
	if got := ComputeExcludes(root); len(got) != 0 {
		t.Fatalf("ComputeExcludes on empty tree = %v", got)
	}
	if got := VisibleRows(root); len(got) != 0 {
		t.Fatalf("VisibleRows on empty tree = %v", got)
	}
}

func TestBuildTree_SingleFile_NoPanic(t *testing.T) {
	res := newResult(map[string]int64{"only.txt": 42})
	root := BuildTree("/tmp/root", res)
	if len(root.Children) != 1 {
		t.Fatalf("expected 1 child, got %d", len(root.Children))
	}
	n := root.Children[0]
	Toggle(n)
	if len(ComputeExcludes(root)) != 1 {
		t.Fatalf("expected the single file excluded")
	}
	Toggle(n)
	if len(ComputeExcludes(root)) != 0 {
		t.Fatalf("expected nothing excluded once rechecked")
	}
}

// TestVolume_60kEntries builds a 60,000-file tree and performs a full
// top-to-bottom toggle, asserting both stay fast. A quadratic bug in either
// setSubtree or recalcAncestors would make this test take seconds instead of
// milliseconds, which is exactly the failure mode a "500 files, length 1"
// unit test is too small to catch.
func TestVolume_60kEntries(t *testing.T) {
	const n = 60000
	res := newResult(manyFiles("bulk", n))

	start := time.Now()
	root := BuildTree("/tmp/root", res)
	buildTime := time.Since(start)
	t.Logf("BuildTree(%d entries) took %s", n, buildTime)
	if buildTime > 3*time.Second {
		t.Fatalf("BuildTree took %s, want well under 3s", buildTime)
	}

	if root.TotalFiles != n {
		t.Fatalf("root.TotalFiles = %d, want %d", root.TotalFiles, n)
	}

	bulk := findNode(root, "bulk")
	if bulk == nil || bulk.TotalFiles != n {
		t.Fatalf("bulk directory missing or wrong total files")
	}

	start = time.Now()
	Toggle(bulk) // uncheck all 60,000 files in one directory
	toggleTime := time.Since(start)
	t.Logf("Toggle(60k-file dir) took %s", toggleTime)
	if toggleTime > 2*time.Second {
		t.Fatalf("Toggle took %s, want well under 2s", toggleTime)
	}

	if root.SelectedFiles != 0 {
		t.Fatalf("SelectedFiles after unchecking everything = %d, want 0", root.SelectedFiles)
	}
	excludes := ComputeExcludes(root)
	if len(excludes) != 1 || excludes[0] != "bulk" {
		t.Fatalf("ComputeExcludes = %v, want exactly [\"bulk\"]", excludes)
	}

	start = time.Now()
	rows := VisibleRows(root)
	flattenTime := time.Since(start)
	t.Logf("VisibleRows(%d visible) took %s", len(rows), flattenTime)
	if len(rows) != n+1 { // n files + the "bulk" directory row itself
		t.Fatalf("VisibleRows length = %d, want %d", len(rows), n+1)
	}
}
