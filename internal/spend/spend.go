// Package spend assembles what a month has cost from what the index recorded.
//
// It exists because two callers need the same answer and neither should own
// it. A backup has to know whether a spending limit has been reached before it
// uploads; the dashboard has to show the same figure the backup acted on. If
// each worked it out separately they would disagree eventually -- and the
// disagreement would surface as a backup that stopped for a reason the screen
// did not show.
//
// It is also the seam that keeps the two sides apart. internal/index stores
// usage and knows no prices; internal/cost knows prices and no storage. This
// package is the only place that imports both.
package spend

import (
	"fmt"
	"time"

	"github.com/saurabhhbansal/r2backup/internal/cost"
	"github.com/saurabhhbansal/r2backup/internal/index"
)

// Snapshot is one reading of the month so far.
type Snapshot struct {
	// StoredBytes and Objects are what the index believes is in the bucket.
	// See index.StoredBytes for what that excludes.
	StoredBytes int64
	Objects     int64

	ClassAOps int
	ClassBOps int

	// OpsLimit and ClassBLimit are the free allowances, carried here so a
	// caller can draw "used against free" without importing cost itself.
	OpsLimit    int
	ClassBLimit int

	// ResetAt is when the counters roll over to a new month.
	ResetAt time.Time

	Usage cost.Usage
	Cost  cost.Breakdown

	// Projected is the month-end estimate. It is for display only: the
	// budget is enforced against Cost.TotalUSD, which has actually
	// happened. See cost.Project.
	Projected float64

	Budget  cost.Budget
	Verdict cost.Verdict
}

// EstimatedUSD is the month-to-date figure, which is what a limit is measured
// against. Named rather than left as a field access because "which of these
// numbers does the budget use" is the question this whole package exists to
// have one answer to.
func (s Snapshot) EstimatedUSD() float64 { return s.Cost.TotalUSD }

// Read builds a Snapshot from the index.
//
// The storage figure comes from the daily samples rather than from the size
// right now, so data added late in the month is charged for the days it has
// actually been stored. A month with no samples yet -- a fresh install, or the
// first of the month before any backup has run -- falls back to treating the
// current size as observed from the start of today, which is the least wrong
// thing available and errs low.
func Read(db *index.DB, budget cost.Budget, now time.Time) (Snapshot, error) {
	if db == nil {
		return Snapshot{}, fmt.Errorf("spend: no index")
	}
	var s Snapshot
	var err error

	s.StoredBytes, s.Objects, err = db.StoredBytes()
	if err != nil {
		return Snapshot{}, err
	}
	s.ClassAOps, s.ResetAt, err = db.OpsThisMonth()
	if err != nil {
		return Snapshot{}, err
	}
	s.ClassBOps, _, err = db.ClassBOpsThisMonth()
	if err != nil {
		return Snapshot{}, err
	}
	samples, err := db.StorageSamples()
	if err != nil {
		return Snapshot{}, err
	}

	s.OpsLimit = cost.FreeClassAOps
	s.ClassBLimit = cost.FreeClassBOps

	s.Usage = cost.Usage{
		StorageGBMonths: accrue(samples, s.StoredBytes, now),
		ClassAOps:       int64(s.ClassAOps),
		ClassBOps:       int64(s.ClassBOps),
	}
	s.Cost = cost.Price(s.Usage)
	s.Projected = cost.Project(s.Cost.TotalUSD, now)
	s.Budget = budget
	s.Verdict = budget.Check(s.Cost.TotalUSD, now)
	return s, nil
}

// accrue turns the index's samples into GB-months, falling back when there are
// none yet.
func accrue(samples []index.StorageSample, currentBytes int64, now time.Time) float64 {
	if len(samples) > 0 {
		converted := make([]cost.Sample, 0, len(samples))
		for _, s := range samples {
			converted = append(converted, cost.Sample{At: s.At, Bytes: s.Bytes})
		}
		return cost.AccrueGBMonths(converted, now)
	}
	if currentBytes <= 0 {
		return 0
	}
	// No history: charge for today only. Assuming the current size held all
	// month would overstate a fresh install's first reading by up to
	// thirtyfold, and this figure is what a spending limit reads.
	startOfDay := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	return cost.GBMonths(currentBytes, now.UTC().Sub(startOfDay))
}
