package ui

import (
	"sort"

	"github.com/saurabhhbansal/r2backup/internal/scan"
)

// newResult builds a scan.Result from a name->size map, sorted the way
// scan.Walk emits entries. Same helper internal/tui's tests use, and for the
// same reason: the picker's tree is built from this shape and nothing else.
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
