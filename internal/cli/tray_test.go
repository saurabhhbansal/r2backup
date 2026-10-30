package cli

import (
	"testing"

	"github.com/saurabhhbansal/r2backup/internal/sets"
	"github.com/saurabhhbansal/r2backup/internal/ui"
)

// While a run holds the index the taskbar panel shows the folders from its
// last load. A folder added from the panel starts a run at once, so it was
// missing from the list for exactly as long as it was being backed up.
func TestAFolderAddedDuringARunIsListedAtOnce(t *testing.T) {
	loaded := []ui.SetView{
		{Name: "Code", Root: "/code", State: "ok", HasRun: true, Objects: 12},
		{Name: "Gone", Root: "/gone", State: "ok", HasRun: true},
	}
	now := []sets.Set{
		{Name: "Code", Root: "/code"},
		{Name: "Photos", Root: "/photos"},
	}
	got := withCurrentSets(loaded, now)
	if len(got) != 2 {
		t.Fatalf("got %d folders, want Code and Photos: %+v", len(got), got)
	}
	if got[0].Name != "Code" || got[0].Objects != 12 || !got[0].HasRun {
		t.Errorf("a folder already loaded lost what was known about it: %+v", got[0])
	}
	if got[1].Name != "Photos" || got[1].Root != "/photos" || got[1].HasRun || got[1].State != "never run" {
		t.Errorf("the new folder should appear as not yet run: %+v", got[1])
	}
}
