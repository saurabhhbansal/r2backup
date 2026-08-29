package tui

import (
	"fmt"
	"sort"

	"github.com/saurabhhbansal/r2backup/internal/scan"
)

// newResult builds a scan.Result from a key->size map, sorted the way
// scan.Walk actually returns entries (BuildTree relies on that order to do a
// single pass without a separate sort step).
func newResult(files map[string]int64) *scan.Result {
	r := &scan.Result{}
	for k, sz := range files {
		r.Entries = append(r.Entries, scan.Entry{Key: k, Size: sz, Kind: scan.KindFile})
		r.Files++
		r.Bytes += sz
	}
	sort.Slice(r.Entries, func(i, j int) bool { return r.Entries[i].Key < r.Entries[j].Key })
	return r
}

// manyFiles generates n files ("dir/file00000".."file0000n") of 100 bytes
// each under dir, for volume tests.
func manyFiles(dir string, n int) map[string]int64 {
	out := make(map[string]int64, n)
	for i := 0; i < n; i++ {
		out[fmt.Sprintf("%s/file%06d.txt", dir, i)] = 100
	}
	return out
}

// findNode locates the node with the given key by depth-first search. Only
// used by tests, where a linear scan over a small fixture tree is fine.
func findNode(root *Node, key string) *Node {
	if root.Key == key {
		return root
	}
	for _, c := range root.Children {
		if n := findNode(c, key); n != nil {
			return n
		}
	}
	return nil
}

// countNodes counts every node in the tree, root excluded, for sanity checks
// against the entry count a scan produced.
func countNodes(n *Node) int {
	total := len(n.Children)
	for _, c := range n.Children {
		total += countNodes(c)
	}
	return total
}
