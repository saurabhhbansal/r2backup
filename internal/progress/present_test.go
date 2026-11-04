package progress

import (
	"testing"
	"time"
)

func TestFormatETAIsCoarse(t *testing.T) {
	for d, want := range map[time.Duration]string{
		20 * time.Second:               "less than a minute",
		70 * time.Second:               "about a minute",
		4*time.Minute + 20*time.Second: "about 4 minutes",
		38 * time.Minute:               "about 40 minutes",
		58 * time.Minute:               "about an hour",
		95 * time.Minute:               "about an hour and a half",
		2*time.Hour + 50*time.Minute:   "about 3 hours",
		26 * time.Hour:                 "about a day",
		80 * time.Hour:                 "about 3 days",
	} {
		if got := FormatETA(d); got != want {
			t.Errorf("FormatETA(%v) = %q, want %q", d, got, want)
		}
	}
}

func snapAt(elapsed, eta time.Duration, pct float64) Snapshot {
	return Snapshot{Elapsed: elapsed, ETA: eta, ETAKnown: true, Percent: pct, FilesTotal: 10000, FilesDone: int64(pct * 100)}
}

func TestPresenterWaitsForTheWarmUp(t *testing.T) {
	var p Presenter
	if _, ok := p.Present(snapAt(20*time.Second, time.Hour, 10)); ok {
		t.Error("shown before 30s")
	}
	if _, ok := p.Present(snapAt(40*time.Second, time.Hour, 1)); ok {
		t.Error("shown before 2% of anything")
	}
	if d, ok := p.Present(snapAt(41*time.Second, time.Hour, 3)); !ok || d != time.Hour {
		t.Errorf("not shown after the warm-up: %v %v", d, ok)
	}
}

func TestPresenterCountsDownAndIgnoresWobble(t *testing.T) {
	var p Presenter
	p.Present(snapAt(60*time.Second, 3*time.Hour, 5))
	// The estimate wobbles by 10% either way, second to second.
	for i := 1; i <= 120; i++ {
		el := 60*time.Second + time.Duration(i)*time.Second
		wobble := 3*time.Hour - time.Duration(i)*time.Second
		if i%2 == 0 {
			wobble = wobble * 11 / 10
		} else {
			wobble = wobble * 9 / 10
		}
		d, _ := p.Present(snapAt(el, wobble, 6))
		if want := 3*time.Hour - time.Duration(i)*time.Second; d != want {
			t.Fatalf("at +%ds shown %v, want the countdown %v", i, d, want)
		}
	}
}

func TestPresenterFollowsARealChangeAfterTwoSnapshots(t *testing.T) {
	var p Presenter
	p.Present(snapAt(60*time.Second, 3*time.Hour, 5))
	if d, _ := p.Present(snapAt(61*time.Second, time.Hour, 6)); d == time.Hour {
		t.Error("followed a change after one snapshot")
	}
	if d, _ := p.Present(snapAt(62*time.Second, time.Hour, 6)); d != time.Hour {
		t.Errorf("did not follow a change held for two snapshots: %v", d)
	}
}

func TestPresenterWaitsForFilesNotOnlyBytes(t *testing.T) {
	var p Presenter
	// Nine tenths of the bytes, a handful of the files: the big ones went
	// first and nothing yet says what the small ones cost.
	s := Snapshot{Elapsed: 90 * time.Second, ETA: time.Hour, ETAKnown: true, Percent: 90, FilesTotal: 41368, FilesDone: 57}
	if _, ok := p.Present(s); ok {
		t.Error("shown with only bytes done")
	}
	s.FilesDone = 800 // under 2% of the files
	if _, ok := p.Present(s); ok {
		t.Error("shown before 2% of the files")
	}
	s.FilesDone = 830
	if _, ok := p.Present(s); !ok {
		t.Error("not shown at 2% of the files")
	}
	// A small plan does not wait for fifty files.
	var q Presenter
	if _, ok := q.Present(Snapshot{Elapsed: 40 * time.Second, ETA: time.Minute, ETAKnown: true, Percent: 30, FilesTotal: 100, FilesDone: 30}); !ok {
		t.Error("a 100-file plan waited for fifty files")
	}
	// A few big files are judged by bytes: there are no small ones to wait for.
	var r Presenter
	if _, ok := r.Present(Snapshot{Elapsed: 40 * time.Second, ETA: time.Hour, ETAKnown: true, Percent: 10, FilesTotal: 3, FilesDone: 0}); !ok {
		t.Error("three big files never showed an estimate")
	}
}
