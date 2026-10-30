package restore

import (
	"testing"

	"github.com/saurabhhbansal/r2backup/internal/remote"
)

func TestFilterAnyKeepsOnlyWhatWasPicked(t *testing.T) {
	const cp = "machines/pc/docs/current/"
	entries := []remote.ListEntry{
		{Key: cp + "a.txt"}, {Key: cp + "dir/b.txt"}, {Key: cp + "dir/sub/c.txt"},
		{Key: cp + "report[1].txt"}, {Key: cp + "other.txt"},
	}
	got := filterAny(entries, cp, []string{"a.txt", "dir/**", `report\[1\].txt`})
	want := []string{cp + "a.txt", cp + "dir/b.txt", cp + "dir/sub/c.txt", cp + "report[1].txt"}
	if len(got) != len(want) {
		t.Fatalf("kept %d, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i].Key != want[i] {
			t.Errorf("kept[%d] = %s, want %s", i, got[i].Key, want[i])
		}
	}
}
