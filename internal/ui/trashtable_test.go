package ui

import (
	"testing"
	"time"
)

// TestTrashTableShowsRecordedDeletionTime is the M5 regression: a row
// whose key genuinely carries a time of day (DeletedExact true) must show
// that time in the Deleted column, not a fabricated "00:00" that happens
// to fall out of formatting a date-only value.
func TestTrashTableShowsRecordedDeletionTime(t *testing.T) {
	b := twoSets()
	m := sized(b, 100, 30)
	m.Update(trashMsg{set: "Documents", rows: []TrashRow{
		{
			Key: "notes.txt", Size: 12,
			Deleted:      time.Date(2026, 8, 15, 14, 30, 22, 0, time.UTC),
			DeletedExact: true,
			Expires:      time.Date(2026, 9, 14, 0, 0, 0, 0, time.UTC),
		},
	}})

	rows := m.trash.Rows()
	if len(rows) != 1 {
		t.Fatalf("got %d trash rows, want 1", len(rows))
	}
	const deletedCol = 2
	got := rows[0][deletedCol]
	want := "15 Aug 14:30"
	if got != want {
		t.Errorf("Deleted column = %q, want %q (real time of day must be shown, not 00:00)", got, want)
	}
}

// TestTrashTableOmitsTimeWhenNoneWasRecorded covers the day-resolution
// fallback: a row whose key never carried a time of day (a foreign object
// found under trash/<date>/... rather than one this program moved there)
// must render with the date alone. Printing any HH:MM for it -- even
// "00:00" -- would claim a precision nothing ever measured.
func TestTrashTableOmitsTimeWhenNoneWasRecorded(t *testing.T) {
	b := twoSets()
	m := sized(b, 100, 30)
	m.Update(trashMsg{set: "Documents", rows: []TrashRow{
		{
			Key: "foreign.bin", Size: 12,
			Deleted:      time.Date(2026, 8, 15, 0, 0, 0, 0, time.UTC),
			DeletedExact: false,
			Expires:      time.Date(2026, 9, 14, 0, 0, 0, 0, time.UTC),
		},
	}})

	rows := m.trash.Rows()
	if len(rows) != 1 {
		t.Fatalf("got %d trash rows, want 1", len(rows))
	}
	const deletedCol = 2
	got := rows[0][deletedCol]
	want := "15 Aug"
	if got != want {
		t.Errorf("Deleted column = %q, want %q (no time of day was ever recorded, so none should be shown)", got, want)
	}
}
