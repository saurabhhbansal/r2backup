package pause

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestMarkerNamesOneRun(t *testing.T) {
	path := Path(filepath.Join(t.TempDir(), "progress.json"))
	if For(path, os.Getpid()) {
		t.Fatal("paused with no marker")
	}
	if err := Set(path, os.Getpid()); err != nil {
		t.Fatal(err)
	}
	if !For(path, os.Getpid()) {
		t.Error("the run the marker names is not paused")
	}
	if For(path, os.Getpid()+1) {
		t.Error("a marker for one run paused another")
	}
	if err := Clear(path); err != nil {
		t.Fatal(err)
	}
	if For(path, os.Getpid()) {
		t.Error("still paused after Clear")
	}
	if err := Clear(path); err != nil {
		t.Errorf("clearing what is not there: %v", err)
	}
}

// A marker written before this process started -- a run paused, then the
// computer restarted, and the pid reused -- holds nothing.
func TestAMarkerFromBeforeThisProcessIsIgnored(t *testing.T) {
	path := Path(filepath.Join(t.TempDir(), "progress.json"))
	data, _ := json.Marshal(marker{PID: os.Getpid(), At: started.Add(-time.Hour)})
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if For(path, os.Getpid()) {
		t.Error("an old marker with a reused pid paused this run")
	}
}

func TestWatcherHoldsAndReleases(t *testing.T) {
	path := Path(filepath.Join(t.TempDir(), "progress.json"))
	w := Watch(path, os.Getpid(), 10*time.Millisecond)
	defer w.Close()

	ctx := NewContext(context.Background(), w)
	if err := Wait(ctx); err != nil {
		t.Fatalf("Wait while not paused: %v", err)
	}

	if err := Set(path, os.Getpid()); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for !w.Paused() {
		if time.Now().After(deadline) {
			t.Fatal("the watcher never saw the marker")
		}
		time.Sleep(5 * time.Millisecond)
	}

	released := make(chan error, 1)
	go func() { released <- Wait(ctx) }()
	select {
	case <-released:
		t.Fatal("Wait returned while paused")
	case <-time.After(50 * time.Millisecond):
	}
	if err := Clear(path); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-released:
		if err != nil {
			t.Errorf("Wait after resuming: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Wait never returned after resuming")
	}
}

func TestWaitEndsWithTheRun(t *testing.T) {
	path := Path(filepath.Join(t.TempDir(), "progress.json"))
	if err := Set(path, os.Getpid()); err != nil {
		t.Fatal(err)
	}
	w := Watch(path, os.Getpid(), time.Hour)
	defer w.Close()
	ctx, cancel := context.WithCancel(NewContext(context.Background(), w))
	go func() { time.Sleep(20 * time.Millisecond); cancel() }()
	if err := Wait(ctx); err == nil {
		t.Error("Wait on a cancelled run returned no error")
	}
}
