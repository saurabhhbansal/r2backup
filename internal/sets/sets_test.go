package sets

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func store(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "sets.json")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	return s, path
}

func add(t *testing.T, s *Store, name, root string) {
	t.Helper()
	if err := s.Add(Set{Name: name, Root: root, Machine: "test-pc"}); err != nil {
		t.Fatal(err)
	}
}

func TestOpenMissingFileGivesEmptyStore(t *testing.T) {
	s, _ := store(t)
	if len(s.List()) != 0 {
		t.Fatal("a store with no file should start empty, not error")
	}
}

func TestAddAppliesDefaults(t *testing.T) {
	s, _ := store(t)
	add(t, s, "Code Projects", "/data/code")

	got, err := s.Get("Code Projects")
	if err != nil {
		t.Fatal(err)
	}
	if got.RetentionDays != DefaultRetentionDays {
		t.Errorf("RetentionDays = %d, want %d", got.RetentionDays, DefaultRetentionDays)
	}
	if got.IntervalMinutes != DefaultIntervalMinutes {
		t.Errorf("IntervalMinutes = %d, want %d", got.IntervalMinutes, DefaultIntervalMinutes)
	}
	if got.Status != StatusOK {
		t.Errorf("Status = %q, want %q", got.Status, StatusOK)
	}
	if got.Prefix != "Code Projects" {
		t.Errorf("Prefix = %q, want it derived from the name at creation", got.Prefix)
	}
	if got.CreatedAt.IsZero() {
		t.Error("CreatedAt was not set")
	}
}

func TestRenameLeavesThePrefixAlone(t *testing.T) {
	// The whole point: renaming is free because it does not touch the bucket.
	// If this ever starts moving the prefix, a rename silently costs one
	// operation per object.
	s, _ := store(t)
	add(t, s, "Code Projects", "/data/code")

	if err := s.Rename("Code Projects", "Projects"); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get("Projects")
	if err != nil {
		t.Fatalf("the set is not reachable under its new name: %v", err)
	}
	if got.Prefix != "Code Projects" {
		t.Fatalf("Prefix = %q, want it unchanged at %q -- moving it would cost one operation per object",
			got.Prefix, "Code Projects")
	}
	if _, err := s.Get("Code Projects"); !errors.Is(err, ErrNotFound) {
		t.Error("the old name should no longer resolve")
	}
}

func TestRenameOntoAnExistingNameIsRefused(t *testing.T) {
	s, _ := store(t)
	add(t, s, "one", "/a")
	add(t, s, "two", "/b")
	if err := s.Rename("one", "two"); !errors.Is(err, ErrExists) {
		t.Fatalf("got %v, want ErrExists", err)
	}
	// And nothing was mangled in the attempt.
	if _, err := s.Get("one"); err != nil {
		t.Error("the failed rename damaged the original set")
	}
}

func TestAddRejectsDuplicates(t *testing.T) {
	s, _ := store(t)
	add(t, s, "one", "/a")
	if err := s.Add(Set{Name: "one", Root: "/other"}); !errors.Is(err, ErrExists) {
		t.Fatalf("got %v, want ErrExists", err)
	}
}

func TestRelinkPointsAtANewRootWithoutTouchingThePrefix(t *testing.T) {
	s, _ := store(t)
	oldRoot := t.TempDir()
	add(t, s, "Code Projects", oldRoot)

	newRoot := t.TempDir()
	if err := s.Relink("Code Projects", newRoot); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Get("Code Projects")
	if got.Root != newRoot {
		t.Errorf("Root = %q, want %q", got.Root, newRoot)
	}
	if got.Prefix != "Code Projects" {
		t.Errorf("relink must not move the bucket prefix; got %q", got.Prefix)
	}
	if got.Status != StatusOK {
		t.Errorf("relink should clear needs-attention, got %q", got.Status)
	}
}

func TestRelinkRefusesAPathThatIsNotThere(t *testing.T) {
	s, _ := store(t)
	add(t, s, "one", t.TempDir())
	if err := s.Relink("one", filepath.Join(t.TempDir(), "nope")); err == nil {
		t.Fatal("relinking to a non-existent directory should fail loudly")
	}
}

func TestMarkNeedsAttentionParksTheSet(t *testing.T) {
	s, _ := store(t)
	add(t, s, "one", "/a")
	const note = "root D:\\Code Projects no longer exists"
	if err := s.MarkNeedsAttention("one", note); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Get("one")
	if got.Status != StatusNeedsAttention {
		t.Errorf("Status = %q, want %q", got.Status, StatusNeedsAttention)
	}
	if got.StatusNote != note {
		t.Errorf("StatusNote = %q, want the reason recorded", got.StatusNote)
	}
}

func TestRemoveDoesNotClaimToTouchTheBucket(t *testing.T) {
	s, _ := store(t)
	add(t, s, "one", "/a")
	add(t, s, "two", "/b")
	if err := s.Remove("one"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get("one"); !errors.Is(err, ErrNotFound) {
		t.Error("removed set is still listed")
	}
	if _, err := s.Get("two"); err != nil {
		t.Error("removing one set disturbed another")
	}
}

func TestChangesSurviveReopening(t *testing.T) {
	s, path := store(t)
	add(t, s, "Code Projects", "/data/code")
	if err := s.Rename("Code Projects", "Projects"); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	got, err := reopened.Get("Projects")
	if err != nil {
		t.Fatal(err)
	}
	if got.Prefix != "Code Projects" {
		t.Errorf("Prefix did not survive a reopen: %q", got.Prefix)
	}
}

func TestSaveIsAtomic(t *testing.T) {
	// After a save there must be exactly one file: no .tmp left behind for a
	// later reader to trip over.
	s, path := store(t)
	add(t, s, "one", "/a")

	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("expected only sets.json, found %v", names)
	}
}

func TestExcludedMatchesDirectoriesAndTheirContents(t *testing.T) {
	s := Set{Excludes: []string{"node_modules", "build/cache"}}
	cases := map[string]bool{
		"node_modules":                true,
		"node_modules/react/index.js": true,
		"build/cache":                 true,
		"build/cache/deep/thing.o":    true,
		"build/output.js":             false,
		"src/index.ts":                false,
		"node_modules_but_not_really": false, // prefix must not match loosely
		"my_node_modules/x":           false,
	}
	for key, want := range cases {
		if got := s.Excluded(key); got != want {
			t.Errorf("Excluded(%q) = %v, want %v", key, got, want)
		}
	}
}

func TestTrashEnabledFollowsRetention(t *testing.T) {
	if (&Set{RetentionDays: 30}).TrashEnabled() != true {
		t.Error("30 days of retention means trash is on")
	}
	if (&Set{RetentionDays: 0}).TrashEnabled() != false {
		t.Error("zero retention means trash is off")
	}
}

func TestGetAndUpdateOnAMissingSetReportNotFound(t *testing.T) {
	s, _ := store(t)
	if _, err := s.Get("ghost"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get: got %v, want ErrNotFound", err)
	}
	if err := s.Update(Set{Name: "ghost"}); !errors.Is(err, ErrNotFound) {
		t.Errorf("Update: got %v, want ErrNotFound", err)
	}
	if err := s.Remove("ghost"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Remove: got %v, want ErrNotFound", err)
	}
	if err := s.MarkNeedsAttention("ghost", "x"); !errors.Is(err, ErrNotFound) {
		t.Errorf("MarkNeedsAttention: got %v, want ErrNotFound", err)
	}
}

func TestListIsSortedAndACopy(t *testing.T) {
	s, _ := store(t)
	add(t, s, "zebra", "/z")
	add(t, s, "alpha", "/a")
	add(t, s, "mango", "/m")

	list := s.List()
	if list[0].Name != "alpha" || list[2].Name != "zebra" {
		t.Fatalf("List is not sorted: %v", []string{list[0].Name, list[1].Name, list[2].Name})
	}
	list[0].Name = "mutated"
	if again := s.List(); again[0].Name != "alpha" {
		t.Error("List handed out a reference; a caller mutated the store")
	}
}

func TestOverlappingFindsANestedRootInEitherDirection(t *testing.T) {
	s, _ := store(t)
	base := t.TempDir()
	docs := filepath.Join(base, "docs")
	add(t, s, "Docs", docs)

	cases := []struct {
		what string
		root string
		want bool
	}{
		{"the very same folder", docs, true},
		{"a folder inside the set", filepath.Join(docs, "invoices"), true},
		{"a folder deep inside the set", filepath.Join(docs, "invoices", "2026", "q3"), true},
		{"a folder that contains the set", base, true},
		{"an unrelated sibling", filepath.Join(base, "music"), false},
		// The one a naive strings.HasPrefix gets wrong: "docs-old" starts
		// with "docs" as text but is not inside it as a path.
		{"a sibling whose name starts with the set's", docs + "-old", false},
		{"a sibling one character longer", docs + "2", false},
	}
	for _, tc := range cases {
		got, ok := s.Overlapping(tc.root)
		if ok != tc.want {
			t.Errorf("Overlapping(%s) = %v, want %v", tc.what, ok, tc.want)
			continue
		}
		if ok && got.Name != "Docs" {
			t.Errorf("Overlapping(%s) named %q, want \"Docs\"", tc.what, got.Name)
		}
	}
}

func TestOverlappingIsQuietWhenThereAreNoSets(t *testing.T) {
	s, _ := store(t)
	if _, ok := s.Overlapping(t.TempDir()); ok {
		t.Error("Overlapping reported an overlap against an empty store")
	}
}

func TestValidNameRefusesWhatWouldBreakTheKey(t *testing.T) {
	// The name becomes part of every object key for the set, so these are
	// not style rules. "../escape" was accepted before this existed: the set
	// was created, `add` said "Added" and exited 0, and every upload for the
	// rest of its life failed with XMinioInvalidResourceName.
	bad := []struct {
		name string
		why  string
	}{
		{"", "empty"},
		{"   ", "only whitespace"},
		{"..", "a path component"},
		{".", "a path component"},
		{"../escape", "escapes the machine prefix"},
		{"a/b", "silently becomes two prefix levels"},
		{`a\b`, "a Windows separator does the same"},
		{"trailing ", "a trailing space Windows would strip"},
		{" leading", "a leading space"},
		{"bell\x07", "a control character"},
		{strings.Repeat("x", MaxNameLen+1), "longer than the key budget allows"},
	}
	for _, tc := range bad {
		if err := ValidName(tc.name); err == nil {
			t.Errorf("ValidName(%q) allowed it, but it is %s", tc.name, tc.why)
		} else if !errors.Is(err, ErrBadName) {
			t.Errorf("ValidName(%q) = %v, which does not wrap ErrBadName", tc.name, err)
		}
	}

	for _, name := range []string{
		"Documents",
		"Code Projects",
		"photos-2026",
		"réservé",
		"emoji 🎉 set",
		"a.b.c",
		"..leading dots are fine mid-name",
		strings.Repeat("x", MaxNameLen),
	} {
		if err := ValidName(name); err != nil {
			t.Errorf("ValidName(%q) refused an ordinary name: %v", name, err)
		}
	}
}

func TestAddAndRenameBothRefuseABadName(t *testing.T) {
	// Enforced in the store, not only in the CLI, so no caller can route
	// around it.
	s, _ := store(t)
	if err := s.Add(Set{Name: "../escape", Root: t.TempDir(), Machine: "test-pc"}); err == nil {
		t.Error("Add accepted a name that would break every key for the set")
	}
	add(t, s, "Docs", t.TempDir())
	if err := s.Rename("Docs", "../escape"); err == nil {
		t.Error("Rename accepted a name Add would have refused")
	}
	if _, err := s.Get("Docs"); err != nil {
		t.Errorf("a refused rename lost the set: %v", err)
	}
}

func TestAddKeepsAnExplicitlyDisabledRetention(t *testing.T) {
	// The two must not collapse into each other. Omitting retention has to
	// keep trash on, because a silently unrecoverable delete is the worse
	// mistake -- but `add --retention 0` has to mean what it says, and it
	// did not: the 0 was read as "unset" and became 30, so a user who asked
	// for no trash paid to keep 30 days of it.
	s, _ := store(t)

	if err := s.Add(Set{Name: "Quiet", Root: "/data/quiet", Machine: "test-pc", RetentionDays: RetentionDisabled}); err != nil {
		t.Fatal(err)
	}
	if err := s.Add(Set{Name: "Ordinary", Root: "/data/ordinary", Machine: "test-pc"}); err != nil {
		t.Fatal(err)
	}

	quiet, err := s.Get("Quiet")
	if err != nil {
		t.Fatal(err)
	}
	if quiet.RetentionDays != RetentionDisabled {
		t.Errorf("RetentionDays = %d, want %d -- an explicit disable was overwritten", quiet.RetentionDays, RetentionDisabled)
	}
	if quiet.TrashEnabled() {
		t.Error("TrashEnabled() is true for a set that asked for no trash")
	}

	ordinary, err := s.Get("Ordinary")
	if err != nil {
		t.Fatal(err)
	}
	if ordinary.RetentionDays != DefaultRetentionDays {
		t.Errorf("RetentionDays = %d, want %d -- omitting retention must not disable trash", ordinary.RetentionDays, DefaultRetentionDays)
	}
	if !ordinary.TrashEnabled() {
		t.Error("TrashEnabled() is false for a set that never asked to disable it")
	}

	// And it survives a reload, since this is what gets written to disk.
	reopened, err := Open(filepath.Join(filepath.Dir(s.path), "sets.json"))
	if err != nil {
		t.Fatal(err)
	}
	again, err := reopened.Get("Quiet")
	if err != nil {
		t.Fatal(err)
	}
	if again.TrashEnabled() {
		t.Error("the disabled retention did not survive a reload")
	}
}
