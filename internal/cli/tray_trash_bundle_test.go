package cli

import (
	"bytes"
	"context"
	"os"
	"testing"

	"github.com/saurabhhbansal/r2backup/internal/bundle"
	"github.com/saurabhhbansal/r2backup/internal/remote"
)

// Deleting from the trash of a bundled backup is an edit to its index. The
// panel must save that edit, and must make it to the index as it is now: a
// scheduled backup (another process) may have changed it since the panel
// last listed the trash.
func TestPanelTrashDeleteOnABundledBackupIsSavedAndKeepsOtherRunsFiles(t *testing.T) {
	main, _ := parityWorld(t, false, folder(t, "Docs"))
	a, err := openApp()
	if err != nil {
		t.Fatal(err)
	}
	c := b2("main")
	c.Bundle = true
	if err := a.creds.Save(c); err != nil {
		t.Fatal(err)
	}
	a.close()

	ctx := context.Background()
	other := func(keys ...string) { // another r2b process writing to the backup
		t.Helper()
		s := bundle.New(main, bundle.Options{Pack: true, TempDir: t.TempDir()})
		defer s.Close()
		for _, k := range keys {
			body := "data of " + k
			if err := s.Put(ctx, remote.PutInput{Key: k, Body: bytes.NewReader([]byte(body)), Size: int64(len(body))}); err != nil {
				t.Fatal(err)
			}
		}
		if err := s.Flush(ctx); err != nil {
			t.Fatal(err)
		}
	}
	ka, kb, kc := trashKey("Docs", "a.txt"), trashKey("Docs", "b.txt"), trashKey("Docs", "c.txt")
	other(ka, kb)

	d, err := openDashboard(&Options{Out: os.Stderr, Err: os.Stderr})
	if err != nil {
		t.Fatal(err)
	}
	defer d.close()
	tb := &trayBackend{d: d}

	if _, err := tb.trashRows(ctx, ""); err != nil { // the panel shows the trash
		t.Fatal(err)
	}
	other(kc) // a scheduled backup trashes c.txt meanwhile
	if n, err := tb.trashDelete(ctx, "Docs", "a.txt", wherePrimary); err != nil || n != 1 {
		t.Fatalf("trashDelete = %d, %v", n, err)
	}

	fresh := bundle.New(main, bundle.Options{TempDir: t.TempDir()})
	defer fresh.Close()
	got := map[string]bool{}
	list, err := fresh.List(ctx, "machines/testpc/Docs/")
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range list {
		got[o.Key] = true
	}
	if got[ka] {
		t.Error("a.txt is still in the trash: the delete was never saved")
	}
	if !got[kb] || !got[kc] {
		t.Errorf("trash lost files it should keep: b=%v c=%v", got[kb], got[kc])
	}
}
