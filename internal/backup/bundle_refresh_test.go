package backup_test

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/saurabhhbansal/r2backup/internal/backup"
	"github.com/saurabhhbansal/r2backup/internal/bundle"
	"github.com/saurabhhbansal/r2backup/internal/index"
	"github.com/saurabhhbansal/r2backup/internal/remote"
	"github.com/saurabhhbansal/r2backup/internal/sets"
)

// memStore is a small in-memory remote.Store, for tests that need no S3
// semantics beyond keys and bytes.
type memStore struct {
	mu   sync.Mutex
	objs map[string][]byte
}

func newMemStore() *memStore { return &memStore{objs: map[string][]byte{}} }

func (m *memStore) Put(_ context.Context, in remote.PutInput) error {
	b, err := io.ReadAll(in.Body)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.objs[in.Key] = b
	return nil
}
func (m *memStore) Get(_ context.Context, key string) (*remote.Object, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.objs[key]
	if !ok {
		return nil, remote.ErrNotFound
	}
	return &remote.Object{Body: io.NopCloser(bytes.NewReader(b)), Size: int64(len(b))}, nil
}
func (m *memStore) Head(_ context.Context, key string) (*remote.HeadResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.objs[key]
	if !ok {
		return nil, remote.ErrNotFound
	}
	return &remote.HeadResult{Size: int64(len(b)), LastModified: time.Now()}, nil
}
func (m *memStore) List(_ context.Context, prefix string) ([]remote.ListEntry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []remote.ListEntry
	for k, b := range m.objs {
		if strings.HasPrefix(k, prefix) {
			out = append(out, remote.ListEntry{Key: k, Size: int64(len(b)), ModTime: time.Now()})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}
func (m *memStore) ListPrefixes(_ context.Context, prefix string) ([]string, error) {
	return nil, nil
}
func (m *memStore) Copy(_ context.Context, src, dst string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.objs[src]
	if !ok {
		return remote.ErrNotFound
	}
	m.objs[dst] = append([]byte(nil), b...)
	return nil
}
func (m *memStore) Delete(ctx context.Context, key string) error {
	return m.DeleteBatch(ctx, []string{key})
}
func (m *memStore) DeleteBatch(_ context.Context, keys []string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, k := range keys {
		delete(m.objs, k)
	}
	return nil
}
func (m *memStore) DeletePrefix(_ context.Context, prefix string) (remote.PrefixDeletion, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var d remote.PrefixDeletion
	for k := range m.objs {
		if strings.HasPrefix(k, prefix) {
			d.Objects++
			delete(m.objs, k)
		}
	}
	return d, nil
}
func (m *memStore) AbandonStaleUploads(context.Context) (int, error) { return 0, nil }
func (m *memStore) ListMultipartUploads(context.Context, string) ([]remote.MultipartUpload, error) {
	return nil, nil
}
func (m *memStore) AbortUpload(context.Context, string, string) error { return nil }
func (m *memStore) Bucket() string                                    { return "mem" }

// The tray keeps one bundling store for its whole life; scheduled backups
// run in processes of their own. A tray backup after a scheduled one must
// start from the backup's index as it is now, not as the tray last read it,
// or saving its own index drops what the scheduled run sent.
func TestATrayRunAfterAScheduledOneKeepsItsFiles(t *testing.T) {
	ctx := context.Background()
	base := newMemStore()
	root := t.TempDir()
	db, err := index.Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	set := sets.Set{Name: "Docs", Prefix: "machines/pc/Docs", Root: root, Machine: "pc"}
	write := func(name string) {
		if err := os.WriteFile(filepath.Join(root, name), []byte("contents of "+name), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	run := func(s remote.Store) {
		t.Helper()
		if _, err := backup.Run(ctx, backup.Options{Set: set, Index: db, Client: s}); err != nil {
			t.Fatal(err)
		}
	}
	opts := bundle.Options{Pack: true, TempDir: t.TempDir()}
	tray := bundle.New(base, opts)
	defer tray.Close()

	write("a.txt")
	run(tray)
	// The panel shows the backup, reading its index.
	if _, err := tray.List(ctx, set.Prefix+"/"); err != nil {
		t.Fatal(err)
	}

	write("b.txt")
	sched := bundle.New(base, opts)
	run(sched)
	sched.Close()

	write("c.txt")
	run(tray)

	fresh := bundle.New(base, opts)
	defer fresh.Close()
	list, err := fresh.List(ctx, set.Prefix+"/current/")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, o := range list {
		got[filepath.Base(o.Key)] = true
	}
	for _, n := range []string{"a.txt", "b.txt", "c.txt"} {
		if !got[n] {
			t.Errorf("%s is no longer in the backup; it has %v", n, got)
		}
	}
}
