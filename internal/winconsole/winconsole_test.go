package winconsole

import (
	"strings"
	"testing"
)

func TestWantsHiddenFindsTheFlag(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want bool
	}{
		{"the scheduled command line", []string{"backup", "--hidden"}, true},
		{"flag before the subcommand", []string{"--hidden", "backup"}, true},
		{"an ordinary interactive run", []string{"backup"}, false},
		{"no arguments at all", nil, false},
		{"a set that merely looks like it", []string{"restore", "--hidden-stuff"}, false},
		{"a set actually named --hidden", []string{"restore", "--", "--hidden"}, false},
		{"--hidden=true is not the spelling the scheduler writes", []string{"backup", "--hidden=true"}, false},
	} {
		if got := WantsHidden(tc.args); got != tc.want {
			t.Errorf("%s: WantsHidden(%q) = %v, want %v", tc.name, tc.args, got, tc.want)
		}
	}
}

// The flag the scheduler writes onto the command line and the flag cobra
// registers are built from the same two constants. This asserts the pair
// stays consistent, since a mismatch would be invisible until a scheduled run
// died with "unknown flag" on a machine nobody was watching.
func TestHiddenFlagSpellings(t *testing.T) {
	if HiddenFlag != "--"+HiddenFlagName {
		t.Fatalf("HiddenFlag = %q, want %q", HiddenFlag, "--"+HiddenFlagName)
	}
	if strings.HasPrefix(HiddenFlagName, "-") {
		t.Fatalf("HiddenFlagName = %q: it is the bare name, cobra adds the dashes", HiddenFlagName)
	}
	if !WantsHidden([]string{HiddenFlag}) {
		t.Fatal("WantsHidden does not recognize HiddenFlag itself")
	}
}
