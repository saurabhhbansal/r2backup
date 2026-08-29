package runstate

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func tmp(t *testing.T, name string) string {
	t.Helper()
	return filepath.Join(t.TempDir(), name)
}

func TestLiveRoundTrip(t *testing.T) {
	p := tmp(t, "progress.json")
	want := Live{Set: "Code Projects", Phase: "uploading", BytesDone: 214, BytesTotal: 340,
		FilesDone: 1166, FilesTotal: 1847, ByteRate: 18.4e6, ETASeconds: 432, ETAKnown: true}
	if err := WriteLive(p, want); err != nil {
		t.Fatal(err)
	}
	got, err := ReadLive(p)
	if err != nil {
		t.Fatal(err)
	}
	if got.Set != want.Set || got.BytesDone != want.BytesDone || !got.ETAKnown {
		t.Errorf("round trip lost data: %+v", got)
	}
	if got.PID != os.Getpid() {
		t.Errorf("PID = %d, want this process %d -- a watcher needs it to tell a dead run from a live one", got.PID, os.Getpid())
	}
	if got.UpdatedAt.IsZero() {
		t.Error("UpdatedAt was not stamped")
	}
}

func TestReadLiveWithNoRun(t *testing.T) {
	if _, err := ReadLive(tmp(t, "nothing.json")); !errors.Is(err, ErrNoRun) {
		t.Fatalf("got %v, want ErrNoRun", err)
	}
}

func TestStaleDetectsADeadRun(t *testing.T) {
	// A killed process leaves its last update behind forever. Showing that as
	// live progress is the "a stored status is a claim, not an answer" trap.
	now := time.Now()

	fresh := Live{PID: os.Getpid(), UpdatedAt: now}
	if fresh.Stale(now) {
		t.Error("a run that just wrote, from a living process, is not stale")
	}

	old := Live{PID: os.Getpid(), UpdatedAt: now.Add(-time.Minute)}
	if !old.Stale(now) {
		t.Error("a run that has not written for a minute is stale even if the PID lives")
	}

	// A PID that cannot exist. Recent timestamp, dead process.
	dead := Live{PID: 0x7FFFFFFE, UpdatedAt: now}
	if !dead.Stale(now) {
		t.Error("a recent update from a process that no longer exists is stale")
	}
}

func TestCorruptFileReadsAsNoRunRatherThanAnError(t *testing.T) {
	// The user asked what happened. "Nothing recorded" is a better answer
	// than a JSON parse error.
	p := tmp(t, "torn.json")
	if err := os.WriteFile(p, []byte(`{"set": "half a fi`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadLive(p); !errors.Is(err, ErrNoRun) {
		t.Fatalf("got %v, want ErrNoRun", err)
	}
	h, err := ReadHistory(p)
	if err != nil {
		t.Fatalf("ReadHistory on a torn file errored: %v", err)
	}
	if len(h.Runs) != 0 {
		t.Error("a torn history should read as empty")
	}
}

func TestClearLiveIsIdempotent(t *testing.T) {
	p := tmp(t, "progress.json")
	if err := WriteLive(p, Live{Set: "x"}); err != nil {
		t.Fatal(err)
	}
	if err := ClearLive(p); err != nil {
		t.Fatal(err)
	}
	if err := ClearLive(p); err != nil {
		t.Fatalf("clearing an already-absent file should not error: %v", err)
	}
}

func TestHistoryKeepsNewestFirstAndBounded(t *testing.T) {
	p := tmp(t, "history.json")
	base := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)

	for i := 0; i < MaxHistory+15; i++ {
		if err := Record(p, Past{Set: "one", FinishedAt: base.Add(time.Duration(i) * time.Minute), Uploaded: i}); err != nil {
			t.Fatal(err)
		}
	}
	h, err := ReadHistory(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(h.Runs) != MaxHistory {
		t.Fatalf("kept %d runs, want %d", len(h.Runs), MaxHistory)
	}
	for i := 1; i < len(h.Runs); i++ {
		if h.Runs[i].FinishedAt.After(h.Runs[i-1].FinishedAt) {
			t.Fatalf("history is not newest-first at %d", i)
		}
	}
	if h.Runs[0].Uploaded != MaxHistory+14 {
		t.Errorf("the newest run was not kept: Uploaded = %d", h.Runs[0].Uploaded)
	}
}

func TestLastFindsThePerSetRun(t *testing.T) {
	p := tmp(t, "history.json")
	base := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	for _, r := range []Past{
		{Set: "Docs", FinishedAt: base, Uploaded: 1},
		{Set: "Code", FinishedAt: base.Add(time.Hour), Uploaded: 2},
		{Set: "Docs", FinishedAt: base.Add(2 * time.Hour), Uploaded: 3},
	} {
		if err := Record(p, r); err != nil {
			t.Fatal(err)
		}
	}
	h, _ := ReadHistory(p)
	got, ok := h.Last("Docs")
	if !ok {
		t.Fatal("no run found for Docs")
	}
	if got.Uploaded != 3 {
		t.Errorf("Last returned the older run: Uploaded = %d, want 3", got.Uploaded)
	}
	if _, ok := h.Last("Never"); ok {
		t.Error("Last found a run for a set that has none")
	}
}

func TestPastOKRequiresACleanRun(t *testing.T) {
	cases := map[string]struct {
		p    Past
		want bool
	}{
		"clean":      {Past{}, true},
		"failures":   {Past{Failures: 1}, false},
		"collisions": {Past{Collisions: 1}, false},
		"error":      {Past{Error: "root missing"}, false},
		// Problems are files that could not be read. The run still completed,
		// and the rest of the folder is backed up.
		"problems only": {Past{Problems: 3}, true},
	}
	for name, tc := range cases {
		if got := tc.p.OK(); got != tc.want {
			t.Errorf("%s: OK() = %v, want %v", name, got, tc.want)
		}
	}
}

func TestWriteIsAtomic(t *testing.T) {
	p := tmp(t, "progress.json")
	if err := WriteLive(p, Live{Set: "x"}); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Dir(p))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("expected only progress.json, found %v -- a temp file was left for a reader to trip over", names)
	}
}
