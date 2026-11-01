package bundle

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/saurabhhbansal/r2backup/internal/remote"
)

// failPacks is a store whose next few pack uploads fail.
type failPacks struct {
	*memStore
	fail atomic.Int32
}

func (f *failPacks) Put(ctx context.Context, in remote.PutInput) error {
	if strings.Contains(in.Key, packsDir) && f.fail.Add(-1) >= 0 {
		return errors.New("connection reset")
	}
	return f.memStore.Put(ctx, in)
}

// A pack that fails to upload takes every file already added to it down
// with it. Those files' own Puts said yes, so the run must not then believe
// them safe: Flush has to fail, or the backup records them as uploaded and
// never sends them again.
func TestAPackThatFailsToSendFailsTheFlush(t *testing.T) {
	base := &failPacks{memStore: newMem()}
	base.fail.Store(1)
	s := New(base, small(t))
	ctx := context.Background()
	var ok []string
	sealErr := false
	for i := 0; i < 40; i++ {
		key := fmt.Sprintf("%scurrent/f%02d.txt", ns, i)
		body := noise(i, 300)
		err := s.Put(ctx, remote.PutInput{Key: key, Body: strings.NewReader(body), Size: int64(len(body))})
		if err != nil {
			sealErr = true
			continue
		}
		ok = append(ok, key)
	}
	if !sealErr {
		t.Fatal("no pack upload failed; the test is not testing anything")
	}
	err := s.Flush(ctx)
	if err == nil {
		// Every file whose Put said yes must be readable from a fresh store.
		s2 := New(base, small(t))
		for _, k := range ok {
			if _, gerr := get(t, s2, k); gerr != nil {
				t.Fatalf("Flush succeeded but %s, accepted by Put, is gone: %v", k, gerr)
			}
		}
	}
}

// The tray keeps one Store for its whole life, while scheduled backups run
// in processes of their own. An index read before such a run must not be
// written back over it afterwards.
func TestAnotherProcessesWritesSurviveALongLivedStore(t *testing.T) {
	base := newMem()
	ctx := context.Background()

	tray := New(base, small(t))
	put(t, tray, ns+"current/a.txt", "alpha")
	if err := tray.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	// The panel lists the backup (reads the index) and stays open.
	_ = keys(t, tray, ns)

	// A scheduled run, another process, backs up b.
	sched := New(base, small(t))
	put(t, sched, ns+"current/b.txt", "bravo")
	if err := sched.Flush(ctx); err != nil {
		t.Fatal(err)
	}

	// Then "Back up now" in the tray: a run starts by flushing (see
	// backup.Run), then writes and flushes.
	if err := tray.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	put(t, tray, ns+"current/c.txt", "charlie")
	if err := tray.Flush(ctx); err != nil {
		t.Fatal(err)
	}

	fresh := New(base, small(t))
	for _, k := range []string{"a", "b", "c"} {
		if _, err := get(t, fresh, ns+"current/"+k+".txt"); err != nil {
			t.Errorf("%s.txt lost: %v", k, err)
		}
	}
}

// Moving a file onto a key whose newer content is still in the pack being
// filled must leave the moved content there, not the pending one.
func TestMovingOntoAKeyStillInTheOpenPack(t *testing.T) {
	base := newMem()
	s := New(base, small(t))
	ctx := context.Background()
	put(t, s, ns+"current/a.txt", "old-a")
	if err := s.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	put(t, s, ns+"current/b.txt", "pending-b")
	if err := s.Move(ctx, ns+"current/a.txt", ns+"current/b.txt"); err != nil {
		t.Fatal(err)
	}
	if got := mustGet(t, s, ns+"current/b.txt"); got != "old-a" {
		t.Fatalf("before flush b = %q, want old-a", got)
	}
	if err := s.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if got := mustGet(t, New(base, small(t)), ns+"current/b.txt"); got != "old-a" {
		t.Fatalf("after flush b = %q, want old-a", got)
	}
}
