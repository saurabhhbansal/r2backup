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

// recordingTrash wraps the real adapter so a run's use of it can be asserted.
type recordingTrash struct {
	inner        backup.Trash
	prunedPrefix []string
	pruneOps     int
	pruneErr     error
}

func (r *recordingTrash) Move(ctx context.Context, prefix string, keys []string) error {
	return r.inner.Move(ctx, prefix, keys)
}

func (r *recordingTrash) Prune(ctx context.Context, prefix string) (backup.Pruned, error) {
	r.prunedPrefix = append(r.prunedPrefix, prefix)
	if r.pruneErr != nil {
		return backup.Pruned{}, r.pruneErr
	}
	return backup.Pruned{Keys: 7, Ops: r.pruneOps, Dates: []string{"2026-07-01"}}, nil
}

// Nothing enforced retention. trash.Prune was written, documented and covered
// by six of its own tests, and no code path anywhere called it -- so every
// "recoverable until <date>" that `trash ls` printed was a date nothing acted
// on, and trash grew forever in a product whose whole argument is cost. There
// is no daemon, so a run is the only thing that can do this.
//
// The failure was the wiring, not the pruning, so this asserts the wiring:
// that a run calls Prune, for its own prefix, every time.
func TestARunExpiresOldTrash(t *testing.T) {
	h := setup(t, fixtures.Spec{SmallFiles: 4, SmallFileSize: 128, Seed: 91})
	tr := &recordingTrash{inner: backup.NewTrash(h.client, h.set.RetentionDays), pruneOps: 3}

	rep, err := backup.Run(context.Background(), backup.Options{
		Set: h.set, Index: h.db, Client: h.client, Trash: tr,
	})
	if err != nil {
		t.Fatalf("backup.Run: %v", err)
	}
	if len(tr.prunedPrefix) != 1 {
		t.Fatalf("Prune called %d times, want exactly 1: nothing else ever expires trash", len(tr.prunedPrefix))
	}
	if tr.prunedPrefix[0] != h.set.Prefix {
		t.Errorf("pruned %q, want the set's own prefix %q", tr.prunedPrefix[0], h.set.Prefix)
	}
	if rep.Pruned.Keys != 7 {
		t.Errorf("Report.Pruned.Keys = %d, want 7", rep.Pruned.Keys)
	}
	if rep.PruneErr != nil {
		t.Errorf("PruneErr = %v, want nil", rep.PruneErr)
	}

	// A second run the same day must NOT sweep again. Finding what to expire
	// costs a listing, and "a run where nothing changed costs nothing" is
	// this product's headline claim; once a day is what holds both.
	if _, err := backup.Run(context.Background(), backup.Options{
		Set: h.set, Index: h.db, Client: h.client, Trash: tr,
	}); err != nil {
		t.Fatalf("second backup.Run: %v", err)
	}
	if len(tr.prunedPrefix) != 1 {
		t.Errorf("Prune called %d times over two runs on one day, want 1", len(tr.prunedPrefix))
	}

	// Tomorrow it sweeps again, whether or not anything changed in between:
	// trash expires by the calendar, not by activity. A set that never
	// changes never trashes anything either, so a sweep that only happened
	// after a change would never come for it.
	h.db.Now = func() time.Time { return time.Now().AddDate(0, 0, 1) }
	if _, err := backup.Run(context.Background(), backup.Options{
		Set: h.set, Index: h.db, Client: h.client, Trash: tr,
	}); err != nil {
		t.Fatalf("next day's backup.Run: %v", err)
	}
	if len(tr.prunedPrefix) != 2 {
		t.Errorf("Prune called %d times including the next day, want 2: an unchanged set would keep its trash forever", len(tr.prunedPrefix))
	}
}

// Finding what to expire costs Class A operations, and the operations budget
// is this product's headline claim. A cost the counter cannot see is a cost
// the user is told they are not paying.
func TestPruningIsCountedAgainstTheOperationsBudget(t *testing.T) {
	h := setup(t, fixtures.Spec{SmallFiles: 4, SmallFileSize: 128, Seed: 92})
	tr := &recordingTrash{inner: backup.NewTrash(h.client, h.set.RetentionDays), pruneOps: 5}

	rep, err := backup.Run(context.Background(), backup.Options{
		Set: h.set, Index: h.db, Client: h.client, Trash: tr,
	})
	if err != nil {
		t.Fatalf("backup.Run: %v", err)
	}
	if rep.Pruned.Ops != 5 {
		t.Fatalf("Report.Pruned.Ops = %d, want 5", rep.Pruned.Ops)
	}
	if rep.Operations < 5 {
		t.Errorf("Operations = %d, which cannot include the %d the prune spent",
			rep.Operations, rep.Pruned.Ops)
	}
}

// A backup that uploaded everything correctly has succeeded, whatever
// happened to last month's trash afterwards. But it must not be silent: a
// retention window nothing enforces is the bug this whole change exists to
// fix, so the failure is carried back rather than dropped.
func TestAFailedPruneIsReportedAndDoesNotFailTheRun(t *testing.T) {
	h := setup(t, fixtures.Spec{SmallFiles: 4, SmallFileSize: 128, Seed: 93})
	boom := io.ErrUnexpectedEOF
	tr := &recordingTrash{inner: backup.NewTrash(h.client, h.set.RetentionDays), pruneErr: boom}

	rep, err := backup.Run(context.Background(), backup.Options{
		Set: h.set, Index: h.db, Client: h.client, Trash: tr,
	})
	if err != nil {
		t.Fatalf("a prune failure must not fail the run: %v", err)
	}
	if rep.PruneErr == nil {
		t.Fatal("a prune failure was swallowed; nothing would ever surface it")
	}
	if !rep.Succeeded() {
		t.Error("Succeeded() is false, but every file the plan asked for was uploaded")
	}
}

// The pricing claim and the retention claim pull against each other, and this
// is where both are held. Expiring trash costs a listing, so sweeping on every
// run would make an idle run cost money -- and "a run where nothing changed
// costs nothing" is the argument this whole product is built on. Sweeping once
// a UTC day is the resolution.
//
// TestSecondBackupOfAnUnchangedTreeIsFree makes the pricing claim with
// Trash: nil, so it never saw this interaction at all. This is the same claim
// with trash switched on, which is how every real set is configured.
func TestAnIdleRunIsStillFreeWithTrashEnabled(t *testing.T) {
	h := setup(t, fixtures.Spec{SmallFiles: 20, SmallFileSize: 256, Seed: 94})
	run := func() *backup.Report {
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

	if first := run(); first.Uploaded == 0 {
		t.Fatal("first run uploaded nothing")
	}
	second := run()
	if second.Uploaded != 0 {
		t.Fatalf("second run uploaded %d, want 0", second.Uploaded)
	}
	if second.Operations != 0 {
		t.Errorf("an idle run with trash enabled cost %d operations, want 0", second.Operations)
	}
	if second.PruneErr != nil {
		t.Errorf("PruneErr = %v, want nil", second.PruneErr)
	}

	// The day rolls over, and the sweep is due again -- so it is no longer
	// free, and that is the price of a retention window that is real. One
	// listing per set per day.
	h.db.Now = func() time.Time { return time.Now().AddDate(0, 0, 1) }
	tomorrow := run()
	if tomorrow.Uploaded != 0 {
		t.Errorf("the next day's idle run uploaded %d, want 0", tomorrow.Uploaded)
	}
	if tomorrow.Operations == 0 {
		t.Error("the first idle run of a new day cost nothing, so no sweep happened and trash would never expire")
	}
}
