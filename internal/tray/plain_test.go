package tray

import (
	"strings"
	"testing"
)

func TestPlain(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", ""},
		{"that code is not right, or it has expired", "That code is not right, or it has expired"},
		{"monthly spending limit reached; backups are paused and nothing was uploaded: about $5.12 of $5.00 this month. To carry on: r2b budget continue",
			"Backups are paused: this month reached your spending limit (about $5.12 of $5.00). Carry on from Usage and cost."},
		{`open index at "C:\\x\\index.db": another copy of r2b is already running (a backup, or the dashboard), and this one stopped rather than wait for it: timeout`,
			"Another backup was running at the same time, so this one did not start. It runs again next time."},
		{"the folder this set points at no longer exists: C:\\Photos",
			"This folder is not where it was. Nothing was deleted from your backup. Open the folder to find where it went."},
		{"no credentials on this machine yet. Run: r2b setup", "Choose where backups go first: Settings, then Account."},
		{"nothing is being backed up yet. Run: r2b add <folder>", "Nothing is being backed up yet. Add a folder first."},
		{"your sign-in has expired. Run: r2b setup", "Your sign-in has expired. Sign in again from Settings, Account."},
		{"no mirror destination yet. Run: r2b mirror setup", "There is no second copy set up yet. Set one up in Settings, Second copy."},
		{`"Photos" is being backed up right now. Wait for it to finish, or check with: r2b status`, `"Photos" is being backed up right now. Wait for it to finish.`},
		{`a set called "Photos" already exists; pass --name to choose another`, `A set called "Photos" already exists`},
		{"written by a newer r2backup (format 3); update with: r2b update", "Written by a newer r2backup (format 3). Update r2backup from Settings."},
		{"something failed\n  Press m on this folder to say where it went", "Something failed"},
	}
	for _, c := range cases {
		if got := Plain(c.in); got != c.want {
			t.Errorf("Plain(%q)\n got %q\nwant %q", c.in, got, c.want)
		}
	}
}

// Nothing the panel can show may still point at a command or a key.
func TestPlainLeavesNoCommands(t *testing.T) {
	for _, in := range []string{
		"no credentials on this machine yet. Run: r2b setup",
		"x. To carry on: r2b budget continue",
		"Nothing was changed. To try again: r2b update",
		"Next: r2b add <folder>",
		"a limit of zero. Run `r2b budget off` instead",
		"the thing. Press k to enter your R2 keys instead",
	} {
		got := Plain(in)
		for _, bad := range []string{"r2b ", "Press ", "--"} {
			if strings.Contains(got, bad) {
				t.Errorf("Plain(%q) = %q still contains %q", in, got, bad)
			}
		}
	}
}
