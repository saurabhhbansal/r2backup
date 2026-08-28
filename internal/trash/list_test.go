package trash

import (
	"context"
	"testing"
	"time"
)

func TestListReportsCorrectExpiryDates(t *testing.T) {
	fb := newFakeBackend()
	fb.seed(buildTrashKey("myset", "docs/report.pdf", mustParseDate("2026-08-01")), 500)
	fb.seed(buildTrashKey("myset", "images/logo.png", mustParseDate("2026-08-15")), 2048)

	tr := New(fb, fixedClock(time.Now()))
	entries, err := tr.List(context.Background(), "myset", 30)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("got %d entries, want 2: %+v", len(entries), entries)
	}

	byPath := make(map[string]Entry)
	for _, e := range entries {
		byPath[e.RelPath] = e
	}

	report, ok := byPath["docs/report.pdf"]
	if !ok {
		t.Fatal("missing entry for docs/report.pdf")
	}
	if !report.TrashedOn.Equal(mustParseDate("2026-08-01")) {
		t.Errorf("report TrashedOn = %v, want 2026-08-01", report.TrashedOn)
	}
	wantExpiry := mustParseDate("2026-08-31") // +30 days
	if !report.ExpiresOn.Equal(wantExpiry) {
		t.Errorf("report ExpiresOn = %v, want %v", report.ExpiresOn, wantExpiry)
	}
	if report.Size != 500 {
		t.Errorf("report Size = %d, want 500", report.Size)
	}

	logo, ok := byPath["images/logo.png"]
	if !ok {
		t.Fatal("missing entry for images/logo.png")
	}
	wantLogoExpiry := mustParseDate("2026-09-14") // +30 days
	if !logo.ExpiresOn.Equal(wantLogoExpiry) {
		t.Errorf("logo ExpiresOn = %v, want %v", logo.ExpiresOn, wantLogoExpiry)
	}
}

func TestListIgnoresARetentionChangeUntilCalledAgain(t *testing.T) {
	// ExpiresOn is a projection made at read time from whatever
	// retentionDays List is called with -- it is not stored on the
	// object -- so calling List again with a different retentionDays
	// must change the reported expiry for the very same trashed object.
	fb := newFakeBackend()
	fb.seed(buildTrashKey("myset", "a.txt", mustParseDate("2026-08-01")), 10)

	tr := New(fb, fixedClock(time.Now()))

	entries30, err := tr.List(context.Background(), "myset", 30)
	if err != nil {
		t.Fatalf("List(30): %v", err)
	}
	entries7, err := tr.List(context.Background(), "myset", 7)
	if err != nil {
		t.Fatalf("List(7): %v", err)
	}
	if entries30[0].ExpiresOn.Equal(entries7[0].ExpiresOn) {
		t.Errorf("ExpiresOn did not change between retentionDays=30 and retentionDays=7")
	}
}

func TestListHonoursContextCancellation(t *testing.T) {
	fb := newFakeBackend()
	fb.seed(buildTrashKey("myset", "a.txt", mustParseDate("2026-08-01")), 10)

	tr := New(fb, fixedClock(time.Now()))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := tr.List(ctx, "myset", 30)
	if err == nil {
		t.Fatal("List with a canceled context returned nil error")
	}
}
