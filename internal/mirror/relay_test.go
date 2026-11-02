package mirror

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/saurabhhbansal/r2backup/internal/remote"
)

// fakeRelay copies within the test's stores directly, and can be told to
// refuse, fail or stop.
type fakeRelay struct {
	mu      sync.Mutex
	refuse  func(key string) bool  // answers ErrNoLink
	fail    func(key string) error // any other error
	copied  []string
	partial int64 // bytes reported before failing, to check progress is put back
}

func (f *fakeRelay) Copy(ctx context.Context, src, dst remote.Store, key string, size int64, onBytes func(int64)) error {
	if f.refuse != nil && f.refuse(key) {
		return fmt.Errorf("small: %w", remote.ErrNoLink)
	}
	if f.fail != nil {
		if err := f.fail(key); err != nil {
			if f.partial > 0 {
				onBytes(f.partial)
			}
			return err
		}
	}
	obj, err := src.Get(ctx, key)
	if err != nil {
		return err
	}
	defer obj.Body.Close()
	if err := dst.Put(ctx, remote.PutInput{Key: key, Body: obj.Body, Size: size, Metadata: obj.Metadata}); err != nil {
		return err
	}
	onBytes(size)
	f.mu.Lock()
	f.copied = append(f.copied, key)
	f.mu.Unlock()
	return nil
}

func relayFixture(t *testing.T, n int) (src, dst *mem) {
	t.Helper()
	src = newMem(time.Unix(1000, 0))
	dst = newMem(time.Unix(5000, 0))
	for i := 0; i < n; i++ {
		src.put(t, fmt.Sprintf("machines/pc/Docs/current/f%02d", i), strings.Repeat("x", i+1))
	}
	return src, dst
}

// progressMax keeps the furthest progress reported; workers report
// concurrently, so the last call is not necessarily the furthest.
type progressMax struct {
	mu   sync.Mutex
	done int64
	tot  int64
}

func (p *progressMax) see(q Progress) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if q.BytesDone > p.done {
		p.done = q.BytesDone
	}
	p.tot = q.BytesTotal
}

func checkAllCopied(t *testing.T, src, dst *mem) {
	t.Helper()
	for k, o := range src.objs {
		if dst.content(k) != string(o.data) {
			t.Fatalf("%s did not arrive", k)
		}
	}
}

func TestRelayCopiesWhatItTakesAndThePCTheRest(t *testing.T) {
	src, dst := relayFixture(t, 10)
	r := &fakeRelay{refuse: func(k string) bool { return strings.HasSuffix(k, "0") }} // f00, f10... are "small"
	var last progressMax
	rep, err := Run(context.Background(), Options{Src: src, Dst: dst, Prefixes: []string{"machines/"}, Relay: r,
		Progress: last.see})
	if err != nil {
		t.Fatal(err)
	}
	checkAllCopied(t, src, dst)
	if rep.Copied != 10 || rep.Relayed != 9 || rep.RelayFellBack != 0 || rep.RelayStopped != nil {
		t.Fatalf("report = %+v", rep)
	}
	if last.done != last.tot {
		t.Fatalf("progress ended at %d of %d", last.done, last.tot)
	}
}

func TestRelayFailureFallsBackForThatFile(t *testing.T) {
	src, dst := relayFixture(t, 6)
	r := &fakeRelay{partial: 1, fail: func(k string) error {
		if strings.HasSuffix(k, "f03") {
			return errors.New("upload answered 500")
		}
		return nil
	}}
	var last progressMax
	rep, err := Run(context.Background(), Options{Src: src, Dst: dst, Prefixes: []string{"machines/"}, Relay: r, Concurrency: 1,
		Progress: last.see})
	if err != nil {
		t.Fatal(err)
	}
	checkAllCopied(t, src, dst)
	if rep.Relayed != 5 || rep.RelayFellBack != 1 || rep.RelayStopped != nil || len(rep.Failures) != 0 {
		t.Fatalf("report = %+v", rep)
	}
	if last.done != last.tot {
		t.Fatalf("progress counted the failed attempt: %d of %d", last.done, last.tot)
	}
}

func TestRelayStopSendsTheRestThroughThePC(t *testing.T) {
	src, dst := relayFixture(t, 8)
	var calls int
	r := &fakeRelay{fail: func(k string) error {
		calls++
		if calls == 3 {
			return fmt.Errorf("%w: the daily allowance is used up", ErrRelayStopped)
		}
		return nil
	}}
	rep, err := Run(context.Background(), Options{Src: src, Dst: dst, Prefixes: []string{"machines/"}, Relay: r, Concurrency: 1})
	if err != nil {
		t.Fatal(err)
	}
	checkAllCopied(t, src, dst)
	if rep.Relayed != 2 || rep.RelayFellBack != 1 || !errors.Is(rep.RelayStopped, ErrRelayStopped) {
		t.Fatalf("report = %+v", rep)
	}
	if calls != 3 {
		t.Fatalf("relay asked %d times after it stopped", calls-3)
	}
}

func TestRelayThatKeepsFailingIsLeftAlone(t *testing.T) {
	src, dst := relayFixture(t, 12)
	var calls int
	r := &fakeRelay{fail: func(string) error { calls++; return errors.New("connection reset") }}
	rep, err := Run(context.Background(), Options{Src: src, Dst: dst, Prefixes: []string{"machines/"}, Relay: r, Concurrency: 1})
	if err != nil {
		t.Fatal(err)
	}
	checkAllCopied(t, src, dst)
	if calls != relayGiveUp || rep.RelayFellBack != relayGiveUp || rep.RelayStopped == nil {
		t.Fatalf("calls = %d, report = %+v", calls, rep)
	}
	if !strings.Contains(rep.RelayStopped.Error(), "connection reset") {
		t.Fatalf("reason = %v", rep.RelayStopped)
	}
}
