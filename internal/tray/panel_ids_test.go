package tray

import (
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// Every id in the panel is unique. $("x") finds the first element with id x,
// so a second sheet reusing an id quietly takes over the first one's buttons:
// Ultra fast mode's sheet once reused the Activity run sheet's u-back, and its
// Back handler replaced the run sheet's, which could then not be closed.
func TestPanelIDsAreUnique(t *testing.T) {
	raw, err := os.ReadFile("panel.html")
	if err != nil {
		t.Fatal(err)
	}
	page := string(raw)
	cut := strings.Index(page, "<script>")
	if cut < 0 {
		t.Fatal("panel.html has no <script>")
	}
	markup, script := page[:cut], page[cut:]
	idAttr := regexp.MustCompile(`\sid="([^"]+)"`)

	static := map[string]int{}
	for _, m := range idAttr.FindAllStringSubmatch(markup, -1) {
		static[m[1]]++
	}
	var dups []string
	for id, n := range static {
		if n > 1 {
			dups = append(dups, id)
		}
	}
	sort.Strings(dups)
	for _, id := range dups {
		t.Errorf("id %q is used %d times in the panel's markup", id, static[id])
	}

	// Elements the script draws must not take an id the markup already has.
	seen := map[string]bool{}
	for _, m := range idAttr.FindAllStringSubmatch(script, -1) {
		id := m[1]
		if static[id] > 0 && !seen[id] {
			seen[id] = true
			t.Errorf("the script draws an element with id %q, which the markup already uses", id)
		}
	}
	if len(static) < 50 {
		t.Fatalf("found only %d ids; is the pattern still matching the markup?", len(static))
	}
}
