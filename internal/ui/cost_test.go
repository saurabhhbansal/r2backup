package ui

import (
	"strings"
	"testing"
)

// costTab returns a model sitting on the Cost tab with the given overview.
func costTab(t *testing.T, mutate func(*Overview)) (*Model, *fakeBackend) {
	t.Helper()
	b := twoSets()
	b.ov.Configured = true
	b.ov.OpsLimit = 1_000_000
	if mutate != nil {
		mutate(&b.ov)
	}
	m := sized(b, 120, 40)
	press(m, tabDigit(tabCost))
	if m.tab != tabCost {
		t.Fatalf("tab = %v, want Cost", m.tab)
	}
	return m, b
}

// The whole reason the Cost tab exists: a paused backup must not be able to
// look like a backup with nothing to do.
func TestPausedBudgetIsVisibleAndEscapable(t *testing.T) {
	m, _ := costTab(t, func(ov *Overview) {
		ov.BudgetState = "paused"
		ov.BudgetUSD = 5
		ov.EstimatedUSD = 6.20
	})
	view := m.View()

	if !strings.Contains(view, "paused") {
		t.Errorf("the Cost tab does not say backups are paused:\n%s", view)
	}
	if !strings.Contains(view, "$5.00") {
		t.Errorf("the limit that caused the pause is not shown:\n%s", view)
	}
	// The way out has to be on the same screen as the bad news.
	if !strings.Contains(view, "carry on this month") {
		t.Errorf("a paused screen does not offer a way to carry on:\n%s", view)
	}
	// And nobody should have to wonder about their data.
	if !strings.Contains(strings.ToLower(view), "restoring still works") {
		t.Errorf("a paused screen does not say restores still work:\n%s", view)
	}
}

// c lifts the pause, and only when there is a pause to lift.
func TestContinueResumesOnlyWhenPaused(t *testing.T) {
	m, b := costTab(t, func(ov *Overview) {
		ov.BudgetState = "paused"
		ov.BudgetUSD = 5
	})
	apply(t, m, press(m, "c"))
	if !b.budgetResumed {
		t.Error("c did not resume a paused budget")
	}

	// Within the limit there is nothing paused, so c must not quietly
	// pre-authorise an overspend nobody has been warned about.
	m2, b2 := costTab(t, func(ov *Overview) {
		ov.BudgetState = "within"
		ov.BudgetUSD = 5
	})
	apply(t, m2, press(m2, "c"))
	if b2.budgetResumed {
		t.Error("c resumed a budget that was not paused")
	}
}

func TestSetBudgetThroughTheForm(t *testing.T) {
	m, b := costTab(t, func(ov *Overview) { ov.BudgetState = "off" })
	press(m, "b")
	if m.form == nil {
		t.Fatal("b did not open the limit form")
	}
	typeIn(m, "12.50")
	apply(t, m, enter(m))
	if b.budgetUSD != 12.50 {
		t.Errorf("budget = %v, want 12.50", b.budgetUSD)
	}
}

// A typed 0 must not silently mean the opposite of what it looks like: zero
// is how "no limit" is stored, and a limit of zero would pause everything.
func TestZeroLimitIsRefusedWithAnExplanation(t *testing.T) {
	m, b := costTab(t, func(ov *Overview) { ov.BudgetState = "off" })
	press(m, "b")
	typeIn(m, "0")
	apply(t, m, enter(m))

	if b.budgetUSD != 0 || m.form == nil {
		t.Fatalf("a zero limit was accepted; form still open = %v, budget = %v", m.form != nil, b.budgetUSD)
	}
	if !strings.Contains(m.View(), "pause every backup") {
		t.Errorf("no explanation of why zero was refused:\n%s", m.View())
	}
}

func TestNonNumericLimitIsRefused(t *testing.T) {
	m, b := costTab(t, func(ov *Overview) { ov.BudgetState = "off" })
	press(m, "b")
	typeIn(m, "lots")
	apply(t, m, enter(m))
	if b.budgetUSD != 0 {
		t.Errorf("budget = %v, want it left alone", b.budgetUSD)
	}
	if !strings.Contains(m.View(), "not an amount") {
		t.Errorf("no explanation for a non-numeric limit:\n%s", m.View())
	}
}

// A currency symbol and separators come along with a number people copy.
func TestLimitForgivesDollarSignsAndCommas(t *testing.T) {
	for _, in := range []string{"$5", "1,200", " 7.25 ", "$1,000.00"} {
		m, b := costTab(t, func(ov *Overview) { ov.BudgetState = "off" })
		press(m, "b")
		typeIn(m, in)
		apply(t, m, enter(m))
		if b.budgetUSD == 0 {
			t.Errorf("%q was refused as a limit", in)
		}
	}
}

// x removes the limit, and does nothing when there is none.
func TestRemoveLimit(t *testing.T) {
	m, b := costTab(t, func(ov *Overview) {
		ov.BudgetState = "within"
		ov.BudgetUSD = 5
	})
	b.budgetUSD = 5
	apply(t, m, press(m, "x"))
	if b.budgetUSD != 0 {
		t.Errorf("budget = %v, want 0 after x", b.budgetUSD)
	}
}

// The free tier is the ordinary case and deserves saying outright rather than
// showing somebody a row of zeroes.
func TestFreeTierIsSaidPlainly(t *testing.T) {
	m, _ := costTab(t, func(ov *Overview) {
		ov.WithinFreeTier = true
		ov.BudgetState = "off"
	})
	view := m.View()
	if !strings.Contains(view, "free tier") {
		t.Errorf("the free tier is not mentioned:\n%s", view)
	}
}

// These are r2backup's own figures, not a bill, and the screen has to keep
// saying so -- a number that looks like a bill and is not one is worse than
// no number.
func TestEstimateIsLabelledAsAnEstimate(t *testing.T) {
	m, _ := costTab(t, func(ov *Overview) {
		ov.EstimatedUSD = 3.40
		ov.BudgetState = "off"
	})
	view := strings.ToLower(m.View())
	if !strings.Contains(view, "estimate") {
		t.Errorf("the figures are not labelled as an estimate:\n%s", m.View())
	}
}

func TestUSDFormatting(t *testing.T) {
	for in, want := range map[float64]string{
		0: "$0.00", 5: "$5.00", 4.5: "$4.50", 12.345: "$12.35",
	} {
		if got := usd(in); got != want {
			t.Errorf("usd(%v) = %q, want %q", in, got, want)
		}
	}
}
