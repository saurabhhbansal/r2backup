package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/saurabhhbansal/r2backup/internal/sets"
)

func TestEveryCommandIsReachableAndDocumented(t *testing.T) {
	root := NewRoot(&Options{})
	want := []string{"setup", "add", "edit", "backup", "restore", "status", "ls", "schedule", "rename", "relink", "remove", "trash", "account", "update"}
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

// TestRenameTellsYouTheOriginalNameOtherComputersNeed is the other half of
// H3: the bucket prefix a set was created under is the only name any other
// computer's `restore` ever sees, forever, because rename deliberately never
// touches it (see the comment on newRenameCmd). Nothing said so. `rename`
// now does, but only when it would tell the user something they don't
// already know -- a set renamed back to the name its prefix already spells
// out has nothing to warn about.
func TestRenameTellsYouTheOriginalNameOtherComputersNeed(t *testing.T) {
	t.Setenv("R2BACKUP_DATA_DIR", t.TempDir())

	setup, err := openApp()
	if err != nil {
		t.Fatal(err)
	}
	if err := setup.sets.Add(sets.Set{
		Name: "docs", Root: t.TempDir(), Machine: "testpc",
		Prefix: "machines/testpc/docs", RetentionDays: 30,
	}); err != nil {
		t.Fatal(err)
	}
	setup.close()

	run := func(args ...string) string {
		t.Helper()
		var out bytes.Buffer
		root := NewRoot(&Options{Out: &out, Err: &out})
		root.SetOut(&out)
		root.SetErr(&out)
		root.SetArgs(args)
		if err := root.Execute(); err != nil {
			t.Fatalf("%v: %v\n--- output ---\n%s", args, err, out.String())
		}
		return out.String()
	}

	// "documents" is not what the prefix ("machines/testpc/docs") spells
	// out, so any other computer still has to ask for "docs". Say so.
	got := run("rename", "docs", "documents")
	if !strings.Contains(got, `"docs"`) || !strings.Contains(got, "restore docs") {
		t.Errorf("renaming to a name that differs from the bucket prefix did not name the original:\n%s", got)
	}

	// Renamed back to exactly what the prefix already spells out: this
	// computer's name and the bucket's agree again, so there is nothing left
	// to warn about.
	got = run("rename", "documents", "docs")
	if strings.Contains(got, "Other computers still see") {
		t.Errorf("renaming back to the prefix's own name still printed the other-computers warning:\n%s", got)
	}
}

// The command list is the whole interface for someone who is not at a
// terminal all day, and three entries in it were costing more than they
// returned.
//
// `completion` is cobra's, generated on every root command unless it is
// switched off: to anyone who does not already know what a completion script
// is, it is a command that appears to do nothing and then prints several
// hundred lines of shell.
//
// `login` and `account push` were two thirds of a three-command choreography
// for getting a second computer working, and nothing said which one to run
// when. `setup` does all of it.
func TestTheCommandsThatWereRemovedStayRemoved(t *testing.T) {
	root := NewRoot(&Options{})
	gone := map[string]string{
		"completion": "cobra's generated completion command is noise in this help; DisableDefaultCmd turns it off",
		"login":      "signing in is part of setup",
	}
	for _, c := range root.Commands() {
		if why, bad := gone[c.Name()]; bad {
			t.Errorf("%q is back in the command list: %s", c.Name(), why)
		}
	}
	for _, c := range root.Commands() {
		if c.Name() != "account" {
			continue
		}
		for _, sub := range c.Commands() {
			if sub.Name() == "push" {
				t.Error("`account push` is back; setup stores the credentials as part of signing in, " +
					"and a step you have to know to run in advance is the thing that made this not work")
			}
		}
	}
}

// The command a user types is "r2b". The product, the repo, the release asset
// and the OS scheduler entry all keep the name they had, on purpose -- those
// are identities other things already point at.
func TestTheCommandIsCalledR2b(t *testing.T) {
	root := NewRoot(&Options{})
	if root.Use != "r2b" {
		t.Errorf("root command Use = %q, want %q", root.Use, "r2b")
	}
	// Help text that names the old command would send the user to a binary
	// that is no longer installed under that name.
	for _, c := range root.Commands() {
		for _, text := range []string{c.Short, c.Long} {
			if strings.Contains(text, "r2backup ") {
				t.Errorf("%q's help still tells the user to run `r2backup ...`:\n%s", c.Name(), text)
			}
		}
	}
}
