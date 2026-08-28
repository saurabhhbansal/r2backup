package plan

import (
	"testing"
	"time"

	"github.com/saurabhhbansal/r2backup/internal/scan"
)

var base = time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)

// mapPrior is the whole reason Prior is an interface: the planner is tested
// without a database and without a network.
type mapPrior map[string]PriorEntry

func (m mapPrior) Each(fn func(PriorEntry) bool) error {
	for _, e := range m {
		if !fn(e) {
			return nil
		}
	}
	return nil
}

func file(key string, size int64, mod time.Time) scan.Entry {
	return scan.Entry{Key: key, Size: size, ModTime: mod, Kind: scan.KindFile}
}

func prior(key string, size int64, mod time.Time) PriorEntry {
	return PriorEntry{Key: key, Size: size, ModTime: mod, Kind: scan.KindFile}
}

func result(entries ...scan.Entry) *scan.Result {
	r := &scan.Result{Entries: entries}
	for _, e := range entries {
		if e.Kind == scan.KindFile {
			r.Files++
			r.Bytes += e.Size
		}
	}
	return r
}

func build(t *testing.T, r *scan.Result, p Prior, opts Options) *Plan {
	t.Helper()
	pl, err := Build(r, p, opts)
	if err != nil {
		t.Fatal(err)
	}
	return pl
}

func keysOf(entries []scan.Entry) []string {
	out := make([]string, len(entries))
	for i, e := range entries {
		out[i] = e.Key
	}
	return out
}

func TestFirstRunUploadsEverything(t *testing.T) {
	p := build(t, result(
		file("a.txt", 100, base),
		file("b/c.txt", 200, base),
	), nil, Options{})

	if len(p.Uploads) != 2 {
		t.Fatalf("Uploads = %d, want 2", len(p.Uploads))
	}
	if p.UploadBytes != 300 {
		t.Errorf("UploadBytes = %d, want 300", p.UploadBytes)
	}
	if p.Unchanged != 0 || len(p.Deletes) != 0 {
		t.Errorf("a first run should have nothing unchanged and nothing to delete")
	}
}

func TestUnchangedFilesCostNothing(t *testing.T) {
	// The claim in the design doc: a run where nothing changed costs zero
	// operations. If this test ever fails, the pricing argument fails with it.
	p := build(t, result(
		file("a.txt", 100, base),
		file("b.txt", 200, base),
	), mapPrior{
		"a.txt": prior("a.txt", 100, base),
		"b.txt": prior("b.txt", 200, base),
	}, Options{})

	if !p.Empty() {
		t.Fatalf("expected an empty plan, got %d uploads, %d moves, %d deletes",
			len(p.Uploads), len(p.Moves), len(p.Deletes))
	}
	if p.Unchanged != 2 {
		t.Errorf("Unchanged = %d, want 2", p.Unchanged)
	}
	if got := p.Operations(true); got != 0 {
		t.Errorf("Operations = %d, want 0 -- an idle run must be free", got)
	}
}

func TestModTimeToleranceAbsorbsFilesystemGranularity(t *testing.T) {
	cases := []struct {
		name    string
		skew    time.Duration
		changed bool
	}{
		{"identical", 0, false},
		{"one second later", time.Second, false},
		{"exactly the tolerance", 2 * time.Second, false},
		{"one second before", -time.Second, false},
		{"three seconds later", 3 * time.Second, true},
		{"three seconds before", -3 * time.Second, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := build(t, result(file("a.txt", 100, base.Add(tc.skew))),
				mapPrior{"a.txt": prior("a.txt", 100, base)}, Options{})
			got := len(p.Uploads) == 1
			if got != tc.changed {
				t.Errorf("skew %s: changed = %v, want %v", tc.skew, got, tc.changed)
			}
		})
	}
}

func TestSizeChangeIsAlwaysAChange(t *testing.T) {
	// Same timestamp, one byte different. Editors that preserve mtime exist.
	p := build(t, result(file("a.txt", 101, base)),
		mapPrior{"a.txt": prior("a.txt", 100, base)}, Options{})
	if len(p.Uploads) != 1 {
		t.Fatal("a size change with an unchanged mtime must still re-upload")
	}
}

func TestVanishedFilesBecomeDeletes(t *testing.T) {
	p := build(t, result(file("keep.txt", 10, base)), mapPrior{
		"keep.txt": prior("keep.txt", 10, base),
		"gone.txt": prior("gone.txt", 20, base),
	}, Options{})

	if len(p.Deletes) != 1 || p.Deletes[0] != "gone.txt" {
		t.Fatalf("Deletes = %v, want [gone.txt]", p.Deletes)
	}
}

func TestMoveDetectionTurnsARenameIntoACopy(t *testing.T) {
	// Renaming src/ to source/ must not re-upload the bytes.
	p := build(t, result(file("source/big.bin", 1<<20, base)), mapPrior{
		"src/big.bin": prior("src/big.bin", 1<<20, base),
	}, Options{DetectMoves: true})

	if len(p.Moves) != 1 {
		t.Fatalf("Moves = %v, want one move", p.Moves)
	}
	m := p.Moves[0]
	if m.From != "src/big.bin" || m.To != "source/big.bin" {
		t.Errorf("move = %s -> %s, want src/big.bin -> source/big.bin", m.From, m.To)
	}
	if len(p.Uploads) != 0 {
		t.Errorf("a detected move must not also upload: %v", keysOf(p.Uploads))
	}
	if len(p.Deletes) != 0 {
		t.Errorf("the move source must not also be deleted: %v", p.Deletes)
	}
	if p.UploadBytes != 0 {
		t.Errorf("UploadBytes = %d, want 0 -- a move transfers no bytes", p.UploadBytes)
	}
}

func TestMoveDetectionIsOffByDefault(t *testing.T) {
	p := build(t, result(file("source/big.bin", 1<<20, base)), mapPrior{
		"src/big.bin": prior("src/big.bin", 1<<20, base),
	}, Options{})
	if len(p.Moves) != 0 {
		t.Fatal("move detection must be opt-in")
	}
	if len(p.Uploads) != 1 || len(p.Deletes) != 1 {
		t.Errorf("without detection this is an upload plus a delete")
	}
}

func TestAmbiguousMatchesUploadRatherThanGuess(t *testing.T) {
	// Two files vanish and two appear inside the SAME directory, all sharing
	// one fingerprint. The directory detector cannot help -- the folder did
	// not move, and its contents no longer match -- so this falls to
	// fingerprint pairing, where it is a coin flip.
	//
	// Copying the wrong bytes to the wrong key is an error a backup may never
	// make, so it uploads instead. That is merely slower.
	p := build(t, result(
		file("data/gamma.bin", 1<<20, base),
		file("data/delta.bin", 1<<20, base),
	), mapPrior{
		"data/alpha.bin": prior("data/alpha.bin", 1<<20, base),
		"data/beta.bin":  prior("data/beta.bin", 1<<20, base),
	}, Options{DetectMoves: true})

	if len(p.Moves) != 0 {
		t.Fatalf("ambiguous fingerprints must not be paired, got %v", p.Moves)
	}
	if len(p.Uploads) != 2 {
		t.Errorf("Uploads = %d, want 2 -- falling back to upload is the safe answer", len(p.Uploads))
	}
	if len(p.Deletes) != 2 {
		t.Errorf("Deletes = %d, want 2", len(p.Deletes))
	}
}

func TestTinyFilesAreNotFingerprintMatched(t *testing.T) {
	// Fingerprint matching pairs files by (size, mtime) alone. Below
	// MinMoveSize that is weak evidence -- a directory of generated stubs
	// holds hundreds of identical ones -- and a copy costs the same as an
	// upload, so there is nothing to win by guessing.
	//
	// The directory detector is a separate case: matching on the whole
	// contents of a folder is strong evidence regardless of file size, so it
	// deliberately has no size floor. The shape here (two files leave, one
	// arrives) cannot be a directory rename, which isolates the fingerprint
	// path.
	p := build(t, result(file("new/stub.txt", 64, base)), mapPrior{
		"old/stub.txt":  prior("old/stub.txt", 64, base),
		"old/other.txt": prior("old/other.txt", 64, base),
	}, Options{DetectMoves: true})
	if len(p.Moves) != 0 {
		t.Fatalf("a 64-byte file should not be fingerprint-matched, got %v", p.Moves)
	}
}

func TestDirectoryMovesCoverSmallFilesToo(t *testing.T) {
	// Renaming a folder full of tiny files is exactly the node_modules case.
	// The bytes are already in the bucket; re-uploading them because each one
	// is small would be the waste this detection exists to prevent.
	p := build(t, result(
		file("packages/a.js", 200, base),
		file("packages/b.js", 300, base),
		file("packages/c.js", 400, base),
	), mapPrior{
		"pkg/a.js": prior("pkg/a.js", 200, base),
		"pkg/b.js": prior("pkg/b.js", 300, base),
		"pkg/c.js": prior("pkg/c.js", 400, base),
	}, Options{DetectMoves: true})

	if len(p.Moves) != 3 {
		t.Fatalf("Moves = %v, want all three -- the folder plainly moved", p.Moves)
	}
	if len(p.Uploads) != 0 || p.UploadBytes != 0 {
		t.Errorf("a directory rename re-uploaded %d files (%d bytes)", len(p.Uploads), p.UploadBytes)
	}
	if len(p.Deletes) != 0 {
		t.Errorf("the move sources must not also be deleted: %v", p.Deletes)
	}
}

func TestDirectoryMoveNeedsAnExactContentMatch(t *testing.T) {
	// One file differs in size, so this is not the same folder somewhere else.
	// Failing closed here means an upload, which is merely slower; failing
	// open would mean copying the wrong bytes to the wrong key.
	p := build(t, result(
		file("packages/a.js", 200, base),
		file("packages/b.js", 999, base),
	), mapPrior{
		"pkg/a.js": prior("pkg/a.js", 200, base),
		"pkg/b.js": prior("pkg/b.js", 300, base),
	}, Options{DetectMoves: true})

	if len(p.Moves) != 0 {
		t.Fatalf("an imperfect directory match must not be treated as a move, got %v", p.Moves)
	}
}

func TestTwoFilesNormalizingToOneKeyAreReportedNotDropped(t *testing.T) {
	// Linux holds "résumé.txt" precomposed and decomposed as two files; both
	// normalize to one object key and only one can be stored. Quietly keeping
	// one is the sort of silent incompleteness this tool must be incapable of.
	dup := file("unicode/résumé.txt", 10, base)
	p := build(t, result(dup, dup), nil, Options{})

	if len(p.Collisions) != 1 {
		t.Fatalf("Collisions = %v, want the clash reported", p.Collisions)
	}
	if len(p.Uploads) != 1 {
		t.Errorf("Uploads = %d, want 1 -- only one file can occupy the key", len(p.Uploads))
	}
	if p.Collisions[0].Key != "unicode/résumé.txt" {
		t.Errorf("Collision key = %q", p.Collisions[0].Key)
	}
}

func TestSymlinkRetargetIsAChange(t *testing.T) {
	now := scan.Entry{Key: "link", Kind: scan.KindSymlink, ModTime: base, Target: "../new"}
	p := build(t, result(now), mapPrior{
		"link": {Key: "link", Kind: scan.KindSymlink, ModTime: base, Target: "../old"},
	}, Options{})
	if len(p.Uploads) != 1 {
		t.Fatal("a symlink pointing somewhere new must be re-recorded")
	}
}

func TestSymlinkPointingSomewhereStableIsUnchanged(t *testing.T) {
	now := scan.Entry{Key: "link", Kind: scan.KindSymlink, ModTime: base, Target: "../same"}
	p := build(t, result(now), mapPrior{
		"link": {Key: "link", Kind: scan.KindSymlink, ModTime: base, Target: "../same"},
	}, Options{})
	if !p.Empty() {
		t.Fatalf("expected nothing to do, got %d uploads", len(p.Uploads))
	}
}

func TestKindChangeIsAChange(t *testing.T) {
	// A path that was a file and is now a symlink is not the same object.
	now := scan.Entry{Key: "thing", Kind: scan.KindSymlink, ModTime: base, Target: "elsewhere"}
	p := build(t, result(now), mapPrior{
		"thing": prior("thing", 100, base),
	}, Options{})
	if len(p.Uploads) != 1 {
		t.Fatal("a file replaced by a symlink must be re-recorded")
	}
}

func TestUploadsAreOrderedLargestFirst(t *testing.T) {
	p := build(t, result(
		file("small.bin", 10, base),
		file("huge.bin", 1<<30, base),
		file("medium.bin", 1<<20, base),
	), nil, Options{})

	got := keysOf(p.Uploads)
	want := []string{"huge.bin", "medium.bin", "small.bin"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("upload order = %v, want %v -- starting the long one early is what makes the ETA converge", got, want)
		}
	}
}

func TestOperationCountReflectsTrash(t *testing.T) {
	p := build(t, result(
		file("changed.txt", 200, base.Add(time.Hour)), // overwrites
		file("new.txt", 50, base),                     // fresh
	), mapPrior{
		"changed.txt": prior("changed.txt", 100, base),
		"gone.txt":    prior("gone.txt", 10, base),
	}, Options{})

	// Without trash: 2 PUTs. The delete is free on R2.
	if got := p.Operations(false); got != 2 {
		t.Errorf("Operations(false) = %d, want 2", got)
	}
	// With trash: 2 PUTs, plus a copy for the overwrite and a copy for the
	// delete.
	if got := p.Operations(true); got != 4 {
		t.Errorf("Operations(true) = %d, want 4", got)
	}
}

func TestPlanIsDeterministic(t *testing.T) {
	r := result(file("b.txt", 10, base), file("a.txt", 10, base), file("c.txt", 10, base))
	pr := mapPrior{"x.txt": prior("x.txt", 1, base), "y.txt": prior("y.txt", 2, base)}

	first := build(t, r, pr, Options{})
	for i := 0; i < 20; i++ {
		next := build(t, r, pr, Options{})
		if len(next.Uploads) != len(first.Uploads) {
			t.Fatal("upload count varied between identical builds")
		}
		for j := range first.Uploads {
			if first.Uploads[j].Key != next.Uploads[j].Key {
				t.Fatalf("upload order varied at %d: %q vs %q", j, first.Uploads[j].Key, next.Uploads[j].Key)
			}
		}
		for j := range first.Deletes {
			if first.Deletes[j] != next.Deletes[j] {
				t.Fatalf("delete order varied at %d", j)
			}
		}
	}
}

func TestNilScanIsAnError(t *testing.T) {
	if _, err := Build(nil, nil, Options{}); err == nil {
		t.Fatal("Build(nil) should error rather than panic")
	}
}
