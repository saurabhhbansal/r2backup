package config

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestDataDirHonoursOverride(t *testing.T) {
	want := t.TempDir()
	t.Setenv(EnvDataDir, want)
	got, err := DataDir()
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Errorf("DataDir() = %q, want %q", got, want)
	}
}

func TestEnsureDataDirCreatesIt(t *testing.T) {
	base := filepath.Join(t.TempDir(), "nested", "deeper")
	t.Setenv(EnvDataDir, base)
	got, err := EnsureDataDir()
	if err != nil {
		t.Fatal(err)
	}
	if got != base {
		t.Errorf("got %q, want %q", got, base)
	}
	if _, err := DataDir(); err != nil {
		t.Fatal(err)
	}
}

func TestDerivedPathsSitUnderDataDir(t *testing.T) {
	base := t.TempDir()
	t.Setenv(EnvDataDir, base)

	for name, fn := range map[string]func() (string, error){
		"index":    IndexPath,
		"progress": ProgressPath,
		"lock":     func() (string, error) { return LockPath("Code Projects") },
		"log":      func() (string, error) { return LogPath("Code Projects") },
	} {
		got, err := fn()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !strings.HasPrefix(got, base) {
			t.Errorf("%s path %q is outside the data dir %q", name, got, base)
		}
	}
}

func TestSanitizeMakesSetNamesSafeAsFilenames(t *testing.T) {
	cases := map[string]string{
		"Code Projects":     "Code Projects",
		"a/b":               "a_b",
		`a\b`:               "a_b",
		"C:drive":           "C_drive",
		"what?":             "what_",
		"star*":             "star_",
		`quote"`:            "quote_",
		"pipe|it":           "pipe_it",
		"less<greater>":     "less_greater_",
		"trailing dot.":     "trailing dot",
		"trailing space   ": "trailing space",
		"":                  "_",
		"...":               "_",
	}
	for in, want := range cases {
		if got := sanitize(in); got != want {
			t.Errorf("sanitize(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSanitizeKeepsUnicode(t *testing.T) {
	// A folder called "Café 🎉" is a perfectly ordinary folder and must not be
	// mangled into unrecognisability.
	if got := sanitize("Café 🎉"); got != "Café 🎉" {
		t.Errorf("sanitize mangled a legal unicode name: %q", got)
	}
}

func TestLockPathsDifferPerSet(t *testing.T) {
	t.Setenv(EnvDataDir, t.TempDir())
	a, err := LockPath("one")
	if err != nil {
		t.Fatal(err)
	}
	b, err := LockPath("two")
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Fatal("two sets share a lockfile; one run would block the other for no reason")
	}
}
