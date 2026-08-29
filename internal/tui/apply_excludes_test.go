package tui

import (
	"reflect"
	"sort"
	"testing"

	"github.com/saurabhhbansal/r2backup/internal/sets"
)

func tree(t *testing.T) *Node {
	t.Helper()
	return BuildTree("/tmp/root", newResult(map[string]int64{
		"docs/a.txt":             10,
		"docs/b.txt":             20,
		"docs/deep/c.txt":        30,
		"code/main.go":           40,
		"code/node_modules/x.js": 50,
		"code/node_modules/y.js": 60,
		"top.txt":                70,
	}))
}

// ApplyExcludes has to be the exact inverse of ComputeExcludes. If it is not,
// reopening the picker on an existing set shows a selection that is not the
// one actually in force, and whatever the user does next is applied to the
// wrong starting point -- which for an exclude list means silently backing up
// or silently dropping files they did not choose.
func TestApplyExcludesRoundTripsWithComputeExcludes(t *testing.T) {
	for _, want := range [][]string{
		{},
		{"code/node_modules"},
		{"docs/b.txt"},
		{"docs/deep", "code/node_modules"},
		{"top.txt", "docs"},
	} {
		root := tree(t)
		ApplyExcludes(root, want)
		got := ComputeExcludes(root)
		sort.Strings(got)
		w := append([]string(nil), want...)
		sort.Strings(w)
		if len(got) == 0 && len(w) == 0 {
			continue
		}
		if !reflect.DeepEqual(got, w) {
			t.Errorf("ApplyExcludes(%v) then ComputeExcludes = %v, want %v", want, got, w)
		}
	}
}

// The picker and the backup have to agree about what a rule covers, or the
// tree shows one thing and the run does another. sets.Set.Excluded is the
// authority; this asserts the picker matches it file by file.
func TestApplyExcludesAgreesWithTheSetItCameFrom(t *testing.T) {
	excludes := []string{"code/node_modules", "docs/b.txt"}
	s := sets.Set{Excludes: excludes}
	root := tree(t)
	ApplyExcludes(root, excludes)

	for _, key := range []string{
		"docs/a.txt", "docs/b.txt", "docs/deep/c.txt",
		"code/main.go", "code/node_modules/x.js", "top.txt",
	} {
		n := findNode(root, key)
		if n == nil {
			t.Fatalf("%q not in the tree", key)
		}
		checked := n.Check == Checked
		if checked == s.Excluded(key) {
			t.Errorf("%q: picker shows checked=%v but the set says excluded=%v",
				key, checked, s.Excluded(key))
		}
	}
}

// Unchecking a directory has to roll up: its parent goes Partial, and the
// selected totals stop counting what is no longer selected. Getting this
// wrong shows a correct set of ticks under a wrong "N files selected".
func TestApplyExcludesRecomputesTotals(t *testing.T) {
	root := tree(t)
	before := root.SelectedFiles
	ApplyExcludes(root, []string{"code/node_modules"})

	if root.SelectedFiles != before-2 {
		t.Errorf("SelectedFiles = %d, want %d", root.SelectedFiles, before-2)
	}
	if root.SelectedSize != 10+20+30+40+70 {
		t.Errorf("SelectedSize = %d, want %d", root.SelectedSize, 10+20+30+40+70)
	}
	code := findNode(root, "code")
	if code.Check != Partial {
		t.Errorf("code Check = %v, want Partial: some of it is excluded and some is not", code.Check)
	}
	if nm := findNode(root, "code/node_modules"); nm.Check != Unchecked {
		t.Errorf("code/node_modules Check = %v, want Unchecked", nm.Check)
	}
}
