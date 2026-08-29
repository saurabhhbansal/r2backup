package cli

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/saurabhhbansal/r2backup/internal/sets"
)

// remoteSet is a backup found in the bucket that this computer has no local
// record of.
type remoteSet struct {
	Name    string
	Machine string
	Prefix  string
}

// discoverBackups lists what is stored in the bucket, from the bucket.
//
// A second computer used to be able to sign in, receive the R2 credentials
// and then do nothing with them: `restore` reads the set out of the local
// sets.json, and a machine that has never run `add` has an empty one. The
// data was there and there was no way to name it. Nothing writes a manifest,
// so the layout is the manifest: `add` builds every prefix as
// machines/<machine>/<set>, so two delimited LISTs recover both halves.
func discoverBackups(ctx context.Context, a *app) ([]remoteSet, error) {
	machines, err := a.client.ListPrefixes(ctx, "machines/")
	if err != nil {
		return nil, err
	}
	var found []remoteSet
	for _, m := range machines {
		names, err := a.client.ListPrefixes(ctx, "machines/"+m+"/")
		if err != nil {
			return nil, err
		}
		for _, n := range names {
			found = append(found, remoteSet{Name: n, Machine: m, Prefix: "machines/" + m + "/" + n})
		}
	}
	return found, nil
}

// existingBackups is the set names stored in the bucket, deduplicated across
// machines, for the one line setup prints at the end.
func existingBackups(ctx context.Context, a *app) ([]string, error) {
	if err := a.connect(ctx); err != nil {
		return nil, err
	}
	found, err := discoverBackups(ctx, a)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var names []string
	for _, f := range found {
		if !seen[f.Name] {
			seen[f.Name] = true
			names = append(names, f.Name)
		}
	}
	sort.Strings(names)
	return names, nil
}

// resolveRestoreSet finds the set to restore, locally first and in the bucket
// second.
//
// The local record is preferred because it carries the folder's original path
// -- which is what lets `restore` put a folder back where it came from without
// being told. A set discovered in the bucket has no Root, so restore will
// require --to, which is the correct demand on a computer that never had the
// folder in the first place.
//
// machine narrows the search when the same set name exists for more than one
// computer. Guessing there would restore one computer's Documents believing it
// to be another's, so an ambiguous name is an error that names the choices.
func resolveRestoreSet(ctx context.Context, a *app, name, machine string) (sets.Set, error) {
	if s, err := a.sets.Get(name); err == nil {
		return s, nil
	}
	found, err := discoverBackups(ctx, a)
	if err != nil {
		return sets.Set{}, fmt.Errorf("no set called %q on this computer, and the bucket could not be read: %w", name, err)
	}
	var matches []remoteSet
	for _, f := range found {
		if f.Name == name && (machine == "" || f.Machine == machine) {
			matches = append(matches, f)
		}
	}
	switch len(matches) {
	case 1:
		m := matches[0]
		return sets.Set{Name: m.Name, Machine: m.Machine, Prefix: m.Prefix}, nil
	case 0:
		return sets.Set{}, fmt.Errorf("nothing called %q is backed up%s.%s",
			name, forMachine(machine), availableText(found))
	default:
		var who []string
		for _, m := range matches {
			who = append(who, m.Machine)
		}
		return sets.Set{}, fmt.Errorf("%q is backed up from more than one computer (%s). "+
			"Say which: r2b restore %s --machine %s --to <folder>",
			name, strings.Join(who, ", "), name, who[0])
	}
}

func forMachine(machine string) string {
	if machine == "" {
		return ""
	}
	return fmt.Sprintf(" from %q", machine)
}

// availableText lists what there is instead, so a mistyped name does not end
// in a dead end on a computer with no local record to consult.
func availableText(found []remoteSet) string {
	if len(found) == 0 {
		return " Nothing is stored in this bucket yet."
	}
	var lines []string
	for _, f := range found {
		lines = append(lines, fmt.Sprintf("  %s (from %s)", f.Name, f.Machine))
	}
	sort.Strings(lines)
	return "\nWhat is there:\n" + strings.Join(lines, "\n")
}
