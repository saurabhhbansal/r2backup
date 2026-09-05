package cost

import (
	"testing"
	"time"
)

// sept is a fixed "now" for the month-scoped resume rule.
var sept = time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)

// The zero value must be off. A backup tool that invents a spending ceiling
// nobody asked for stops backing up for a reason its owner never chose.
func TestZeroBudgetIsOff(t *testing.T) {
	var b Budget
	if b.Enabled() {
		t.Error("the zero Budget is enabled")
	}
	if got := b.Check(1_000_000, sept); got != Off {
		t.Errorf("Check on an unset budget = %v, want Off", got)
	}
	if !b.AllowsBackup(1_000_000, sept) {
		t.Error("an unset budget blocked a backup")
	}
}

func TestNegativeLimitIsOff(t *testing.T) {
	b := Budget{LimitUSD: -5}
	if b.Enabled() {
		t.Error("a negative limit counts as enabled")
	}
	if !b.AllowsBackup(100, sept) {
		t.Error("a negative limit blocked a backup")
	}
}

func TestCheckVerdicts(t *testing.T) {
	b := Budget{LimitUSD: 10}
	cases := []struct {
		spent float64
		want  Verdict
	}{
		{0, Within},
		{5, Within},
		{7.99, Within},
		{8, Near}, // exactly WarnFraction
		{9.99, Near},
		{10, Paused}, // exactly at the limit
		{25, Paused},
	}
	for _, c := range cases {
		if got := b.Check(c.spent, sept); got != c.want {
			t.Errorf("Check(%.2f) = %v, want %v", c.spent, got, c.want)
		}
	}
}

// The warning has to arrive before the stop, or reaching the limit is the
// first news anyone gets.
func TestWarningComesBeforeTheStop(t *testing.T) {
	b := Budget{LimitUSD: 100}
	if got := b.Check(b.LimitUSD*WarnFraction-0.01, sept); got != Within {
		t.Errorf("just below the warning point = %v, want Within", got)
	}
	if got := b.Check(b.LimitUSD*WarnFraction, sept); got != Near {
		t.Errorf("at the warning point = %v, want Near", got)
	}
	if !b.AllowsBackup(b.LimitUSD*WarnFraction, sept) {
		t.Error("a warning stopped a backup; warnings must not bite")
	}
}

func TestAllowsBackupStopsOnlyWhenPaused(t *testing.T) {
	b := Budget{LimitUSD: 10}
	if !b.AllowsBackup(9.99, sept) {
		t.Error("blocked below the limit")
	}
	if b.AllowsBackup(10, sept) {
		t.Error("allowed an upload at the limit")
	}
}

// Restoring is the emergency the backups were taken for. A limit set months
// ago in a calmer mood must never be what stands between someone and their
// data -- so this is asserted rather than left to a comment.
func TestRestoreIsNeverBlocked(t *testing.T) {
	for _, b := range []Budget{
		{},
		{LimitUSD: 0.01},
		{LimitUSD: 10},
		{LimitUSD: -1},
	} {
		if !b.AllowsRestore() {
			t.Errorf("Budget%+v blocked a restore", b)
		}
	}
	// Including when spending is far past the ceiling.
	over := Budget{LimitUSD: 1}
	if over.Check(9999, sept) != Paused {
		t.Fatal("test setup: expected an exceeded budget")
	}
	if !over.AllowsRestore() {
		t.Error("an exceeded budget blocked a restore")
	}
}

func TestRemaining(t *testing.T) {
	b := Budget{LimitUSD: 10}
	got, ok := b.Remaining(4)
	if !ok {
		t.Fatal("Remaining reported no limit")
	}
	closeTo(t, got, 6, "remaining")

	// Past the limit, remaining is zero rather than negative -- there is no
	// such thing as minus six dollars of headroom.
	got, _ = b.Remaining(25)
	closeTo(t, got, 0, "remaining when over")

	if _, ok := (Budget{}).Remaining(4); ok {
		t.Error("an unset budget reported a remaining amount")
	}
}

// Fraction is allowed past 1: uploading stops at the ceiling but storage
// already written keeps accruing, so the real figure does go over.
func TestFractionReportsOverspendHonestly(t *testing.T) {
	b := Budget{LimitUSD: 10}
	got, ok := b.Fraction(15)
	if !ok {
		t.Fatal("Fraction reported no limit")
	}
	closeTo(t, got, 1.5, "fraction when over")

	if _, ok := (Budget{}).Fraction(5); ok {
		t.Error("an unset budget reported a fraction")
	}
}

func TestVerdictString(t *testing.T) {
	for v, want := range map[Verdict]string{
		Off: "off", Within: "within", Near: "near", Paused: "paused", Resumed: "resumed",
	} {
		if got := v.String(); got != want {
			t.Errorf("Verdict(%d).String() = %q, want %q", int(v), got, want)
		}
	}
}

// Saying "carry on" lifts the pause immediately, without raising or clearing
// the limit -- the point is to get past this month, not to change the ceiling.
func TestResumeLiftsThePause(t *testing.T) {
	b := Budget{LimitUSD: 10}
	if got := b.Check(25, sept); got != Paused {
		t.Fatalf("Check = %v, want Paused before resuming", got)
	}

	b = b.Resume(sept)
	if got := b.Check(25, sept); got != Resumed {
		t.Errorf("Check = %v, want Resumed", got)
	}
	if !b.AllowsBackup(25, sept) {
		t.Error("backups still blocked after resuming")
	}
	// The ceiling is untouched: this was a decision about one month.
	if b.LimitUSD != 10 {
		t.Errorf("LimitUSD = %v, want the limit left alone at 10", b.LimitUSD)
	}
}

// Resumed is deliberately not Within. Spending is over the line its owner
// drew, on purpose, and the screen should keep saying so.
func TestResumedIsStillWorthShowing(t *testing.T) {
	b := Budget{LimitUSD: 10}.Resume(sept)
	if got := b.Check(25, sept); got == Within {
		t.Error("a resumed budget reads as Within; the overspend stops being visible")
	}
}

// The whole reason a month is stored rather than a flag: carrying on in
// September must not quietly carry on forever.
func TestResumeExpiresWithTheMonth(t *testing.T) {
	b := Budget{LimitUSD: 10}.Resume(sept)
	october := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)

	if b.ResumedFor(october) {
		t.Error("September's resume still counts in October")
	}
	if got := b.Check(25, october); got != Paused {
		t.Errorf("Check in October = %v, want Paused again", got)
	}
	if b.AllowsBackup(25, october) {
		t.Error("last month's resume is still allowing uploads")
	}
}

// A resume only matters once the limit is actually reached; below it the
// verdict is unchanged.
func TestResumeChangesNothingBelowTheLimit(t *testing.T) {
	b := Budget{LimitUSD: 10}.Resume(sept)
	if got := b.Check(1, sept); got != Within {
		t.Errorf("Check(1) = %v, want Within", got)
	}
	if got := b.Check(8, sept); got != Near {
		t.Errorf("Check(8) = %v, want Near", got)
	}
}

// Resuming a budget that was never set changes nothing: there is no pause to
// lift, and it must not somehow switch a limit on.
func TestResumeOnAnUnsetBudgetStaysOff(t *testing.T) {
	b := Budget{}.Resume(sept)
	if b.Enabled() {
		t.Error("resuming enabled a budget that was never set")
	}
	if got := b.Check(999, sept); got != Off {
		t.Errorf("Check = %v, want Off", got)
	}
}

func TestMonthKey(t *testing.T) {
	if got, want := MonthKey(sept), "2026-09"; got != want {
		t.Errorf("MonthKey = %q, want %q", got, want)
	}
	// Formatted in UTC, so a machine in another zone resumes for the same
	// month the counters roll over on.
	late := time.Date(2026, 9, 30, 23, 0, 0, 0, time.FixedZone("east", 5*3600))
	if got, want := MonthKey(late), "2026-09"; got != want {
		t.Errorf("MonthKey with an offset zone = %q, want %q", got, want)
	}
}
