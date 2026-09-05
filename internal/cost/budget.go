package cost

import "time"

// A spending limit on a backup tool is a genuinely dangerous feature, and the
// shape of this file is mostly an argument with that danger.
//
// The problem is that the limit stops protecting data at exactly the moment
// there is more data to protect -- you reach the limit *because* the backup
// grew. And it does it silently by default: a skipped run looks like a run
// with nothing to do. So the rules below are deliberately narrow.
//
//  1. Off unless someone sets a limit. No default ceiling, ever.
//  2. Warn before biting, so reaching the limit is never the first news.
//  3. Uploads only. A limit must never block a restore -- see AllowsRestore.
//  4. Nothing is ever deleted to get back under the line. The limit stops
//     spending; it does not tidy up, and it does not choose what to lose.
//  5. Reaching it is loud. Callers must surface Paused as a state the
//     dashboard shows, not as a quiet no-op.
//  6. It pauses, it does not fail. Whoever set the limit can say carry on --
//     see Resume -- and backups continue immediately.

// WarnFraction is how far into the limit a warning starts. Four fifths is
// far enough in to mean something and early enough that a monthly backup
// still has a run left to notice it.
const WarnFraction = 0.8

// Budget is a monthly spending limit, in US dollars.
//
// The zero value is off, which is the only safe default: a backup tool that
// invents a ceiling nobody asked for is a backup tool that stops backing up
// for reasons its owner never chose.
type Budget struct {
	// LimitUSD is the ceiling. Zero or negative means no limit at all.
	LimitUSD float64

	// ResumedMonth is the calendar month, formatted "2006-01" in UTC, in
	// which someone has said to carry on past the limit.
	//
	// It is a month rather than a flag so that carrying on is a decision
	// about this month and not a decision forever. Come the first, the limit
	// is back without anyone having to remember to restore it -- which is
	// the right way round, because the person who waves a limit through in
	// October is not thinking about November, and a limit quietly disabled
	// for good is worse than no limit at all.
	ResumedMonth string
}

// Verdict is what a Budget says about a month's spending so far.
type Verdict int

const (
	// Off means no limit is set. Distinct from Within so callers can leave
	// the whole idea out of the interface rather than showing an empty
	// gauge to someone who never wanted one.
	Off Verdict = iota
	// Within means under the limit and not yet near it.
	Within
	// Near means past WarnFraction of the limit. Backups still run.
	Near
	// Paused means the limit is reached and nobody has said to carry on.
	// New uploads stop; restores do not. This must be shown, not merely
	// obeyed, and it must be shown as something with a way out.
	Paused
	// Resumed means the limit is reached and someone has said to carry on
	// anyway for this month. Uploads continue. It is a separate verdict from
	// Within because it is still worth showing: spending is over the line
	// the owner drew, on purpose, and the screen should keep saying so.
	Resumed
)

func (v Verdict) String() string {
	switch v {
	case Off:
		return "off"
	case Within:
		return "within"
	case Near:
		return "near"
	case Paused:
		return "paused"
	case Resumed:
		return "resumed"
	default:
		return "unknown"
	}
}

// MonthKey formats a time the way ResumedMonth stores one.
func MonthKey(t time.Time) string { return t.UTC().Format("2006-01") }

// Check reports where spentUSD stands against the limit.
//
// spentUSD should be the month-to-date estimate, not the projection. A limit
// enforced against a projection would stop backups over spending that has not
// happened and might never -- and early in the month the projection is barely
// more than a guess multiplied by thirty.
func (b Budget) Check(spentUSD float64, now time.Time) Verdict {
	if !b.Enabled() {
		return Off
	}
	switch {
	case spentUSD >= b.LimitUSD:
		if b.ResumedFor(now) {
			return Resumed
		}
		return Paused
	case spentUSD >= b.LimitUSD*WarnFraction:
		return Near
	default:
		return Within
	}
}

// Enabled reports whether a limit is set at all.
func (b Budget) Enabled() bool { return b.LimitUSD > 0 }

// ResumedFor reports whether someone has said to carry on in now's month.
//
// A resume from an earlier month does not count, which is the whole point of
// storing a month rather than a flag.
func (b Budget) ResumedFor(now time.Time) bool {
	return b.ResumedMonth != "" && b.ResumedMonth == MonthKey(now)
}

// Resume returns a copy that carries on past the limit for now's month.
func (b Budget) Resume(now time.Time) Budget {
	b.ResumedMonth = MonthKey(now)
	return b
}

// AllowsBackup reports whether a new backup may upload.
//
// This is the only thing a budget is allowed to stop. A caller that finds
// false here must say so where a person will see it -- the dashboard, the
// command's output -- because a backup that silently stops running is the
// failure this whole feature risks causing. It must also say how to carry on,
// because a pause nobody can lift is just a break.
func (b Budget) AllowsBackup(spentUSD float64, now time.Time) bool {
	return b.Check(spentUSD, now) != Paused
}

// AllowsRestore always reports true, whatever the limit and whatever has been
// spent.
//
// It is a method that ignores its receiver on purpose. Restoring is the
// emergency -- it is the moment the backups were taken for -- and a spending
// limit set months ago in a calmer mood must not be what stands between
// someone and their data. Writing the rule as code rather than as a comment
// means the test below it fails if anyone ever changes their mind quietly.
//
// The cost of a restore is Class B operations, which are the cheap ones and
// come with ten million free a month. There is no version of this where
// blocking it is the right trade.
func (b Budget) AllowsRestore() bool { return true }

// Remaining is how much of the limit is left, floored at zero. It returns
// false when no limit is set, so callers do not render a meaningless zero.
func (b Budget) Remaining(spentUSD float64) (float64, bool) {
	if !b.Enabled() {
		return 0, false
	}
	return max0(b.LimitUSD - spentUSD), true
}

// Fraction is how much of the limit is used, from 0 upward. It can exceed 1
// -- spending does not stop at the ceiling, only uploading does, and storage
// already written goes on accruing. Callers drawing a gauge should clamp it;
// callers reporting a number should not.
func (b Budget) Fraction(spentUSD float64) (float64, bool) {
	if !b.Enabled() {
		return 0, false
	}
	return max0(spentUSD) / b.LimitUSD, true
}
