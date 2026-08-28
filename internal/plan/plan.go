// Package plan turns "what is on disk" plus "what we already uploaded" into an
// explicit list of work.
//
// The plan is complete before a single byte moves. That ordering is the whole
// reason the progress bar can be honest: the denominator is known up front and
// never changes. The tool this replaces discovered files while transferring
// them, so its total kept moving and its estimate was worthless.
package plan

import (
	"fmt"
	"sort"
	"time"

	"github.com/saurabhhbansal/r2backup/internal/scan"
)

// MTimeTolerance is how far two modification times may differ before the file
// counts as changed.
//
// FAT and exFAT store timestamps at two-second granularity while NTFS uses
// 100ns. Without a tolerance, every file on a FAT volume looks changed on every
// run and re-uploads forever.
const MTimeTolerance = 2 * time.Second

// MinMoveSize is the smallest file considered for move detection.
//
// Tiny files collide constantly on (size, mtime) -- a directory of generated
// stubs can hold hundreds that are byte-identical and written in the same
// second. Re-uploading a 200-byte file costs one operation, exactly what a
// server-side copy would cost, so there is nothing to win below this and a
// wrong guess would be silently wrong.
const MinMoveSize = 4096

// Prior is what a previous run recorded. The index satisfies this; tests use a
// map. Keeping it an interface means the planner can be tested without a
// database and without a network.
type Prior interface {
	// Each visits every recorded entry. Returning false stops iteration.
	Each(fn func(PriorEntry) bool) error
}

// PriorEntry is one object we believe is already in the bucket.
type PriorEntry struct {
	Key     string
	Size    int64
	ModTime time.Time
	Kind    scan.Kind
	Target  string
}

// Move is a file that appeared at a new key with contents we already hold, so
// the object can be copied server-side instead of uploaded again.
type Move struct {
	From string
	To   string
	Size int64
}

// Plan is the complete set of work for one run.
type Plan struct {
	// Uploads are new or changed files, in the order they will be sent.
	Uploads []scan.Entry
	// Moves are renames, satisfied by a server-side copy plus a delete.
	Moves []Move
	// Deletes are keys present remotely and gone locally.
	Deletes []string
	// Unchanged is how many files needed nothing. These cost zero operations,
	// which is why a quiet run is free.
	Unchanged int

	// UploadBytes is the exact byte total of Uploads. This is the ETA's
	// denominator and must never change once the plan is built.
	UploadBytes int64

	// overwrites counts uploads that replace an existing object, which is what
	// trash has to copy aside first.
	overwrites int
}

// Empty reports whether the run has nothing to do.
func (p *Plan) Empty() bool {
	return len(p.Uploads) == 0 && len(p.Moves) == 0 && len(p.Deletes) == 0
}

// Operations estimates the Class A operations this plan will consume, so the
// count can be shown against the free tier before anything is spent.
//
// An upload is one PUT. A move is one server-side copy; its delete is free. A
// delete is free on R2. Trash, when enabled, adds one copy for every object
// that is about to be overwritten or removed.
func (p *Plan) Operations(trash bool) int {
	ops := len(p.Uploads) + len(p.Moves)
	if trash {
		ops += len(p.Deletes) + p.overwrites
	}
	return ops
}

// Options tunes plan construction.
type Options struct {
	// Tolerance overrides MTimeTolerance. Zero uses the default.
	Tolerance time.Duration
	// DetectMoves enables rename detection. Without it, renaming a folder
	// re-uploads everything beneath it and trashes every original.
	DetectMoves bool
}

// Build diffs a scan against what a previous run recorded.
func Build(scanned *scan.Result, prior Prior, opts Options) (*Plan, error) {
	if scanned == nil {
		return nil, fmt.Errorf("plan: nil scan result")
	}
	tol := opts.Tolerance
	if tol == 0 {
		tol = MTimeTolerance
	}

	priorByKey := map[string]PriorEntry{}
	if prior != nil {
		if err := prior.Each(func(e PriorEntry) bool {
			priorByKey[e.Key] = e
			return true
		}); err != nil {
			return nil, fmt.Errorf("plan: read prior state: %w", err)
		}
	}

	p := &Plan{}
	seen := make(map[string]struct{}, len(scanned.Entries))
	var candidates []scan.Entry // new keys, possible move destinations

	for _, e := range scanned.Entries {
		seen[e.Key] = struct{}{}
		old, known := priorByKey[e.Key]
		switch {
		case !known:
			candidates = append(candidates, e)
		case changed(old, e, tol):
			p.Uploads = append(p.Uploads, e)
			p.UploadBytes += e.Size
			p.overwrites++
		default:
			p.Unchanged++
		}
	}

	// Anything recorded but no longer on disk is gone -- unless it turns out
	// to be the source of a move.
	var vanished []PriorEntry
	for key, e := range priorByKey {
		if _, still := seen[key]; !still {
			vanished = append(vanished, e)
		}
	}
	sort.Slice(vanished, func(i, j int) bool { return vanished[i].Key < vanished[j].Key })

	matched := map[string]bool{} // vanished keys consumed by a move
	if opts.DetectMoves {
		p.Moves, matched = detectMoves(candidates, vanished, tol)
	}

	movedTo := map[string]bool{}
	for _, m := range p.Moves {
		movedTo[m.To] = true
	}
	for _, e := range candidates {
		if movedTo[e.Key] {
			continue
		}
		p.Uploads = append(p.Uploads, e)
		p.UploadBytes += e.Size
	}
	for _, e := range vanished {
		if !matched[e.Key] {
			p.Deletes = append(p.Deletes, e.Key)
		}
	}

	// Largest first. A long upload that starts early finishes alongside the
	// short ones rather than being the lone straggler at the end, which makes
	// the ETA converge sooner and stay converged.
	sort.SliceStable(p.Uploads, func(i, j int) bool {
		if p.Uploads[i].Size != p.Uploads[j].Size {
			return p.Uploads[i].Size > p.Uploads[j].Size
		}
		return p.Uploads[i].Key < p.Uploads[j].Key
	})
	sort.Strings(p.Deletes)
	sort.Slice(p.Moves, func(i, j int) bool { return p.Moves[i].To < p.Moves[j].To })
	return p, nil
}

func changed(old PriorEntry, now scan.Entry, tol time.Duration) bool {
	if old.Kind != now.Kind {
		return true
	}
	if now.Kind == scan.KindSymlink {
		return old.Target != now.Target
	}
	if old.Size != now.Size {
		return true
	}
	d := old.ModTime.Sub(now.ModTime)
	return d > tol || d < -tol
}

// detectMoves pairs files that appeared with files that vanished, when they
// agree on size and modification time.
//
// A pairing is only accepted when it is unambiguous on both sides: exactly one
// vanished file and exactly one new file share that fingerprint. A copied tree
// is full of files that match each other, and guessing wrong would copy the
// wrong bytes to the wrong key -- an error a backup can never be allowed to
// make. When in doubt it falls back to a plain upload, which is merely slower.
func detectMoves(candidates []scan.Entry, vanished []PriorEntry, tol time.Duration) ([]Move, map[string]bool) {
	type fp struct {
		size int64
		sec  int64
	}
	bucket := func(size int64, t time.Time) fp {
		// Round to the tolerance window so two spellings of the same instant
		// land together.
		return fp{size: size, sec: t.Unix() / int64(tol/time.Second)}
	}

	gone := map[fp][]PriorEntry{}
	for _, e := range vanished {
		if e.Kind != scan.KindFile || e.Size < MinMoveSize {
			continue
		}
		k := bucket(e.Size, e.ModTime)
		gone[k] = append(gone[k], e)
	}

	fresh := map[fp][]scan.Entry{}
	for _, e := range candidates {
		if e.Kind != scan.KindFile || e.Size < MinMoveSize {
			continue
		}
		k := bucket(e.Size, e.ModTime)
		fresh[k] = append(fresh[k], e)
	}

	var moves []Move
	matched := map[string]bool{}
	for k, olds := range gone {
		news := fresh[k]
		if len(olds) != 1 || len(news) != 1 {
			continue // ambiguous; upload instead of guessing
		}
		moves = append(moves, Move{From: olds[0].Key, To: news[0].Key, Size: news[0].Size})
		matched[olds[0].Key] = true
	}
	return moves, matched
}
