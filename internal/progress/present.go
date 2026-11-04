package progress

import (
	"math"
	"strconv"
	"time"
)

// Presenter turns the estimator's running answer into the number a person is
// shown: nothing until it is worth saying, then a value that counts down by
// itself and only changes when the estimate has really moved.
//
// The estimate underneath is recomputed every second and is allowed to move;
// the display is not. "About 3 hours", then "about 2 hours" an hour later, is
// what a person can plan around. A number that wobbles between 2h50 and 3h10
// every few seconds is not more accurate, it is only more anxious.
//
// It keeps time by Snapshot.Elapsed, which stops while a run is paused, so a
// paused run's estimate stays where it was.
type Presenter struct {
	shown    bool
	anchor   time.Duration // the estimate last adopted
	anchorAt time.Duration // Elapsed when it was adopted
	pending  int           // consecutive snapshots that disagreed with it
}

const (
	// presentAfter and presentPercent are the warm-up: the first half
	// minute of any run is mostly connection setup and the first few slow
	// files, and an estimate from it says more about that than the run.
	presentAfter   = 30 * time.Second
	presentPercent = 2.0
	// presentFiles is how many files must be done before the estimate is
	// shown (or presentPercent of them, if that is more): by then the run
	// has met small files as well as big ones. Bytes alone are not enough --
	// a few large files going first can be nine tenths of the bytes while
	// the forty thousand small ones, which take nearly all the time, have
	// not started.
	presentFiles = 50
	// fewFiles is the size below which a plan is judged by bytes instead:
	// a handful of big files has no small ones to wait for.
	fewFiles = 20
	// presentChange is how far the estimate must move, as a share of what
	// is shown, before the display follows it; presentHold is how many
	// snapshots in a row it must stay moved.
	presentChange = 0.15
	presentHold   = 2
)

// seenEnough reports whether the run has done enough of its files to say
// anything about how long the rest will take.
func seenEnough(s Snapshot) bool {
	if s.FilesTotal < fewFiles {
		return s.Percent >= presentPercent
	}
	need := math.Max(math.Ceil(presentPercent/100*float64(s.FilesTotal)), math.Min(presentFiles, float64(s.FilesTotal)/4))
	return float64(s.FilesDone) >= need
}

// Present returns the remaining time to show, and whether to show one.
func (p *Presenter) Present(s Snapshot) (time.Duration, bool) {
	if !p.shown {
		if !s.ETAKnown || s.Elapsed < presentAfter || !seenEnough(s) {
			return 0, false
		}
		p.shown, p.anchor, p.anchorAt = true, s.ETA, s.Elapsed
		return p.anchor, true
	}
	current := p.anchor - (s.Elapsed - p.anchorAt)
	if current < 0 {
		current = 0
	}
	if !s.ETAKnown {
		return current, true
	}
	moved := math.Abs(float64(s.ETA-current)) > presentChange*math.Max(float64(current), float64(time.Minute)) ||
		FormatETA(s.ETA) != FormatETA(current)
	if !moved {
		p.pending = 0
		return current, true
	}
	p.pending++
	if p.pending < presentHold {
		return current, true
	}
	p.pending, p.anchor, p.anchorAt = 0, s.ETA, s.Elapsed
	return p.anchor, true
}

// FormatETA says how long is left the way a person would say it: "less than
// a minute", "about 40 minutes", "about 3 hours". Deliberately coarse. The
// estimate is an estimate, and a display precise to the second claims a
// certainty nobody has.
func FormatETA(d time.Duration) string {
	switch m := d.Minutes(); {
	case m < 1:
		return "less than a minute"
	case m < 1.5:
		return "about a minute"
	case m < 10:
		return "about " + strconv.Itoa(int(math.Round(m))) + " minutes"
	case m < 52.5:
		return "about " + strconv.Itoa(5*int(math.Round(m/5))) + " minutes"
	case m < 75:
		return "about an hour"
	case m < 105:
		return "about an hour and a half"
	case m < 22.5*60:
		return "about " + strconv.Itoa(int(math.Round(m/60))) + " hours"
	case m < 36*60:
		return "about a day"
	default:
		return "about " + strconv.Itoa(int(math.Round(m/(24*60)))) + " days"
	}
}
