package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/saurabhhbansal/r2backup/internal/runstate"
)

// `r2b ls <set> [folder]` reads what the backup holds from storage, as the
// panel's Restore does, for any computer's backup.
func TestLsShowsAStoredBackupOneFolderAtATime(t *testing.T) {
	main, _ := parityWorld(t, false)
	main.put("machines/Desk/Photos/current/2024/a.jpg", "machines/Desk/Photos/current/2024/b.jpg",
		"machines/Desk/Photos/current/top.txt")

	out, err := r2b(t, nil, "ls", "Photos", "--machine", "Desk")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Photos: 3 files") || !strings.Contains(out, "2024/  (2 files)") || !strings.Contains(out, "top.txt") {
		t.Fatalf("ls:\n%s", out)
	}
	if strings.Index(out, "2024/") > strings.Index(out, "top.txt") {
		t.Errorf("folders should come first:\n%s", out)
	}
	out, err = r2b(t, nil, "ls", "Photos", "2024", "--machine", "Desk")
	if err != nil || !strings.Contains(out, "a.jpg") || strings.Contains(out, "top.txt") {
		t.Fatalf("ls 2024: %v\n%s", err, out)
	}
	if _, err := r2b(t, nil, "ls", "Photos", "nope", "--machine", "Desk"); err == nil {
		t.Fatal("listed a folder that is not there")
	}
}

// Several --only restore several picked paths, as the panel's selection does.
func TestRestoreTakesSeveralPaths(t *testing.T) {
	main, _ := parityWorld(t, false)
	main.put("machines/Desk/Photos/current/2024/a.jpg", "machines/Desk/Photos/current/2025/b.jpg",
		"machines/Desk/Photos/current/top.txt")
	to := t.TempDir()
	if _, err := r2b(t, nil, "restore", "Photos", "--machine", "Desk", "--to", to, "--only", "2024", "--only", "top.txt"); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"2024/a.jpg", "top.txt"} {
		if _, err := os.Stat(filepath.Join(to, filepath.FromSlash(p))); err != nil {
			t.Errorf("%s not restored: %v", p, err)
		}
	}
	if _, err := os.Stat(filepath.Join(to, "2025")); err == nil {
		t.Error("restored a path that was not picked")
	}
}

// `r2b activity` is the panel's Activity list, and a number its run page.
func TestActivityListsRunsAndShowsOne(t *testing.T) {
	parityWorld(t, false)
	p, err := historyPath()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	for _, r := range []runstate.Past{
		{Set: "Docs", FinishedAt: now.Add(-time.Hour), Uploaded: 3, Bytes: 2048, Unchanged: 10},
		{Set: "Photos", FinishedAt: now, Moved: 2, Deleted: 1, Problems: 1, Examples: []string{"locked.db: in use"}},
	} {
		if err := runstate.Record(p, r); err != nil {
			t.Fatal(err)
		}
	}
	out, err := r2b(t, nil, "activity")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Today") || strings.Index(out, "Photos") > strings.Index(out, "Docs") ||
		!strings.Contains(out, "3 files uploaded") {
		t.Fatalf("activity:\n%s", out)
	}
	out, err = r2b(t, nil, "activity", "1")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Photos: Backed up", "Moved or renamed", "Moved to deleted files", "Could not be read", "locked.db: in use", "The next backup tries these again."} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}
	if _, err := r2b(t, nil, "activity", "9"); err == nil {
		t.Error("showed a run that is not there")
	}
}
