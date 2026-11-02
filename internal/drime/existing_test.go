package drime

import (
	"context"
	"errors"
	"testing"
)

// A folder to read from is opened only if it is there: a typo must not make
// a new, empty folder in someone's Drime.
func TestExistingRefusesAMissingFolderAndCreatesNothing(t *testing.T) {
	f := newFake(t)
	backups := f.add("", "Backups", "folder", nil)
	f.add(backups.ID.String(), "Laptop", "folder", nil)

	_, err := New(context.Background(), Config{Token: f.token, BaseURL: f.srv.URL, Root: "Backups/Lpatop", Existing: true, MaxAttempts: 1})
	if !errors.Is(err, ErrFolderNotFound) {
		t.Fatalf("got %v, want ErrFolderNotFound", err)
	}
	for _, e := range f.entries {
		if e.Name == "Lpatop" {
			t.Fatal("the mistyped folder was created")
		}
	}

	if _, err := New(context.Background(), Config{Token: f.token, BaseURL: f.srv.URL, Root: "Backups/Laptop", Existing: true, MaxAttempts: 1}); err != nil {
		t.Fatalf("an existing folder was refused: %v", err)
	}
}
