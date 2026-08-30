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

// A run that was stopped rather than finished is a state of its own, and it
// used to read as "no run at all": every reader checked Stale and then threw
// away what it had found. This is what lets the interface say an upload is
// paused instead of showing nothing.
func TestAnInterruptedRunIsToldApartFromNoRunAndFromALiveOne(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "progress.json")
	now := time.Now()

	if _, ok := ReadInterrupted(path, now); ok {
		t.Error("a machine that has never run anything has no interrupted run")
	}

	// A live run: this process, updated a moment ago.
	if err := WriteLive(path, Live{Set: "Photos", BytesDone: 5, BytesTotal: 10}); err != nil {
		t.Fatal(err)
	}
	if _, ok := ReadInterrupted(path, now); ok {
		t.Error("a run that is happening now is not an interrupted one")
	}

	// The same file left behind by a process that is gone.
	l, err := ReadLive(path)
	if err != nil {
		t.Fatal(err)
	}
	l.PID = 0x7FFFFFFE // a pid that cannot exist
	if err := writeJSON(path, l); err != nil {
		t.Fatal(err)
	}
	got, ok := ReadInterrupted(path, time.Now().Add(time.Minute))
	if !ok {
		t.Fatal("a progress file whose process is gone is an interrupted run")
	}
	if got.Set != "Photos" || got.BytesDone != 5 {
		t.Errorf("read back %+v, want Photos at 5 of 10", got)
	}

	// And once the run is finished properly, there is nothing to report.
	if err := ClearLive(path); err != nil {
		t.Fatal(err)
	}
	if _, ok := ReadInterrupted(path, time.Now()); ok {
		t.Error("a run that finished and cleared up is not interrupted")
	}
}

// Pids are reused. After a reboot the number in an old progress file can
// belong to something else entirely, and asking only whether a process holds
// it would report a run that died with the machine as still going.
func TestAReusedPidDoesNotHideAnInterruptedRun(t *testing.T) {
	path := filepath.Join(t.TempDir(), "progress.json")
	// This process is certainly alive, standing in for whatever inherited
	// the pid -- and the file has not been touched for an hour.
	if err := writeJSON(path, Live{
		Set: "Photos", PID: os.Getpid(),
		UpdatedAt: time.Now().Add(-time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	if _, ok := ReadInterrupted(path, time.Now()); !ok {
		t.Error("an hour-old progress file is an interrupted run whatever its pid resolves to")
	}
}
