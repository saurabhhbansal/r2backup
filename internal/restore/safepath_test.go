package restore

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSafeJoinRefusesEveryTraversalShape is the core security test for this
// package: an object key must never be allowed to resolve outside the
// restore target, no matter how it tries to escape.
func TestSafeJoinRefusesEveryTraversalShape(t *testing.T) {
	target := t.TempDir()

	malicious := []string{
		"../../etc/passwd",
		"../secret.txt",
		"/etc/passwd",
		"..\\..\\windows\\system32\\config",
		"a/../../b",
		"C:\\Windows\\System32\\evil.dll",
		"c:/windows/system32",
		"..",
		"a/../../../etc/shadow",
		"",
	}
	for _, key := range malicious {
		got, err := safeJoin(target, key)
		if err == nil {
			t.Errorf("safeJoin(%q) = %q, nil; want an error -- this key must be refused", key, got)
		}
		if got != "" {
			t.Errorf("safeJoin(%q) returned a non-empty path %q alongside an error", key, got)
		}
	}
}

// TestSafeJoinAcceptsOrdinaryPaths makes sure the guard above isn't so
// aggressive it also rejects the paths a real restore needs to write --
// including ones that legitimately contain "..' as a literal filename
// substring, not a traversal.
func TestSafeJoinAcceptsOrdinaryPaths(t *testing.T) {
	target := t.TempDir()

	ok := []string{
		"file.txt",
		"a/b/c/file.txt",
		"unicode/résumé.txt",
		"awkward/dot..dot.txt",
		"a file with spaces.txt",
		"emoji-🎉-party.txt",
	}
	for _, key := range ok {
		got, err := safeJoin(target, key)
		if err != nil {
			t.Errorf("safeJoin(%q) returned an error for a legitimate path: %v", key, err)
			continue
		}
		rel, err := filepath.Rel(target, got)
		if err != nil || strings.HasPrefix(rel, "..") {
			t.Errorf("safeJoin(%q) = %q, which is not under %q", key, got, target)
		}
	}
}

// TestPathTraversalNeverWritesOutsideTarget drives a full restore through
// a bucket seeded with keys designed to escape the target directory, and
// asserts that nothing whatsoever landed outside it.
func TestPathTraversalNeverWritesOutsideTarget(t *testing.T) {
	backend := newFakeBackend()
	backend.put("set/current/good.txt", []byte("fine"), fileMeta())
	backend.put("set/current/../../evil.txt", []byte("escaped"), fileMeta())
	backend.put("set/current/..\\..\\also-evil.txt", []byte("escaped2"), fileMeta())

	outer := t.TempDir()
	target := filepath.Join(outer, "target")

	rep, err := Run(t.Context(), Options{
		Set:    testSet("set", target),
		Client: backend,
		Target: target,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(rep.Failures) == 0 {
		t.Error("the malicious keys were not reported as failures")
	}
	if _, err := os.Stat(filepath.Join(target, "good.txt")); err != nil {
		t.Errorf("the legitimate file was not restored: %v", err)
	}

	// Nothing besides "target" itself may exist directly under outer.
	entries, err := os.ReadDir(outer)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "target" {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("something landed outside the restore target: %v", names)
	}
}
