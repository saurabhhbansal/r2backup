package mirror

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/saurabhhbansal/r2backup/internal/remote"
)

// mem is a minimal in-memory remote.Store. Each write stamps the object with
// the store's clock, as a real store stamps its upload time.
type mem struct {
	mu      sync.Mutex
	objs    map[string]memObj
	clock   time.Time
	flushed int
	failGet map[string]bool
}

type memObj struct {
	data []byte
	md   remote.Metadata
	at   time.Time
}

func newMem(start time.Time) *mem {
	return &mem{objs: map[string]memObj{}, clock: start, failGet: map[string]bool{}}
}

func (m *mem) tick() time.Time { m.clock = m.clock.Add(time.Second); return m.clock }

func (m *mem) Put(_ context.Context, in remote.PutInput) error {
	b, err := io.ReadAll(in.Body)
	if err != nil {
		return err
	}
	if int64(len(b)) != in.Size {
		return fmt.Errorf("size %d != %d", len(b), in.Size)
	}
	if in.Progress != nil {
		in.Progress(in.Size)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.objs[in.Key] = memObj{data: b, md: in.Metadata, at: m.tick()}
	return nil
}
func (m *mem) Get(_ context.Context, key string) (*remote.Object, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failGet[key] {
		return nil, fmt.Errorf("boom")
	}
	o, ok := m.objs[key]
	if !ok {
		return nil, remote.ErrNotFound
	}
	return &remote.Object{Body: io.NopCloser(bytes.NewReader(o.data)), Metadata: o.md, Size: int64(len(o.data))}, nil
}
func (m *mem) Head(context.Context, string) (*remote.HeadResult, error) { panic("not used") }
func (m *mem) List(_ context.Context, prefix string) ([]remote.ListEntry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []remote.ListEntry
	for k, o := range m.objs {
		if strings.HasPrefix(k, prefix) {
			out = append(out, remote.ListEntry{Key: k, Size: int64(len(o.data)), ModTime: o.at})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}
func (m *mem) ListPrefixes(context.Context, string) ([]string, error) { panic("not used") }
func (m *mem) Copy(context.Context, string, string) error             { panic("not used") }
func (m *mem) Delete(ctx context.Context, key string) error           { return m.DeleteBatch(ctx, []string{key}) }
func (m *mem) DeleteBatch(_ context.Context, keys []string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, k := range keys {
		delete(m.objs, k)
	}
	return nil
}
func (m *mem) DeletePrefix(context.Context, string) (remote.PrefixDeletion, error) {
	panic("not used")
}
func (m *mem) AbandonStaleUploads(context.Context) (int, error) { return 0, nil }
func (m *mem) ListMultipartUploads(context.Context, string) ([]remote.MultipartUpload, error) {
	return nil, nil
}
func (m *mem) AbortUpload(context.Context, string, string) error { return nil }
func (m *mem) Bucket() string                                    { return "mem" }
func (m *mem) Flush(context.Context) error                       { m.flushed++; return nil }

func (m *mem) put(t *testing.T, key, data string) {
	t.Helper()
	md := remote.Metadata{ModTime: time.Unix(1_600_000_000, 0), Mode: 0o640}
	if err := m.Put(context.Background(), remote.PutInput{Key: key, Body: strings.NewReader(data), Size: int64(len(data)), Metadata: md}); err != nil {
		t.Fatal(err)
	}
}

func (m *mem) content(key string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return string(m.objs[key].data)
}

const scope = "machines/pc/Docs/"

func TestMirrorCopiesThenOnlyChanges(t *testing.T) {
	t0 := time.Unix(1_700_000_000, 0)
	src, dst := newMem(t0), newMem(t0.Add(time.Hour)) // destination clock runs later, as it writes later
	src.put(t, scope+"current/a.txt", "aaa")
	src.put(t, scope+"current/b/c.txt", "cc")
	src.put(t, "machines/pc/Other/current/x", "not mirrored")

	rep, err := Run(context.Background(), Options{Src: src, Dst: dst, Prefixes: []string{scope}, Delete: true})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Copied != 2 || rep.Bytes != 5 || len(rep.Failures) != 0 {
		t.Fatalf("first run: %+v", rep)
	}
	if dst.content(scope+"current/a.txt") != "aaa" || dst.objs[scope+"current/a.txt"].md.Mode != 0o640 {
		t.Fatal("content or metadata not carried")
	}
	if _, ok := dst.objs["machines/pc/Other/current/x"]; ok {
		t.Fatal("mirrored outside the prefix")
	}
	if dst.flushed == 0 {
		t.Fatal("destination not flushed")
	}

	// Nothing changed: nothing copied.
	rep, _ = Run(context.Background(), Options{Src: src, Dst: dst, Prefixes: []string{scope}, Delete: true})
	if rep.Copied != 0 || rep.Unchanged != 2 {
		t.Fatalf("second run: %+v", rep)
	}

	// Same size, rewritten later on the source: copied.
	src.clock = t0.Add(2 * time.Hour)
	src.put(t, scope+"current/a.txt", "AAA")
	// Deleted on the source: removed from the mirror.
	_ = src.Delete(context.Background(), scope+"current/b/c.txt")
	rep, _ = Run(context.Background(), Options{Src: src, Dst: dst, Prefixes: []string{scope}, Delete: true})
	if rep.Copied != 1 || rep.Deleted != 1 || dst.content(scope+"current/a.txt") != "AAA" {
		t.Fatalf("third run: %+v", rep)
	}
}

func TestEmptySourceNeverEmptiesMirror(t *testing.T) {
	src, dst := newMem(time.Unix(0, 0)), newMem(time.Unix(0, 0))
	dst.put(t, scope+"current/keep.txt", "precious")
	rep, err := Run(context.Background(), Options{Src: src, Dst: dst, Prefixes: []string{scope}, Delete: true})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Deleted != 0 || rep.SkippedDeletes != 1 || dst.content(scope+"current/keep.txt") != "precious" {
		t.Fatalf("mirror emptied itself: %+v", rep)
	}
}

// A backup deleted from where backups go only stays on the second copy, even
// when everything is mirrored at once; a file deleted inside a backup that is
// still there goes from both.
func TestWholeBackupGoneFromSourceIsKept(t *testing.T) {
	src, dst := newMem(time.Unix(0, 0)), newMem(time.Unix(0, 0))
	src.put(t, scope+"current/a.txt", "a")
	dst.put(t, scope+"current/a.txt", "a")
	dst.put(t, scope+"current/gone.txt", "g")
	dst.put(t, "machines/pc/Removed/current/keep.txt", "k")
	rep, err := Run(context.Background(), Options{Src: src, Dst: dst, Prefixes: []string{"machines/"}, Delete: true})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Deleted != 1 || rep.SkippedDeletes != 1 {
		t.Fatalf("report: %+v", rep)
	}
	if _, ok := dst.objs[scope+"current/gone.txt"]; ok {
		t.Fatal("a file deleted inside a kept backup stayed on the mirror")
	}
	if dst.content("machines/pc/Removed/current/keep.txt") != "k" {
		t.Fatal("a whole backup missing from the source was deleted from the mirror")
	}
}

func TestDryRunChangesNothing(t *testing.T) {
	src, dst := newMem(time.Unix(0, 0)), newMem(time.Unix(0, 0))
	src.put(t, scope+"current/a", "1")
	dst.put(t, scope+"current/gone", "2")
	rep, _ := Run(context.Background(), Options{Src: src, Dst: dst, Prefixes: []string{scope}, Delete: true, DryRun: true})
	if rep.Copied != 1 || rep.Deleted != 1 {
		t.Fatalf("plan: %+v", rep)
	}
	if _, ok := dst.objs[scope+"current/a"]; ok || dst.content(scope+"current/gone") != "2" {
		t.Fatal("dry run changed the destination")
	}
}

func TestOneFailureDoesNotStopTheRest(t *testing.T) {
	src, dst := newMem(time.Unix(0, 0)), newMem(time.Unix(0, 0))
	src.put(t, scope+"current/bad", "x")
	src.put(t, scope+"current/good", "y")
	src.failGet[scope+"current/bad"] = true
	rep, _ := Run(context.Background(), Options{Src: src, Dst: dst, Prefixes: []string{scope}})
	if rep.Copied != 1 || len(rep.Failures) != 1 || rep.Failures[0].Key != scope+"current/bad" {
		t.Fatalf("got %+v", rep)
	}
}
