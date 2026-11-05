package backup_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/saurabhhbansal/r2backup/internal/backup"
	"github.com/saurabhhbansal/r2backup/internal/bundle"
	"github.com/saurabhhbansal/r2backup/internal/index"
	"github.com/saurabhhbansal/r2backup/internal/remote"
	"github.com/saurabhhbansal/r2backup/internal/sets"
)

// A backup cut off partway -- a crash, a kill, a shutdown, the lid closing --
// used to lose the record of every file it had finished, because the index
// was written once, at the end. The next run sent all of them again. These
// tests stop a run with files still to go and check that what it finished is
// in the index while it is still running, and that the next run does not
// send any of it again.

// stallStore lets the first allow uploads through and holds every later one
// until the run is stopped, standing in for a run cut off partway. It counts
// which backed-up files were sent.
type stallStore struct {
	remote.Store
	allow     int32 // uploads let through
	unlimited bool

	mu   sync.Mutex
	sent map[string]int
}

func newStallStore(s remote.Store, allow int32) *stallStore {
	return &stallStore{Store: s, allow: allow, unlimited: allow < 0, sent: map[string]int{}}
}

func (s *stallStore) Put(ctx context.Context, in remote.PutInput) error {
	if strings.Contains(in.Key, "/current/") {
		if !s.unlimited && atomic.AddInt32(&s.allow, -1) < 0 {
			<-ctx.Done()
			return ctx.Err()
		}
		s.mu.Lock()
		s.sent[in.Key]++
		s.mu.Unlock()
	}
	return s.Store.Put(ctx, in)
}

func (s *stallStore) Flush(ctx context.Context) error { return remote.FlushStore(ctx, s.Store) }
func (s *stallStore) Pending() bool                   { return remote.PendingStore(s.Store) }
func (s *stallStore) Checkpoint(ctx context.Context) error {
	ok, err := remote.CheckpointStore(ctx, s.Store)
	if err == nil && !ok {
		err = remote.ErrNoCheckpoint
	}
	return err
}

func (s *stallStore) sentKeys() map[string]int {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]int{}
	for k, v := range s.sent {
		out[k] = v
	}
	return out
}

func writeFiles(t *testing.T, root string, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		name := filepath.Join(root, fmt.Sprintf("f%02d.txt", i))
		if err := os.WriteFile(name, []byte("contents of file "+name), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// waitForRecords polls the index until it holds n records for set.
func waitForRecords(t *testing.T, db *index.DB, set string, n int) []index.Record {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		recs, err := db.All(set)
		if err != nil {
			t.Fatal(err)
		}
		if len(recs) >= n {
			return recs
		}
		if time.Now().After(deadline) {
			t.Fatalf("the index holds %d of the %d finished files while the run is going; finished files are not being noted as they land", len(recs), n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func checkpointCase(t *testing.T, newStore func() remote.Store, afterCheckpoint func(t *testing.T, recs []index.Record)) {
	defer backup.SetCheckpointEvery(10*time.Millisecond, 30*time.Millisecond)()
	const total, finished = 20, 8

	root := t.TempDir()
	writeFiles(t, root, total)
	db, err := index.Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	set := sets.Set{Name: "Docs", Prefix: "machines/pc/Docs", Root: root, Machine: "pc"}

	// First run: eight files go up, the rest hang until it is stopped.
	first := newStallStore(newStore(), finished)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := backup.Run(ctx, backup.Options{Set: set, Index: db, Client: first})
		done <- err
	}()

	// While the run is still going -- the moment a kill would lose them --
	// the finished files are already in the index.
	recs := waitForRecords(t, db, set.Name, finished)
	if afterCheckpoint != nil {
		afterCheckpoint(t, recs)
	}
	cancel()
	if err := <-done; !errors.Is(err, backup.ErrCancelled) {
		t.Fatalf("the stopped run returned %v, want ErrCancelled", err)
	}
	firstSent := first.sentKeys()
	if len(firstSent) != finished {
		t.Fatalf("the first run sent %d files, want %d", len(firstSent), finished)
	}

	// Second run, as a new process would: nothing it finished goes again.
	second := newStallStore(newStore(), -1)
	rep, err := backup.Run(context.Background(), backup.Options{Set: set, Index: db, Client: second})
	if err != nil {
		t.Fatal(err)
	}
	againSent := second.sentKeys()
	for k := range firstSent {
		if againSent[k] > 0 {
			t.Errorf("%s was sent again, though the run before finished it", k)
		}
	}
	if rep.Uploaded != total-finished || len(againSent) != total-finished {
		t.Errorf("the second run sent %d files (report says %d), want the %d the first did not finish", len(againSent), rep.Uploaded, total-finished)
	}
	all, err := db.All(set.Name)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != total {
		t.Errorf("the index holds %d files after both runs, want %d", len(all), total)
	}
}

func TestARunCutOffPartwayKeepsWhatItFinished(t *testing.T) {
	base := newMemStore()
	checkpointCase(t, func() remote.Store { return base }, nil)
}

// With bundling, a small file is not safe when its Put returns: it sits in a
// pack being filled, and is only on record once that pack is sent and the
// backup's bundle index saved. A file must not be in the local index before
// then, or a run killed in between would never send it again.
func TestABundledRunCutOffPartwayKeepsWhatItFinished(t *testing.T) {
	base := newMemStore()
	opts := bundle.Options{Pack: true, TempDir: t.TempDir()}
	var stores []*bundle.Store
	defer func() {
		for _, s := range stores {
			s.Close()
		}
	}()
	newStore := func() remote.Store {
		s := bundle.New(base, opts)
		stores = append(stores, s)
		return s
	}
	checkpointCase(t, newStore, func(t *testing.T, recs []index.Record) {
		// Read the backup as a new process would, right now: every file
		// the index says is backed up must be there.
		fresh := bundle.New(base, opts)
		defer fresh.Close()
		list, err := fresh.List(context.Background(), "machines/pc/Docs/current/")
		if err != nil {
			t.Fatal(err)
		}
		have := map[string]bool{}
		for _, o := range list {
			have[strings.TrimPrefix(o.Key, "machines/pc/Docs/current/")] = true
		}
		for _, r := range recs {
			if !have[r.Key] {
				t.Errorf("%s is in the index but not yet in the backup; a run killed now would never send it", r.Key)
			}
		}
	})
}
