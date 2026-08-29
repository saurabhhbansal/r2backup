// Package tui is the interactive folder picker shown once, when a folder is
// first added. Every other run of r2backup is unattended, so this screen
// carries the entire "does this look right" burden for a set — see the
// package-level doc on Pick for the full argument.
package tui

import (
	"strings"

	"github.com/saurabhhbansal/r2backup/internal/scan"
)

// CheckState is a node's selection state. It has to be three-valued, not
// boolean: a directory whose children disagree (some in, some out) is neither
// "checked" nor "unchecked", and collapsing that into either one would show
// the user a tree that lies about what gets backed up.
type CheckState uint8

const (
	Unchecked CheckState = iota
	Checked
	Partial
)

// Node is one entry in the in-memory tree built from a scan.Result. It knows
// nothing about bubbletea or a terminal — that separation is what lets the
// selection logic (this file) be tested without one.
type Node struct {
	Name     string // base name only, for display
	Key      string // full relative key as scan.Walk produced it; "" for the synthetic root
	Parent   *Node
	Children []*Node

	IsDir bool  // true for directories, including empty ones (scan.KindEmptyDir)
	Size  int64 // this node's own size; 0 for directories, symlinks and empty dirs
	Kind  scan.Kind

	// TotalSize and TotalFiles are the aggregate size and item count of
	// everything beneath this node (or, for a leaf, the node itself). They
	// are fixed at build time and do not change as things are checked or
	// unchecked — they describe the tree, not the current selection.
	TotalSize  int64
	TotalFiles int64

	// SelectedSize and SelectedFiles are the same aggregates, but restricted
	// to what is currently checked beneath this node. They are maintained
	// incrementally by toggle (see setSubtree/recalc) rather than recomputed
	// by walking the whole tree, so the live footer total stays cheap even on
	// a 60,000-entry tree: reading it is just reading the root's two fields.
	SelectedSize  int64
	SelectedFiles int64

	Depth    int // 0 for a node directly under the picked root
	Expanded bool
	Check    CheckState
}

// expandable reports whether right/left should treat this node as a
// directory with something to show. An empty directory is still IsDir, but
// has nothing beneath it, so it behaves like a leaf for navigation purposes.
func (n *Node) expandable() bool {
	return n.IsDir && len(n.Children) > 0
}

// BuildTree turns the flat, sorted entry list from scan.Walk into a tree.
// It runs in a single pass over the entries: each entry's key is split once
// into path components, and intermediate directory nodes are created on
// first reference and memoized in dirs, so a directory with 52,880
// descendants is only ever allocated once, not once per descendant. That is
// what keeps this O(n) rather than O(n) allocations times O(depth) lookups
// with re-splitting each time.
func BuildTree(root string, scanned *scan.Result) *Node {
	rootNode := &Node{Name: root, IsDir: true, Expanded: true, Depth: -1}
	dirs := make(map[string]*Node, len(scanned.Entries)/4+1)
	dirs[""] = rootNode

	var getOrCreateDir func(key string) *Node
	getOrCreateDir = func(key string) *Node {
		if n, ok := dirs[key]; ok {
			return n
		}
		parentKey, name := splitParent(key)
		parent := getOrCreateDir(parentKey)
		n := &Node{
			Name:     name,
			Key:      key,
			IsDir:    true,
			Expanded: true, // "I get the whole work tree shown" — collapsed-by-default would hide it
			Parent:   parent,
			Depth:    parent.Depth + 1,
		}
		parent.Children = append(parent.Children, n)
		dirs[key] = n
		return n
	}

	for i := range scanned.Entries {
		e := &scanned.Entries[i]
		parentKey, name := splitParent(e.Key)
		parent := getOrCreateDir(parentKey)

		n := &Node{
			Name:   name,
			Key:    e.Key,
			Parent: parent,
			Depth:  parent.Depth + 1,
			Kind:   e.Kind,
		}
		if e.Kind == scan.KindEmptyDir {
			// An empty directory is its own entry (object storage has no
			// directories, so it needs a marker), but has no children of its
			// own: it is a leaf that happens to render as a folder.
			n.IsDir = true
			n.TotalFiles = 1
		} else {
			n.Size = e.Size
			n.TotalSize = e.Size
			n.TotalFiles = 1
		}
		parent.Children = append(parent.Children, n)
	}

	// Entries arrive from scan.Walk sorted by full key, so siblings are
	// appended to each parent in sorted order already — no separate sort
	// pass needed, which matters at 60,000 entries.

	finalizeChecked(rootNode)
	return rootNode
}

// finalizeChecked walks the freshly built tree once, bottom-up, to roll leaf
// totals up into their ancestors and to seed every node as fully checked —
// the picker's starting state is "everything selected", so Selected* equals
// Total* until the user unchecks something.
func finalizeChecked(n *Node) {
	for _, c := range n.Children {
		finalizeChecked(c)
		n.TotalSize += c.TotalSize
		n.TotalFiles += c.TotalFiles
	}
	n.SelectedSize = n.TotalSize
	n.SelectedFiles = n.TotalFiles
	n.Check = Checked
}

// splitParent splits a "/"-joined key into its parent key and base name.
// A top-level key (no "/") has the synthetic root, keyed by "", as its parent.
func splitParent(key string) (parentKey, name string) {
	if i := strings.LastIndexByte(key, '/'); i >= 0 {
		return key[:i], key[i+1:]
	}
	return "", key
}

// Toggle flips a node's check state and propagates the change down to every
// descendant and up through every ancestor. Toggling a partially-checked
// directory checks it fully — the natural reading of clicking a checkbox that
// is already showing "something is off" is "turn it all on".
func Toggle(n *Node) {
	target := Checked
	if n.Check == Checked {
		target = Unchecked
	}
	setSubtree(n, target)
	recalcAncestors(n.Parent)
}

// setSubtree sets a node and every descendant to the same state in one pass.
// This is the O(size of subtree) operation the "no O(n²)" requirement is
// about: unchecking a 500-file directory touches 501 nodes, never the whole
// 60,000-entry tree, and never re-walks anything it already set.
func setSubtree(n *Node, state CheckState) {
	n.Check = state
	if state == Checked {
		n.SelectedSize = n.TotalSize
		n.SelectedFiles = n.TotalFiles
	} else {
		n.SelectedSize = 0
		n.SelectedFiles = 0
	}
	for _, c := range n.Children {
		setSubtree(c, state)
	}
}

// recalcAncestors recomputes check state and selected totals from the bottom
// up, stopping at the synthetic root. It only ever walks the ancestor chain
// (O(depth)), never sibling subtrees, which is what keeps a toggle deep in a
// large tree cheap regardless of how big the tree around it is.
func recalcAncestors(n *Node) {
	for n != nil {
		var selSize, selFiles int64
		allChecked, allUnchecked := true, true
		for _, c := range n.Children {
			selSize += c.SelectedSize
			selFiles += c.SelectedFiles
			if c.Check != Checked {
				allChecked = false
			}
			if c.Check != Unchecked {
				allUnchecked = false
			}
		}
		n.SelectedSize = selSize
		n.SelectedFiles = selFiles
		switch {
		case len(n.Children) == 0:
			// A leaf's own state was already set by setSubtree; nothing to
			// derive from children it doesn't have.
		case allChecked:
			n.Check = Checked
		case allUnchecked:
			n.Check = Unchecked
		default:
			n.Check = Partial
		}
		n = n.Parent
	}
}

// SetAll checks or unchecks the entire tree, for the "a"/"n" keys.
func SetAll(root *Node, state CheckState) {
	setSubtree(root, state)
}

// ApplyExcludes unchecks everything an existing set already leaves out, so
// the picker opens showing what is actually being backed up rather than
// starting over from "everything".
//
// It is the inverse of ComputeExcludes and has to agree with sets.Set.Excluded
// about what a rule covers: a rule naming a directory excludes everything
// beneath it. Disagreeing would show the user a selection that is not the one
// in force.
//
// A node that matches is unchecked whole and not descended into -- its
// children are already covered by the same rule, and re-walking them would
// only redo work.
func ApplyExcludes(root *Node, excludes []string) {
	if root == nil || len(excludes) == 0 {
		return
	}
	var walk func(n *Node)
	walk = func(n *Node) {
		if n.Key != "" && excludedBy(n.Key, excludes) {
			setSubtree(n, Unchecked)
			return
		}
		for _, c := range n.Children {
			walk(c)
		}
	}
	walk(root)
	// One bottom-up pass. recalcAncestors walks upward from a single change,
	// which is right for a keypress and wrong here: this applies many at once
	// and would otherwise re-walk the same ancestors for every one of them.
	recalcSubtree(root)
}

// excludedBy reports whether key falls under any rule. Kept in step with
// sets.Set.Excluded deliberately; see ApplyExcludes.
func excludedBy(key string, excludes []string) bool {
	for _, ex := range excludes {
		if key == ex || strings.HasPrefix(key, ex+"/") {
			return true
		}
	}
	return false
}

// recalcSubtree recomputes check state and selected totals for a whole tree,
// bottom up. Leaves keep whatever they were set to.
func recalcSubtree(n *Node) {
	if len(n.Children) == 0 {
		return
	}
	var selSize, selFiles int64
	allChecked, allUnchecked := true, true
	for _, c := range n.Children {
		recalcSubtree(c)
		selSize += c.SelectedSize
		selFiles += c.SelectedFiles
		if c.Check != Checked {
			allChecked = false
		}
		if c.Check != Unchecked {
			allUnchecked = false
		}
	}
	n.SelectedSize = selSize
	n.SelectedFiles = selFiles
	switch {
	case allChecked:
		n.Check = Checked
	case allUnchecked:
		n.Check = Unchecked
	default:
		n.Check = Partial
	}
}

// ComputeExcludes returns the minimal set of keys that reproduces the current
// selection under sets.Set.Excluded, which treats a directory key as covering
// everything beneath it. The naive approach — collecting every unchecked leaf
// — produces a list as long as the unchecked part of the tree; a single
// unchecked directory of 52,880 files must instead cost one string. Recursion
// stops the instant it finds a fully-unchecked subtree, so it never descends
// into the part of the tree it has already accounted for.
func ComputeExcludes(root *Node) []string {
	var out []string
	var walk func(n *Node)
	walk = func(n *Node) {
		for _, c := range n.Children {
			switch c.Check {
			case Unchecked:
				out = append(out, c.Key)
			case Partial:
				walk(c)
			case Checked:
				// nothing to exclude here
			}
		}
	}
	walk(root)
	return out
}

// Row is one visible line: a node together with the depth it should be
// indented at (0 for a node directly under the picked root).
type Row struct {
	Node  *Node
	Depth int
}

// VisibleRows flattens the tree into the rows a fully-unrolled view would
// show, skipping the contents of any collapsed directory entirely. It costs
// O(visible nodes), not O(total nodes): a tree with a collapsed 52,880-file
// directory contributes exactly one row for it. This is still not what gets
// rendered — the model windows this list to a viewport — but it is the list
// cursor movement, home/end and paging are computed against.
func VisibleRows(root *Node) []Row {
	var rows []Row
	var walk func(n *Node)
	walk = func(n *Node) {
		for _, c := range n.Children {
			rows = append(rows, Row{Node: c, Depth: c.Depth})
			if c.expandable() && c.Expanded {
				walk(c)
			}
		}
	}
	walk(root)
	return rows
}
