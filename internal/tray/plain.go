package tray

import (
	"regexp"
	"strings"
)

// Plain rewrites a message from the shared code for the panel.
//
// The command line and the terminal interface answer a problem with the next
// command to type or key to press -- "Run: r2b setup", "Press m on this
// folder" -- which is the right answer in a terminal and a dead end here,
// where there is neither. Everything the panel shows that came from that code
// (a run's error, a folder's note, a failed request) goes through this, so
// the panel says what to do in its own terms and the commands keep theirs.
func Plain(msg string) string {
	msg = strings.TrimSpace(msg)
	if msg == "" {
		return ""
	}
	for _, r := range plainRules {
		if r.re.MatchString(msg) {
			msg = r.re.ReplaceAllString(msg, r.with)
		}
	}
	msg = strings.TrimSpace(msg)
	if msg == "" {
		return ""
	}
	// The shared code writes errors in lower case to be wrapped; on their
	// own in the panel they are sentences.
	return strings.ToUpper(msg[:1]) + msg[1:]
}

type plainRule struct {
	re   *regexp.Regexp
	with string
}

// plainRules are applied in order. Whole-message replacements come first,
// because the tail-trimming ones after them would otherwise leave half of a
// sentence that only made sense with its command attached.
var plainRules = []plainRule{
	// A backup paused by the monthly limit.
	{regexp.MustCompile(`(?s)^.*monthly spending limit reached.*?(about \$[0-9.]+ of \$[0-9.]+) this month.*$`),
		"Backups are paused: this month reached your spending limit ($1). Carry on from Usage and cost."},
	{regexp.MustCompile(`(?s)^.*monthly spending limit reached.*$`),
		"Backups are paused: this month reached your spending limit. Carry on from Usage and cost."},
	// The index is held by another copy.
	{regexp.MustCompile(`(?s)^.*another copy of r2b is already running.*$`),
		"Another backup was running at the same time, so this one did not start. It runs again next time."},
	// A folder that is not where it was.
	{regexp.MustCompile(`(?s)^.*(the folder this set points at no longer exists|set root does not exist).*$`),
		"This folder is not where it was. Nothing was deleted from your backup. Open the folder to find where it went."},
	// Nothing connected yet.
	{regexp.MustCompile(`(?s)no credentials on this machine yet\.?\s*Run: r2b setup`),
		"Choose where backups go first: Settings, then Account."},
	{regexp.MustCompile(`(?s)nothing is being backed up yet\.?\s*Run: r2b add <folder>`),
		"nothing is being backed up yet. Add a folder first."},
	{regexp.MustCompile(`(?s)(your sign-in has expired|not signed in on this computer)\.?\s*Run: r2b setup`),
		"$1. Sign in again from Settings, Account."},
	{regexp.MustCompile(`(?s)no mirror destination yet\.?\s*Run: r2b mirror setup`),
		"there is no second copy set up yet. Set one up in Settings, Second copy."},
	{regexp.MustCompile(`(?s);?\s*update with: r2b update`), ". Update r2backup from Settings."},
	// Tails that name a command or a key: the sentence before them stands.
	{regexp.MustCompile(`(?s)\s*Press [a-zA-Z] [^.]*\.?$`), ""},
	{regexp.MustCompile(`(?s)[,.]?\s*(or )?check with: r2b status\.?`), "."},
	{regexp.MustCompile(`(?s);?\s*pass --name to choose another`), ""},
	{regexp.MustCompile(`(?s)\s*(To carry on|To try again|To do it later|When you know where it is|Next)[^:\n]*:\s*r2b [^\n]*`), ""},
	{regexp.MustCompile(`(?s)\s*Run:? ` + "`?" + `r2b [^\n]*`), ""},
}
