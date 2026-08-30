package remote

import "testing"

func TestPartSizeForSmallFiles(t *testing.T) {
	// Well under the 10,000-part cap at the default part size: no scaling.
	sizes := []int64{0, 1, multipartThreshold, multipartThreshold + 1, 10 << 30 /* 10 GiB */}
	for _, size := range sizes {
		got := partSizeFor(size, defaultPartSize)
		if got != defaultPartSize {
			t.Errorf("partSizeFor(%d) = %d, want defaultPartSize %d", size, got, defaultPartSize)
		}
	}
}

func TestPartSizeForScalesAboveCap(t *testing.T) {
	// One byte past exactly maxParts * defaultPartSize needs the part size
	// to grow -- at defaultPartSize it would take maxParts+1 parts.
	size := int64(maxParts)*int64(defaultPartSize) + 1

	got := partSizeFor(size, defaultPartSize)
	if got <= defaultPartSize {
		t.Fatalf("partSizeFor(%d) = %d, want > defaultPartSize %d", size, got, defaultPartSize)
	}

	numParts := (size + got - 1) / got
	if numParts > maxParts {
		t.Errorf("partSizeFor(%d) = %d implies %d parts, want <= %d", size, got, numParts, maxParts)
	}
}

func TestPartSizeForVeryLargeFile(t *testing.T) {
	// A hypothetical 5TiB object -- comfortably past R2's per-object limits
	// in practice, but partSizeFor must still return a part size that keeps
	// the part count at or under the cap rather than silently producing an
	// upload plan that R2 would reject.
	const fiveTiB = int64(5) << 40

	got := partSizeFor(fiveTiB, defaultPartSize)
	numParts := (fiveTiB + got - 1) / got
	if numParts > maxParts {
		t.Errorf("partSizeFor(%d) = %d implies %d parts, want <= %d", fiveTiB, got, numParts, maxParts)
	}
	if got%(1<<20) != 0 {
		t.Errorf("partSizeFor(%d) = %d, want a whole-MiB part size", fiveTiB, got)
	}
}

func TestPartSizeForNeverBelowSDKMinimum(t *testing.T) {
	// Guard against a pathological input producing a part size the SDK
	// itself would refuse (its own floor is 5MiB); partSizeFor only grows
	// the part size, so this mostly documents the invariant.
	got := partSizeFor(1, defaultPartSize)
	if got < 5<<20 {
		t.Errorf("partSizeFor(1, defaultPartSize) = %d, below the SDK's 5MiB minimum part size", got)
	}
}
