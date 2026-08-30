package cli

import (
	"sort"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/saurabhhbansal/r2backup/internal/ui"
)

// Everything the command line can do, the window can do.
//
// That is a promise about the whole product and not about one screen, and the
// way it broke last time was not that someone removed a feature -- it was that
// `remove --purge` and `restore --machine` were added to the command line and
// nobody went back to the interface. So it is checked mechanically: this walks
// the real cobra tree, and any command or flag with no entry below fails the
// build. Adding a flag now costs a line here, and writing that line is the
// moment you notice there is nowhere in the window to press it.
//
// A few entries are honestly "not applicable", and each says why. That is the
// escape hatch, and it is deliberately one you have to write a sentence into.
type surface struct {
	// Keys are the keystrokes that reach it. They are checked against what
	// the interface actually binds, so a line here cannot go stale quietly.
	Keys []string
	// Where is the screen, in the words the user would use.
	Where string
	// NA explains why a flag has no place in an interactive window. Only a
	// flag whose entire purpose is to answer a question nobody is there to
	// answer belongs here.
	NA string
}

var tuiSurface = map[string]surface{
	// The root command is the interface.
	"r2b": {Keys: []string{"1", "2", "3", "4", "tab"}, Where: "running r2b with no arguments is the interface"},
	"r2b --yes": {NA: "pre-answers a prompt for a script or a scheduled run. " +
		"Nothing in the window prompts at someone who is not there to answer."},
	"r2b --no":     {NA: "the same, taking the safe path."},
	"r2b --hidden": {NA: "hides the console window of a Windows scheduled run. There is no window to hide when a person opened one."},

	"r2b setup":        {Keys: []string{"i", "d", "k"}, Where: "Account: sign in, download saved keys, enter R2 keys"},
	"r2b setup --keys": {Keys: []string{"k"}, Where: "Account: k enters R2 keys whether or not a saved copy exists"},

	"r2b add":             {Keys: []string{"a"}, Where: "Folders: a browses to a folder, then the tree picker"},
	"r2b add --all":       {Keys: []string{"enter"}, Where: "the picker opens with everything ticked; enter takes all of it"},
	"r2b add --name":      {Keys: []string{"a"}, Where: "the form after the picker asks what to call it"},
	"r2b add --retention": {Keys: []string{"a"}, Where: "the same form asks how long to keep deleted files"},
	"r2b add --every":     {Keys: []string{"y", "s", "e"}, Where: "the first folder added is followed by the same offer to schedule; Schedule: s and e after that"},

	"r2b edit": {Keys: []string{"e"}, Where: "Folders: e reopens the picker on what is already chosen"},

	"r2b backup": {Keys: []string{"b", "B"}, Where: "Folders: b backs up the highlighted folder, B all of them"},

	"r2b restore":             {Keys: []string{"r"}, Where: "Folders: r opens the restore form"},
	"r2b restore --to":        {Keys: []string{"r"}, Where: "the restore form's first field"},
	"r2b restore --only":      {Keys: []string{"r"}, Where: "the restore form's second field"},
	"r2b restore --overwrite": {Keys: []string{"r"}, Where: "the restore form's third field"},
	"r2b restore --verify":    {Keys: []string{"r"}, Where: "the restore form's fourth field"},
	"r2b restore --deleted":   {Keys: []string{"enter"}, Where: "Trash: enter recovers the highlighted file"},
	"r2b restore --machine":   {Keys: []string{"c"}, Where: "Folders: c lists every backup in the bucket; enter restores another computer's"},

	"r2b status":         {Keys: []string{"1"}, Where: "Folders is the status screen, and the header and footer carry the rest"},
	"r2b status --watch": {Keys: []string{"w"}, Where: "w reopens the progress screen, including for a run the scheduler started"},

	"r2b ls": {Keys: []string{"f"}, Where: "Folders: f lists what is stored for the highlighted folder, with sizes"},

	"r2b trash":    {Keys: []string{"3"}, Where: "the Trash mode"},
	"r2b trash ls": {Keys: []string{"3"}, Where: "the Trash mode lists what is recoverable and until when"},

	"r2b schedule":          {Keys: []string{"2"}, Where: "the Schedule mode"},
	"r2b schedule --every":  {Keys: []string{"e"}, Where: "Schedule: e changes how often"},
	"r2b schedule --remove": {Keys: []string{"s"}, Where: "Schedule: s turns automatic backups off"},
	"r2b schedule --repair": {Keys: []string{"p"}, Where: "Schedule: p re-points the task at this copy of r2b"},

	"r2b rename": {Keys: []string{"n"}, Where: "Folders: n renames"},
	"r2b relink": {Keys: []string{"m"}, Where: "Folders: m says where a folder went"},

	"r2b remove":         {Keys: []string{"x"}, Where: "Folders: x stops backing a folder up and keeps what is stored"},
	"r2b remove --purge": {Keys: []string{"X"}, Where: "Folders: X stops and deletes the stored copy, after the name is typed back"},

	"r2b update":         {Keys: []string{"u"}, Where: "Account: u checks, and a second u installs"},
	"r2b update --check": {Keys: []string{"u"}, Where: "the first u only reports what is available"},

	"r2b account":         {Keys: []string{"4"}, Where: "the Account mode"},
	"r2b account devices": {Keys: []string{"4"}, Where: "Account lists the computers signed in, this one marked"},
	"r2b account logout":  {Keys: []string{"o"}, Where: "Account: o signs out"},
}

func TestEveryCommandAndFlagIsReachableFromTheInterface(t *testing.T) {
	bound := map[string]bool{}
	for _, k := range ui.BoundKeys() {
		bound[k] = true
	}

	seen := map[string]bool{}
	check := func(name string) {
		t.Helper()
		seen[name] = true
		s, ok := tuiSurface[name]
		if !ok {
			t.Errorf("%s has no entry in tuiSurface: either give it a place in the interface, "+
				"or record why an interactive window has no use for it", name)
			return
		}
		if s.NA != "" {
			if len(s.Keys) > 0 {
				t.Errorf("%s is marked not applicable but also lists keys", name)
			}
			return
		}
		if s.Where == "" {
			t.Errorf("%s has no entry saying where in the interface it is", name)
		}
		if len(s.Keys) == 0 {
			t.Errorf("%s names no key", name)
		}
		for _, k := range s.Keys {
			if !bound[k] {
				t.Errorf("%s says press %q, and the interface does not bind that key", name, k)
			}
		}
	}

	var walk func(c *cobra.Command, path string)
	walk = func(c *cobra.Command, path string) {
		if c.Hidden {
			return
		}
		check(path)
		c.LocalFlags().VisitAll(func(f *pflag.Flag) {
			if f.Hidden && f.Name != "hidden" {
				return
			}
			// cobra adds these to every command and they are not features.
			if f.Name == "help" || f.Name == "version" {
				return
			}
			check(path + " --" + f.Name)
		})
		for _, sub := range c.Commands() {
			walk(sub, path+" "+strings.Fields(sub.Use)[0])
		}
	}
	walk(NewRoot(&Options{}), "r2b")

	var stale []string
	for name := range tuiSurface {
		if !seen[name] {
			stale = append(stale, name)
		}
	}
	sort.Strings(stale)
	if len(stale) > 0 {
		t.Errorf("tuiSurface names things the command line no longer has: %v", stale)
	}
}
