package backup_test

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/saurabhhbansal/r2backup/internal/backup"
	"github.com/saurabhhbansal/r2backup/test/fixtures"
)

// The safety net, proven against a real server: an overwritten file must still
// be recoverable afterwards. This is the whole reason trash exists, so it is
// asserted on actual bytes rather than on a call count.
func TestOverwritingAFileKeepsTheOldVersion(t *testing.T) {
	h := setup(t, fixtures.Spec{SmallFiles: 12, SmallFileSize: 256, Seed: 77})

	runWithTrash := func() *backup.Report {
		t.Helper()
		rep, err := backup.Run(context.Background(), backup.Options{
			Set:    h.set,
			Index:  h.db,
			Client: h.client,
			Trash:  backup.NewTrash(h.client, h.set.RetentionDays),
		})
		if err != nil {
			t.Fatalf("backup.Run: %v", err)
		}
		return rep
	}
	runWithTrash()

	// Pick a real file, remember what was in it, then overwrite it.
	var victim string
	for k := range h.liveKeys(t) {
		if strings.Contains(k, "/current/pkg/") {
			victim = strings.TrimPrefix(k, h.set.Prefix+"/current/")
			break
		}
	}
	if victim == "" {
		t.Fatal("no file to overwrite")
	}
	onDisk := filepath.Join(h.root, filepath.FromSlash(victim))
	original, err := os.ReadFile(onDisk)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(onDisk, []byte("completely different contents"), 0o644); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(time.Hour)
	if err := os.Chtimes(onDisk, future, future); err != nil {
		t.Fatal(err)
	}

	rep := runWithTrash()
	if rep.Uploaded != 1 {
		t.Fatalf("Uploaded = %d, want exactly the one edited file", rep.Uploaded)
	}

	// The old version must be sitting in trash, byte for byte.
	entries, err := h.client.List(context.Background(), h.set.Prefix+"/trash/")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) == 0 {
		t.Fatal("nothing was moved to trash; an overwrite destroyed the only copy")
	}

	var found bool
	for _, e := range entries {
		obj, err := h.client.Get(context.Background(), e.Key)
		if err != nil {
			t.Fatal(err)
		}
		got, err := io.ReadAll(obj.Body)
		obj.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		if string(got) == string(original) {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("the previous contents of %q are not recoverable from trash", victim)
	}

	// And the live object is the new version, not the old one.
	obj, err := h.client.Get(context.Background(), h.set.Prefix+"/current/"+victim)
	if err != nil {
		t.Fatal(err)
	}
	defer obj.Body.Close()
	live, _ := io.ReadAll(obj.Body)
	if string(live) != "completely different contents" {
		t.Error("the live object was not replaced with the new version")
	}
}

func TestRetentionZeroKeepsNoHistory(t *testing.T) {
	// Some sets are pure build output. Thirty days of it is waste, and the
	// extra copy per changed file is a real operation each time.
	h := setup(t, fixtures.Spec{SmallFiles: 6, SmallFileSize: 128, Seed: 4})
	h.set.RetentionDays = 0

	if tr := backup.NewTrash(h.client, h.set.RetentionDays); tr != nil {
		t.Fatal("retention 0 must produce no trash at all, not an enabled one")
	}
	if h.set.TrashEnabled() {
		t.Error("TrashEnabled() should be false at retention 0")
	}
}
