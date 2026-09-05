// Package cost estimates what a month of R2 usage costs, and decides when a
// spending limit has been reached.
//
// Every number this package produces is an estimate, and callers must present
// it as one. Cloudflare does publish a billable-usage API, but it is in beta
// behind an allowlist and -- decisively -- there is no billing scope in the
// OAuth catalogue, so no amount of signing in would reach it. What is left is
// arithmetic: r2backup's own usage, priced at R2's published rates.
//
// That has a blind spot worth stating plainly wherever these figures are
// shown. This package only ever sees what r2backup itself did. Another tool
// writing to the same bucket, a second bucket on the account, Workers, D1 --
// all of it is invisible here and all of it lands on the same bill. So the
// estimate is a floor, not a forecast, and the free-tier allowances below are
// shared with everything this package cannot see.
//
// Money is float64 rather than integer cents. The inputs are a fractional
// number of GB-months and a per-million operation rate, so cent precision
// would be precision this arithmetic does not have; rounding belongs at the
// point of display, and a budget compared to the nearest cent would be
// comparing against a figure that is already approximate by more than that.
package cost

import (
	"time"
)

// R2's published Standard-class rates, in US dollars.
//
// They are constants rather than anything fetched, because a backup tool that
// cannot price its own storage without a network call has invented a new way
// to fail. When Cloudflare changes a rate this needs a release; a wrong
// estimate for a few weeks is a far smaller problem than a dashboard that
// blocks on an HTTP request.
const (
	StorageUSDPerGBMonth = 0.015
	ClassAUSDPerMillion  = 4.50
	ClassBUSDPerMillion  = 0.36
)

// R2's free allowances, per calendar month, on the Standard class.
//
// FreeClassAOps duplicates index.FreeTierOpsPerMonth rather than importing
// it. The index counts operations to keep a running total and does not care
// what they cost; this package prices them and does not care how they were
// counted. Wiring the two together would make a change to either one's
// reason for existing a change to both.
const (
	FreeStorageGBMonths = 10.0
	FreeClassAOps       = 1_000_000
	FreeClassBOps       = 10_000_000
)

// bytesPerGB is R2's own unit: decimal gigabytes, not gibibytes. Using 2^30
// here would under-report storage by about 7% and make every estimate quietly
// optimistic.
const bytesPerGB = 1_000_000_000.0

// Usage is a month's consumption so far, in the units R2 bills in.
type Usage struct {
	// StorageGBMonths is storage accrued, not storage held. R2 bills
	// storage over time, so 100 GB kept for half a month is 50 GB-months.
	// Build it with GBMonths or AccrueGBMonths rather than by hand.
	StorageGBMonths float64

	// ClassAOps are the writes and lists: PutObject, UploadPart,
	// CompleteMultipartUpload, CopyObject, ListObjectsV2. These are the
	// expensive ones and the ones r2backup makes most of.
	ClassAOps int64

	// ClassBOps are the reads: GetObject, HeadObject. In practice these
	// come from restores, so most months they are zero.
	ClassBOps int64
}

// Breakdown is what a Usage costs, split so a person can see which part of
// their bill is which -- the whole point of showing it. A total alone tells
// someone they are spending money; the split tells them whether the answer is
// to back up less often or to keep less trash.
type Breakdown struct {
	StorageUSD float64
	ClassAUSD  float64
	ClassBUSD  float64
	TotalUSD   float64

	// The amounts left after the free tier, which are what actually cost
	// anything. Showing these next to the allowance is how someone sees
	// they are still inside it.
	BillableGBMonths float64
	BillableClassA   int64
	BillableClassB   int64

	// WithinFreeTier is true when nothing here costs anything yet. It is
	// worth its own field because it is the common case and deserves to be
	// said outright rather than inferred from a total of zero.
	WithinFreeTier bool
}

// Price applies the free tier and the published rates.
func Price(u Usage) Breakdown {
	var b Breakdown

	b.BillableGBMonths = max0(u.StorageGBMonths - FreeStorageGBMonths)
	b.BillableClassA = max0i(u.ClassAOps - FreeClassAOps)
	b.BillableClassB = max0i(u.ClassBOps - FreeClassBOps)

	b.StorageUSD = b.BillableGBMonths * StorageUSDPerGBMonth
	b.ClassAUSD = float64(b.BillableClassA) / 1_000_000 * ClassAUSDPerMillion
	b.ClassBUSD = float64(b.BillableClassB) / 1_000_000 * ClassBUSDPerMillion
	b.TotalUSD = b.StorageUSD + b.ClassAUSD + b.ClassBUSD
	b.WithinFreeTier = b.TotalUSD == 0

	return b
}

// GBMonths converts "this many bytes, held for this long" into the GB-months
// R2 bills for.
//
// A month here is 30 days rather than the calendar's own answer. The
// alternative is making the price of storing a byte depend on whether it was
// stored in February, which is true of the real bill but is noise in an
// estimate -- and noise that moves the number a person is watching by a few
// percent for reasons that have nothing to do with anything they did.
func GBMonths(bytes int64, held time.Duration) float64 {
	if bytes <= 0 || held <= 0 {
		return 0
	}
	const month = 30 * 24 * time.Hour
	return float64(bytes) / bytesPerGB * (float64(held) / float64(month))
}

// Sample is one observation of how much was stored at a moment in time.
type Sample struct {
	At    time.Time
	Bytes int64
}

// AccrueGBMonths integrates a series of size observations into GB-months.
//
// This exists because the obvious approach -- take today's size, assume it
// held all month -- is wrong in the direction that matters. Someone who adds
// a large folder on the 28th would see the whole month priced as though they
// had stored it since the 1st, and a spending limit built on that figure
// would trip on storage they have not had long enough to be charged for.
//
// So each sample is held to be true until the next one, and the last is held
// until upTo. Samples need not be sorted or evenly spaced; anything at or
// after upTo is ignored, since a run cannot have observed the future.
func AccrueGBMonths(samples []Sample, upTo time.Time) float64 {
	if len(samples) == 0 {
		return 0
	}
	ordered := make([]Sample, 0, len(samples))
	for _, s := range samples {
		if s.At.Before(upTo) {
			ordered = append(ordered, s)
		}
	}
	if len(ordered) == 0 {
		return 0
	}
	sortSamples(ordered)

	var total float64
	for i, s := range ordered {
		end := upTo
		if i+1 < len(ordered) {
			end = ordered[i+1].At
		}
		total += GBMonths(s.Bytes, end.Sub(s.At))
	}
	return total
}

// Project extrapolates a month-to-date figure to the end of the month.
//
// It is deliberately the crudest possible model -- the rest of the month
// looks like the part that has happened -- because a cleverer one would be
// guessing with more confidence, not more accuracy. Early in a month the
// projection is nearly meaningless, which is why callers should show it as a
// projection and never as the figure a limit is enforced against.
func Project(monthToDate float64, now time.Time) float64 {
	elapsed := MonthElapsed(now)
	if elapsed <= 0 {
		return monthToDate
	}
	return monthToDate / elapsed
}

// MonthElapsed is how much of now's calendar month has passed, from 0 to 1.
func MonthElapsed(now time.Time) float64 {
	now = now.UTC()
	start := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	next := start.AddDate(0, 1, 0)
	total := next.Sub(start)
	if total <= 0 {
		return 0
	}
	elapsed := now.Sub(start)
	switch {
	case elapsed <= 0:
		return 0
	case elapsed >= total:
		return 1
	default:
		return float64(elapsed) / float64(total)
	}
}

func max0(f float64) float64 {
	if f < 0 {
		return 0
	}
	return f
}

func max0i(n int64) int64 {
	if n < 0 {
		return 0
	}
	return n
}

// sortSamples orders by time. It is an insertion sort because these slices
// hold one entry per day at most -- thirty-odd items, nearly always already
// in order -- and pulling in sort for that is more machinery than the job.
func sortSamples(s []Sample) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j].At.Before(s[j-1].At); j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}
