package drime

import (
	"context"
	"testing"
	"time"

	"github.com/saurabhhbansal/r2backup/internal/remote"
)

// A long-lived Client (the tray's) must not write its old copy of a set's
// file details over what another process (a scheduled backup) saved since.
func TestALongLivedClientKeepsAnotherProcessesFileDetails(t *testing.T) {
	f := newFake(t)
	ctx := context.Background()
	tray := newClient(t, f, nil)
	sched := newClient(t, f, nil)

	when := time.Unix(1_700_000_000, 0)
	put(t, tray, "machines/pc/Docs/current/a.txt", []byte("a"), remote.Metadata{ModTime: when})
	if err := tray.Flush(ctx); err != nil {
		t.Fatal(err)
	}

	put(t, sched, "machines/pc/Docs/current/b.txt", []byte("b"), remote.Metadata{ModTime: when})
	if err := sched.Flush(ctx); err != nil {
		t.Fatal(err)
	}

	// The tray records something else and saves.
	put(t, tray, "machines/pc/Docs/current/c.txt", []byte("c"), remote.Metadata{ModTime: when})
	if err := tray.Flush(ctx); err != nil {
		t.Fatal(err)
	}

	fresh := newClient(t, f, nil)
	for _, k := range []string{"a.txt", "b.txt", "c.txt"} {
		h, err := fresh.Head(ctx, "machines/pc/Docs/current/"+k)
		if err != nil {
			t.Fatal(err)
		}
		if !h.Metadata.ModTime.Equal(when) {
			t.Errorf("%s lost its date: %v", k, h.Metadata.ModTime)
		}
	}
}
