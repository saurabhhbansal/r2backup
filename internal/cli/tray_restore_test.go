package cli

import (
	"testing"
	"time"
)

func TestBuildTreeAndBrowse(t *testing.T) {
	keys := []string{"a.txt", "docs/b.txt", "docs/sub/c.txt", "docs/sub/d.txt"}
	sizes := []int64{1, 10, 100, 1000}
	mods := make([]time.Time, len(keys))
	root := buildTree(keys, sizes, mods)

	if root.files != 4 || root.size != 1111 {
		t.Fatalf("root = %d files, %d bytes; want 4, 1111", root.files, root.size)
	}
	docs := root.at("docs")
	if docs == nil || docs.files != 3 || docs.size != 1110 {
		t.Fatalf("docs = %+v; want 3 files, 1110 bytes", docs)
	}
	if sub := root.at("docs/sub/"); sub == nil || len(sub.kids) != 2 {
		t.Fatalf("docs/sub = %+v; want two files", sub)
	}
	if root.at("a.txt") != nil {
		t.Error("a file is not a folder to open")
	}
	if root.at("nope") != nil {
		t.Error("a missing folder must not resolve")
	}
}

func TestOnlyPatternMeansExactlyWhatWasPicked(t *testing.T) {
	cases := []struct {
		path string
		dir  bool
		want string
	}{
		{"docs", true, "docs/**"},
		{"docs/", true, "docs/**"},
		{"a.txt", false, "a.txt"},
		{"report[1].txt", false, `report\[1].txt`},
		{"what*.txt", false, `what\*.txt`},
	}
	for _, c := range cases {
		if got := onlyPattern(c.path, c.dir); got != c.want {
			t.Errorf("onlyPattern(%q, %v) = %q, want %q", c.path, c.dir, got, c.want)
		}
	}
}
