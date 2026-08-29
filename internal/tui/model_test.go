package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/saurabhhbansal/r2backup/internal/scan"
)

func key(t tea.KeyType) tea.KeyMsg { return tea.KeyMsg{Type: t} }

func runeKey(r string) tea.KeyMsg { return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(r)} }

func TestModel_EnterImmediately_YieldsEmptyExcludes(t *testing.T) {
	res := newResult(map[string]int64{
		"a.txt":     1,
		"dir/b.txt": 2,
		"dir/c.txt": 3,
	})
	m := NewModel("/tmp/root", res)

	m.Update(key(tea.KeyEnter))

	if !m.Accepted() {
		t.Fatal("expected Accepted() after enter")
	}
	if m.Cancelled() {
		t.Fatal("did not expect Cancelled() after enter")
	}
	if got := m.Excludes(); len(got) != 0 {
		t.Fatalf("Excludes() = %v, want empty (nothing was ever unchecked)", got)
	}
}

func TestModel_QuitAndEsc_Cancel_NoExcludes(t *testing.T) {
	for _, k := range []tea.KeyMsg{runeKey("q"), key(tea.KeyEsc), key(tea.KeyCtrlC)} {
		res := newResult(map[string]int64{"a.txt": 1})
		m := NewModel("/tmp/root", res)

		m.Update(k)

		if !m.Cancelled() {
			t.Fatalf("key %q: expected Cancelled()", k)
		}
		if m.Accepted() {
			t.Fatalf("key %q: did not expect Accepted()", k)
		}
	}
}

func TestModel_SpaceTogglesCursorItem(t *testing.T) {
	res := newResult(map[string]int64{
		"a.txt": 1,
		"b.txt": 2,
	})
	m := NewModel("/tmp/root", res)

	// Cursor starts on the first visible row, "a.txt".
	first := m.rows[m.cursor].Node
	if first.Key != "a.txt" {
		t.Fatalf("expected cursor to start on a.txt, got %q", first.Key)
	}

	m.Update(key(tea.KeySpace))
	if first.Check != Unchecked {
		t.Fatalf("a.txt Check = %v, want Unchecked after space", first.Check)
	}
	excludes := m.Excludes()
	if len(excludes) != 1 || excludes[0] != "a.txt" {
		t.Fatalf("Excludes() = %v, want [a.txt]", excludes)
	}

	m.Update(key(tea.KeySpace)) // toggle back
	if first.Check != Checked {
		t.Fatalf("a.txt Check = %v, want Checked after second space", first.Check)
	}
}

func TestModel_AAndN_CheckAndUncheckEverything(t *testing.T) {
	res := newResult(manyFiles("dir", 10))
	m := NewModel("/tmp/root", res)

	m.Update(runeKey("n"))
	if m.root.Check != Unchecked {
		t.Fatalf("after 'n', root.Check = %v, want Unchecked", m.root.Check)
	}
	if len(m.Excludes()) != 1 {
		t.Fatalf("after 'n', Excludes() = %v, want a single directory entry", m.Excludes())
	}

	m.Update(runeKey("a"))
	if m.root.Check != Checked {
		t.Fatalf("after 'a', root.Check = %v, want Checked", m.root.Check)
	}
	if len(m.Excludes()) != 0 {
		t.Fatalf("after 'a', Excludes() = %v, want empty", m.Excludes())
	}
}

func TestModel_Navigation_CursorStaysInRange(t *testing.T) {
	res := newResult(map[string]int64{
		"a.txt": 1,
		"b.txt": 2,
		"c.txt": 3,
	})
	m := NewModel("/tmp/root", res)

	// Up at the top does nothing.
	m.Update(key(tea.KeyUp))
	if m.cursor != 0 {
		t.Fatalf("cursor = %d, want 0 (clamped at top)", m.cursor)
	}

	m.Update(key(tea.KeyEnd))
	if m.cursor != len(m.rows)-1 {
		t.Fatalf("cursor after End = %d, want %d", m.cursor, len(m.rows)-1)
	}

	// Down past the bottom does nothing further.
	m.Update(key(tea.KeyDown))
	if m.cursor != len(m.rows)-1 {
		t.Fatalf("cursor = %d, want clamped at %d", m.cursor, len(m.rows)-1)
	}

	m.Update(key(tea.KeyHome))
	if m.cursor != 0 {
		t.Fatalf("cursor after Home = %d, want 0", m.cursor)
	}
}

func TestModel_CollapseOnCollapsedNode_JumpsToParent(t *testing.T) {
	res := newResult(map[string]int64{
		"dir/sub/leaf.txt": 1,
	})
	m := NewModel("/tmp/root", res)

	leaf := findNode(m.root, "dir/sub/leaf.txt")
	m.jumpTo(leaf)
	if m.rows[m.cursor].Node != leaf {
		t.Fatalf("expected cursor on leaf.txt")
	}

	// leaf.txt cannot be collapsed (it's a file) -> jump to its parent, "sub".
	m.Update(key(tea.KeyLeft))
	sub := findNode(m.root, "dir/sub")
	if m.rows[m.cursor].Node != sub {
		t.Fatalf("after collapsing a leaf, cursor node = %q, want %q", m.rows[m.cursor].Node.Key, sub.Key)
	}

	// "sub" is expanded but has nothing left to collapse into being useful:
	// it IS expandable, so left here collapses it rather than jumping.
	m.Update(key(tea.KeyLeft))
	if sub.Expanded {
		t.Fatalf("expected sub to collapse")
	}
	if m.rows[m.cursor].Node != sub {
		t.Fatalf("cursor should stay on sub after collapsing it")
	}

	// Now sub is an already-collapsed directory: left again jumps to its
	// parent, "dir".
	m.Update(key(tea.KeyLeft))
	dir := findNode(m.root, "dir")
	if m.rows[m.cursor].Node != dir {
		t.Fatalf("after collapsing sub, left should jump to dir; got %q", m.rows[m.cursor].Node.Key)
	}
}

func TestModel_ExpandCollapse_ChangesVisibleRows(t *testing.T) {
	res := newResult(map[string]int64{
		"dir/a.txt": 1,
		"dir/b.txt": 2,
	})
	m := NewModel("/tmp/root", res)
	// Everything starts expanded ("the whole work tree shown").
	withExpanded := len(m.rows)
	if withExpanded != 3 { // dir, dir/a.txt, dir/b.txt
		t.Fatalf("rows with dir expanded = %d, want 3", withExpanded)
	}

	dir := findNode(m.root, "dir")
	m.jumpTo(dir)
	m.Update(key(tea.KeyLeft)) // collapse dir
	if len(m.rows) != 1 {
		t.Fatalf("rows with dir collapsed = %d, want 1", len(m.rows))
	}

	m.Update(key(tea.KeyRight)) // expand again
	if len(m.rows) != withExpanded {
		t.Fatalf("rows after re-expanding = %d, want %d", len(m.rows), withExpanded)
	}
}

func TestModel_View_60kTree_ProducesBoundedLines(t *testing.T) {
	res := newResult(manyFiles("bulk", 60000))
	m := NewModel("/tmp/root", res)
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 40})

	out := m.View()
	lines := strings.Count(out, "\n")
	// Header (2 lines) + blank + up to m.height rows + blank + footer: a
	// small constant, absolutely not anywhere near 60,000.
	if lines > 100 {
		t.Fatalf("View() produced %d lines for a 60,000-entry tree, want a small bounded number", lines)
	}
	if lines < 3 {
		t.Fatalf("View() produced suspiciously few lines: %d", lines)
	}
}

func TestModel_EmptyScan_NoPanic(t *testing.T) {
	m := NewModel("/tmp/root", &scan.Result{})
	m.Update(key(tea.KeyDown))
	m.Update(key(tea.KeySpace))
	m.Update(key(tea.KeyRight))
	m.Update(key(tea.KeyLeft))
	_ = m.View()
	m.Update(key(tea.KeyEnter))
	if !m.Accepted() {
		t.Fatal("expected Accepted() on an empty tree")
	}
	if got := m.Excludes(); len(got) != 0 {
		t.Fatalf("Excludes() on empty tree = %v", got)
	}
}

// windowsSpaceKey is what bubbletea's Windows console reader delivers when the
// user presses space: KeyRunes carrying a single ' ', not KeySpace. Every
// other test in this file sends the unix spelling, which is exactly how the
// checkbox came to be dead on Windows -- the one platform this ships to --
// while the suite stayed green.
func windowsSpaceKey() tea.KeyMsg {
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{' '}}
}

func TestModel_SpaceToggles_InTheWindowsSpelling(t *testing.T) {
	res := newResult(map[string]int64{
		"a.txt": 1,
		"b.txt": 2,
	})
	m := NewModel("/tmp/root", res)

	first := m.rows[m.cursor].Node
	if first.Key != "a.txt" {
		t.Fatalf("expected cursor to start on a.txt, got %q", first.Key)
	}

	m.Update(windowsSpaceKey())
	if first.Check != Unchecked {
		t.Fatalf("a.txt Check = %v, want Unchecked after space", first.Check)
	}
	if got := m.Excludes(); len(got) != 1 || got[0] != "a.txt" {
		t.Fatalf("Excludes() = %v, want [a.txt]", got)
	}

	m.Update(windowsSpaceKey())
	if first.Check != Checked {
		t.Fatalf("a.txt Check = %v, want Checked after second space", first.Check)
	}
	if got := m.Excludes(); len(got) != 0 {
		t.Fatalf("Excludes() = %v, want empty after toggling back", got)
	}
}
