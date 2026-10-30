package cli

import (
	"testing"

	"github.com/saurabhhbansal/r2backup/internal/sets"
)

func TestEntryStateOfLeftOutThings(t *testing.T) {
	s := sets.Set{Excludes: []string{"node_modules", "build/cache", "notes/draft.txt"}}
	cases := []struct {
		path      string
		dir       bool
		state     string
		inherited string
	}{
		{"src", true, "on", ""},
		{"node_modules", true, "off", ""},
		{"node_modules/left-pad", true, "off", "node_modules"},
		{"build", true, "partial", ""},
		{"build/cache", true, "off", ""},
		{"build/cache/x.tmp", false, "off", "build/cache"},
		{"build/out.bin", false, "on", ""},
		{"notes", true, "partial", ""},
		{"notes/draft.txt", false, "off", ""},
		{"notes/keep.txt", false, "on", ""},
		{"node_modules2", true, "on", ""}, // a name that merely starts the same
	}
	for _, c := range cases {
		st, inh := entryState(s, c.path, c.dir)
		if st != c.state || inh != c.inherited {
			t.Errorf("%s: got (%s, %q), want (%s, %q)", c.path, st, inh, c.state, c.inherited)
		}
	}
}
