package scan

import (
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// The same name spelled two ways: NFC (single precomposed rune) and NFD (base
// letter plus a combining accent). macOS hands back the second for a file
// created as the first.
const (
	nfcName = "r\u00e9sum\u00e9.txt"   // résumé.txt, precomposed
	nfdName = "re\u0301sume\u0301.txt" // résumé.txt, decomposed
)

func TestKeyNormalizesToNFC(t *testing.T) {
	root := filepath.FromSlash("/data/set")
	for _, name := range []string{nfcName, nfdName} {
		got, err := Key(root, filepath.Join(root, "docs", name))
		if err != nil {
			t.Fatalf("Key(%q) errored: %v", name, err)
		}
		want := "docs/" + nfcName
		if got != want {
			t.Errorf("Key(%q)\n got %q\nwant %q", name, got, want)
		}
	}
}

func TestNFCAndNFDProduceTheSameKey(t *testing.T) {
	root := filepath.FromSlash("/data/set")
	a, err := Key(root, filepath.Join(root, nfcName))
	if err != nil {
		t.Fatal(err)
	}
	b, err := Key(root, filepath.Join(root, nfdName))
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Fatalf("the same filename produced two keys:\n  NFC -> %q\n  NFD -> %q\nthis is the bug that made every sync re-upload the file", a, b)
	}
}

func TestKeyUsesForwardSlashes(t *testing.T) {
	root := filepath.FromSlash("/data/set")
	got, err := Key(root, filepath.Join(root, "a", "b", "c.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if got != "a/b/c.txt" {
		t.Errorf("got %q, want a/b/c.txt", got)
	}
	if strings.Contains(got, `\`) {
		t.Errorf("key %q still contains a backslash", got)
	}
}

func TestKeyOfRootIsEmpty(t *testing.T) {
	root := filepath.FromSlash("/data/set")
	got, err := Key(root, root)
	if err != nil {
		t.Fatal(err)
	}
	if got != "" {
		t.Errorf("got %q, want empty string for the root itself", got)
	}
}

func TestKeyRejectsPathsOutsideRoot(t *testing.T) {
	root := filepath.FromSlash("/data/set")
	for _, p := range []string{
		filepath.FromSlash("/data/other/x.txt"),
		filepath.FromSlash("/data"),
	} {
		if _, err := Key(root, p); err == nil {
			t.Errorf("Key(%q) should have refused a path outside the root", p)
		}
	}
}

func TestKeyKeepsAwkwardButLegalNames(t *testing.T) {
	root := filepath.FromSlash("/data/set")
	for _, name := range []string{
		"a file with spaces.txt",
		"hash#and?question.txt",
		"100% done.md",
		"emoji-🎉.txt",
		"trailing space .txt",
		"dot.in.the.middle.tar.gz",
	} {
		got, err := Key(root, filepath.Join(root, name))
		if err != nil {
			t.Fatalf("Key(%q) errored: %v", name, err)
		}
		if got != name {
			t.Errorf("Key(%q) = %q, want it unchanged", name, got)
		}
	}
}

func TestKeyHandlesWindowsSeparators(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("filepath.Rel only treats backslash as a separator on Windows")
	}
	got, err := Key(`C:\Users\me\Docs`, `C:\Users\me\Docs\a\b.txt`)
	if err != nil {
		t.Fatal(err)
	}
	if got != "a/b.txt" {
		t.Errorf("got %q, want a/b.txt", got)
	}
}

func TestNormalizeKeyMatchesKey(t *testing.T) {
	root := filepath.FromSlash("/data/set")
	fromDisk, err := Key(root, filepath.Join(root, "docs", nfdName))
	if err != nil {
		t.Fatal(err)
	}
	fromBucket := NormalizeKey("docs/" + nfdName)
	if fromDisk != fromBucket {
		t.Fatalf("a key from disk and the same key from a bucket listing disagree:\n  disk   %q\n  bucket %q", fromDisk, fromBucket)
	}
}
