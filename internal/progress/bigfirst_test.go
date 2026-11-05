package progress

import (
	"testing"
	"time"
)

// bigFirstRun is the shape of a real Drime backup: a few hundred larger files
// carrying nearly all the bytes go first, then forty thousand tiny ones that
// cost a fixed time each. Bytes are 90% done long before files are 2% done,
// which is exactly where the first estimate used to be shown and swung from
// fourteen minutes to two and a half hours.
//
// It returns when the presenter first showed an estimate, and the presented
// remaining time as a multiple of the true remaining time, one per simulated
// second from then on.
func bigFirstRun(t *testing.T, perFile, linkBytesPerSec float64) (shownAt time.Duration, ratios []float64, at []time.Duration, total time.Duration) {
	t.Helper()
	const (
		files    = 41368
		bigFiles = 414 // 1% of the files
		bytes    = 456 << 20
	)
	bigBytes := int64(bytes) / 10 * 9
	tinyBytes := int64(bytes) - bigBytes
	type ev struct {
		at    float64
		bytes int64
	}
	var evs []ev
	now := 20.0 // the pipeline filling before anything completes
	for i := 0; i < bigFiles; i++ {
		sz := bigBytes / bigFiles
		now += perFile + float64(sz)/linkBytesPerSec
		evs = append(evs, ev{now, sz})
	}
	for i := 0; i < files-bigFiles; i++ {
		sz := tinyBytes / (files - bigFiles)
		now += perFile + float64(sz)/linkBytesPerSec
		evs = append(evs, ev{now, sz})
	}
	end := now
	clock := newFakeClock(time.Unix(0, 0))
	tr := New(bytes, files, clock.now)
	var pres Presenter
	i := 0
	for sec := 1.0; sec <= end; sec++ {
		clock.advance(time.Second)
		for i < len(evs) && evs[i].at <= sec {
			tr.Complete(evs[i].bytes)
			i++
		}
		s := tr.Snapshot()
		d, ok := pres.Present(s)
		if !ok {
			continue
		}
		if shownAt == 0 {
			shownAt = s.Elapsed
		}
		trueLeft := end - sec
		if trueLeft < 60 {
			break
		}
		ratios = append(ratios, d.Seconds()/trueLeft)
		at = append(at, s.Elapsed)
	}
	return shownAt, ratios, at, time.Duration(end * float64(time.Second))
}

func TestEstimateAfterBigFilesFirst(t *testing.T) {
	for _, c := range []struct {
		name    string
		perFile float64
		link    float64
	}{
		{"observed", 0.2, 9 << 20},
		{"slow files", 0.5, 9 << 20},
		{"slow link", 0.15, 2 << 20},
	} {
		t.Run(c.name, func(t *testing.T) {
			shownAt, ratios, at, total := bigFirstRun(t, c.perFile, c.link)
			if shownAt == 0 {
				t.Fatal("never shown")
			}
			t.Logf("run %v, first shown at %v", total.Round(time.Second), shownAt.Round(time.Second))
			var settleFrom time.Duration
			for i, r := range ratios {
				if i%600 == 0 {
					t.Logf("  at %v shown/true = %.2f", at[i].Round(time.Second), r)
				}
				if r < 0.5 || r > 2 {
					t.Errorf("at %v shown/true = %.2f, outside 0.5..2", at[i].Round(time.Second), r)
					break
				}
				if r < 0.75 || r > 1.25 {
					settleFrom = at[i]
				}
			}
			if settleFrom > 10*time.Minute {
				t.Errorf("still outside +-25%% of true at %v", settleFrom.Round(time.Second))
			}
		})
	}
}
