package cli

import (
	"bytes"
	"strings"
	"testing"
)

func TestEveryCommandIsReachableAndDocumented(t *testing.T) {
	root := NewRoot(&Options{})
	want := []string{"add", "edit", "backup", "restore", "status", "ls", "schedule", "rename", "relink", "remove", "trash"}
	have := map[string]bool{}
	for _, c := range root.Commands() {
		have[c.Name()] = true
		if c.Short == "" {
			t.Errorf("%q has no short description; it would appear blank in --help", c.Name())
		}
	}
	for _, w := range want {
		if !have[w] {
			t.Errorf("command %q is missing from the root", w)
		}
	}
}

func TestDecisionPrefersTheSafeAnswer(t *testing.T) {
	cases := []struct {
		yes, no bool
		want    Answer
	}{
		{false, false, Ask},
		{true, false, AlwaysYes},
		{false, true, AlwaysNo},
		// Both set is user error. Taking the destructive reading would be the
		// wrong way to resolve it.
		{true, true, AlwaysNo},
	}
	for _, tc := range cases {
		o := &Options{Yes: tc.yes, No: tc.no}
		if got := o.Decision(); got != tc.want {
			t.Errorf("Options{Yes:%v, No:%v}.Decision() = %v, want %v", tc.yes, tc.no, got, tc.want)
		}
	}
}

func TestUnimplementedCommandsFailLoudly(t *testing.T) {
	// A backup tool that silently does nothing is worse than one that errors.
	root := NewRoot(&Options{})
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"backup"})
	if err := root.Execute(); err == nil {
		t.Fatal("an unimplemented command returned success")
	}
}

func TestRestoreDocumentsThatItNeverGuesses(t *testing.T) {
	root := NewRoot(&Options{})
	for _, c := range root.Commands() {
		if c.Name() == "restore" {
			if !strings.Contains(c.Long, "never guesses") {
				t.Error("restore's help should say it never guesses a destination")
			}
			for _, f := range []string{"to", "only", "machine", "deleted", "overwrite", "verify"} {
				if c.Flags().Lookup(f) == nil {
					t.Errorf("restore is missing --%s", f)
				}
			}
			return
		}
	}
	t.Fatal("restore command not found")
}

// Moving a set's objects to a prefix matching a new name is deliberately not
// offered. It is a server-side copy and a delete of every object -- two
// operations each -- to change a name only the R2 dashboard shows, and the
// prefix is the set's identity, assigned once and never rewritten. It was
// offered once as `--remote` and never implemented; the flag was removed
// rather than finished. This is here so it is not reintroduced by someone
// reading the half-built version in the history and assuming it was wanted.
func TestRenameOffersNoWayToMoveTheBucketPrefix(t *testing.T) {
	root := NewRoot(&Options{})
	for _, c := range root.Commands() {
		if c.Name() != "rename" {
			continue
		}
		if f := c.Flags().Lookup("remote"); f != nil {
			t.Error("rename has a --remote flag again; moving a prefix is a decision that was made against")
		}
		if strings.Contains(c.Long, "--remote") {
			t.Error("rename's help mentions --remote, which does not exist")
		}
		return
	}
	t.Fatal("rename command not found")
}
