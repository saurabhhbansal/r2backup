package bundle

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/saurabhhbansal/r2backup/internal/remote"
)

// memStore is an in-memory remote.Store that counts what it is asked to do.
type memStore struct {
	mu      sync.Mutex
	objects map[string]memObject
	puts    int
	gets    int
	ranges  int
	lists   int
	deletes int
	noRange bool
}

type memObject struct {
	data []byte
	md   remote.Metadata
	at   time.Time
}

func newMem() *memStore { return &memStore{objects: map[string]memObject{}} }

func (m *memStore) Put(ctx context.Context, in remote.PutInput) error {
	data, err := io.ReadAll(in.Body)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.puts++
	m.objects[in.Key] = memObject{data: data, md: in.Metadata, at: time.Now()}
	if in.Progress != nil {
		in.Progress(int64(len(data)))
	}
	return nil
}

func (m *memStore) Get(ctx context.Context, key string) (*remote.Object, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.gets++
	o, ok := m.objects[key]
	if !ok {
		return nil, fmt.Errorf("get %q: %w", key, remote.ErrNotFound)
	}
	return &remote.Object{Body: io.NopCloser(bytes.NewReader(o.data)), Metadata: o.md, Size: int64(len(o.data))}, nil
}

func (m *memStore) Head(ctx context.Context, key string) (*remote.HeadResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	o, ok := m.objects[key]
	if !ok {
		return nil, fmt.Errorf("head %q: %w", key, remote.ErrNotFound)
	}
	return &remote.HeadResult{Size: int64(len(o.data)), LastModified: o.at, Metadata: o.md}, nil
}

func (m *memStore) List(ctx context.Context, prefix string) ([]remote.ListEntry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lists++
	var out []remote.ListEntry
	for k, o := range m.objects {
		if strings.HasPrefix(k, prefix) {
			out = append(out, remote.ListEntry{Key: k, Size: int64(len(o.data)), ModTime: o.at})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

func (m *memStore) ListPrefixes(ctx context.Context, prefix string) ([]string, error) {
	list, _ := m.List(ctx, prefix)
	seen := map[string]bool{}
	var out []string
	for _, o := range list {
		rest := o.Key[len(prefix):]
		if i := strings.Index(rest, "/"); i > 0 && !seen[rest[:i]] {
			seen[rest[:i]] = true
			out = append(out, rest[:i])
		}
	}
	return out, nil
}

func (m *memStore) Copy(ctx context.Context, src, dst string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	o, ok := m.objects[src]
	if !ok {
		return remote.ErrNotFound
	}
	m.objects[dst] = o
	return nil
}

func (m *memStore) Delete(ctx context.Context, key string) error {
	return m.DeleteBatch(ctx, []string{key})
}

func (m *memStore) DeleteBatch(ctx context.Context, keys []string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.deletes++
	for _, k := range keys {
		delete(m.objects, k)
	}
	return nil
}

func (m *memStore) DeletePrefix(ctx context.Context, prefix string) (remote.PrefixDeletion, error) {
	list, _ := m.List(ctx, prefix)
	var keys []string
	for _, o := range list {
		keys = append(keys, o.Key)
	}
	return remote.PrefixDeletion{Objects: len(keys)}, m.DeleteBatch(ctx, keys)
}

func (m *memStore) AbandonStaleUploads(context.Context) (int, error) { return 0, nil }
func (m *memStore) ListMultipartUploads(context.Context, string) ([]remote.MultipartUpload, error) {
	return nil, nil
}
func (m *memStore) AbortUpload(context.Context, string, string) error { return nil }
func (m *memStore) Bucket() string                                   { return "mem" }

func (m *memStore) has(key string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.objects[key]
	return ok
}

func (m *memStore) count(prefix string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for k := range m.objects {
		if strings.HasPrefix(k, prefix) {
			n++
		}
	}
	return n
}

// rangeMem is a memStore that can read part of an object, as S3 can.
type rangeMem struct{ *memStore }

func (r rangeMem) GetRange(ctx context.Context, key string, off, n int64) (io.ReadCloser, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ranges++
	o, ok := r.objects[key]
	if !ok {
		return nil, remote.ErrNotFound
	}
	return io.NopCloser(bytes.NewReader(o.data[off : off+n])), nil
}

const ns = "machines/pc/Docs/"

func put(t *testing.T, s remote.Store, key, body string) {
	t.Helper()
	err := s.Put(context.Background(), remote.PutInput{
		Key: key, Body: strings.NewReader(body), Size: int64(len(body)),
		Metadata: remote.Metadata{ModTime: time.Unix(1700000000, 0), Mode: 0o640, Kind: remote.KindFile, Size: int64(len(body))},
	})
	if err != nil {
		t.Fatalf("put %s: %v", key, err)
	}
}

func get(t *testing.T, s remote.Store, key string) (string, error) {
	t.Helper()
	obj, err := s.Get(context.Background(), key)
	if err != nil {
		return "", err
	}
	defer obj.Body.Close()
	b, err := io.ReadAll(obj.Body)
	return string(b), err
}

func mustGet(t *testing.T, s remote.Store, key string) string {
	t.Helper()
	b, err := get(t, s, key)
	if err != nil {
		t.Fatalf("get %s: %v", key, err)
	}
	return b
}

func keys(t *testing.T, s remote.Store, prefix string) []string {
	t.Helper()
	list, err := s.List(context.Background(), prefix)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, o := range list {
		out = append(out, o.Key)
	}
	return out
}

func small(t *testing.T) Options {
	return Options{Pack: true, PackSize: 4096, SmallMax: 1024, TempDir: t.TempDir()}
}

func TestManySmallFilesAreAFewRequests(t *testing.T) {
	base := newMem()
	s := New(rangeMem{base}, small(t))
	ctx := context.Background()
	for i := 0; i < 200; i++ {
		put(t, s, fmt.Sprintf("%scurrent/f%03d.txt", ns, i), strings.Repeat(fmt.Sprintf("file %d ", i), 10))
	}
	if err := s.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if base.puts > 20 {
		t.Errorf("200 small files took %d writes", base.puts)
	}
	if n := base.count(ns + "current/"); n != 0 {
		t.Errorf("%d files went up on their own", n)
	}

	// A fresh Store -- the next run, or another computer -- reads them back.
	s2 := New(rangeMem{base}, small(t))
	if got := mustGet(t, s2, ns+"current/f123.txt"); got != strings.Repeat("file 123 ", 10) {
		t.Fatalf("got %q", got)
	}
	if got := len(keys(t, s2, ns+"current/")); got != 200 {
		t.Fatalf("listed %d, want 200", got)
	}
	head, err := s2.Head(ctx, ns+"current/f001.txt")
	if err != nil {
		t.Fatal(err)
	}
	if head.Metadata.Mode != 0o640 || !head.Metadata.ModTime.Equal(time.Unix(1700000000, 0)) {
		t.Errorf("metadata = %+v", head.Metadata)
	}
	if base.ranges == 0 {
		t.Error("a store with range reads should be read by range")
	}
}

func TestPacksAreZipFiles(t *testing.T) {
	base := newMem()
	s := New(base, small(t))
	put(t, s, ns+"current/a.txt", strings.Repeat("hello ", 100))
	put(t, s, ns+"current/dir/b.bin", "\x00\x01\x02")
	if err := s.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	var pack []byte
	for k, o := range base.objects {
		if strings.HasPrefix(k, ns+packsDir) {
			pack = o.data
		}
	}
	zr, err := zip.NewReader(bytes.NewReader(pack), int64(len(pack)))
	if err != nil {
		t.Fatalf("the pack is not a zip: %v", err)
	}
	got := map[string]string{}
	for _, f := range zr.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(rc)
		rc.Close()
		got[f.Name] = string(b)
	}
	if got["current/a.txt"] != strings.Repeat("hello ", 100) || got["current/dir/b.bin"] != "\x00\x01\x02" {
		t.Fatalf("zip holds %v", got)
	}
}

func TestBigFilesGoUpOnTheirOwn(t *testing.T) {
	base := newMem()
	s := New(base, small(t))
	big := strings.Repeat("x", 5000)
	put(t, s, ns+"current/big.iso", big)
	if !base.has(ns + "current/big.iso") {
		t.Fatal("a file over SmallMax should be an object of its own")
	}
	if err := s.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	s2 := New(base, small(t))
	if mustGet(t, s2, ns+"current/big.iso") != big {
		t.Fatal("big file did not read back")
	}
	if got := keys(t, s2, ns); len(got) != 1 {
		t.Fatalf("listing = %v", got)
	}
}

func TestNothingIsOnRecordUntilFlush(t *testing.T) {
	base := newMem()
	s := New(base, small(t))
	defer s.Close()
	put(t, s, ns+"current/a.txt", "a")
	// Readable in this run...
	if mustGet(t, s, ns+"current/a.txt") != "a" {
		t.Fatal("unflushed file not readable in the same run")
	}
	// ...but a run that dies here has put nothing on record.
	s2 := New(base, small(t))
	if _, err := get(t, s2, ns+"current/a.txt"); err == nil {
		t.Fatal("a file from an unflushed run is on record")
	}
}

func TestTrashMovesAndDeletesAreIndexEdits(t *testing.T) {
	base := newMem()
	s := New(base, small(t))
	ctx := context.Background()
	put(t, s, ns+"current/a.txt", "alpha")
	put(t, s, ns+"current/b.txt", "bravo")
	if err := s.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	puts := base.puts

	if err := s.Move(ctx, ns+"current/a.txt", ns+"trash/2026-10-31/a.txt"); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(ctx, ns+"current/b.txt"); err != nil {
		t.Fatal(err)
	}
	if err := s.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if base.puts-puts != 1 {
		t.Errorf("a move and a delete took %d writes, want just the index", base.puts-puts)
	}
	s2 := New(base, small(t))
	if mustGet(t, s2, ns+"trash/2026-10-31/a.txt") != "alpha" {
		t.Fatal("trashed file lost")
	}
	if _, err := get(t, s2, ns+"current/a.txt"); !errors.Is(err, remote.ErrNotFound) {
		t.Fatalf("moved file still there: %v", err)
	}
	if got := keys(t, s2, ns); len(got) != 1 {
		t.Fatalf("listing = %v", got)
	}
	names, _ := s2.ListPrefixes(ctx, ns)
	if strings.Join(names, ",") != "trash" {
		t.Fatalf("prefixes = %v (bundling's own folder must not show)", names)
	}
}

func TestAChangedFileSendsOnlyItself(t *testing.T) {
	base := newMem()
	opts := small(t)
	opts.PackSize = 1 << 20
	s := New(base, opts)
	ctx := context.Background()
	for i := 0; i < 50; i++ {
		put(t, s, fmt.Sprintf("%scurrent/f%02d", ns, i), strings.Repeat("v1 ", 50))
	}
	if err := s.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	before := base.puts
	s2 := New(base, opts)
	put(t, s2, ns+"current/f07", "v2")
	if err := s2.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if base.puts-before != 2 {
		t.Errorf("one changed file took %d writes, want a small pack and the index", base.puts-before)
	}
	s3 := New(base, opts)
	if mustGet(t, s3, ns+"current/f07") != "v2" || mustGet(t, s3, ns+"current/f08") != strings.Repeat("v1 ", 50) {
		t.Fatal("wrong content after a change")
	}
}

func TestEmptyPacksGoAndSparseOnesAreCompacted(t *testing.T) {
	base := newMem()
	s := New(base, small(t))
	ctx := context.Background()
	for i := 0; i < 12; i++ {
		put(t, s, fmt.Sprintf("%scurrent/f%02d", ns, i), noise(i, 900))
	}
	if err := s.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	packs := base.count(ns + packsDir)
	if packs < 3 {
		t.Fatalf("expected several packs, got %d", packs)
	}
	// Delete all but one file: most packs empty, one sparse.
	var doomed []string
	for i := 1; i < 12; i++ {
		doomed = append(doomed, fmt.Sprintf("%scurrent/f%02d", ns, i))
	}
	if err := s.DeleteBatch(ctx, doomed); err != nil {
		t.Fatal(err)
	}
	if err := s.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if got := base.count(ns + packsDir); got != 1 {
		t.Fatalf("%d packs left, want 1", got)
	}
	s2 := New(base, small(t))
	if mustGet(t, s2, ns+"current/f00") != noise(0, 900) {
		t.Fatal("the surviving file is damaged")
	}
}

// noise is incompressible content.
func noise(seed, n int) string {
	b := make([]byte, n)
	x := uint32(seed*2654435761 + 1)
	for i := range b {
		x ^= x << 13
		x ^= x >> 17
		x ^= x << 5
		b[i] = byte(x)
	}
	return string(b)
}

func TestCompactionKeepsEveryLiveFile(t *testing.T) {
	base := newMem()
	s := New(base, small(t))
	ctx := context.Background()
	for i := 0; i < 16; i++ {
		put(t, s, fmt.Sprintf("%scurrent/f%02d", ns, i), noise(i, 900))
	}
	if err := s.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	// Keep every fourth file, and a trash copy of one, so packs are sparse
	// but not empty.
	if err := s.Copy(ctx, ns+"current/f04", ns+"trash/d/f04"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 16; i++ {
		if i%4 != 0 {
			if err := s.Delete(ctx, fmt.Sprintf("%scurrent/f%02d", ns, i)); err != nil {
				t.Fatal(err)
			}
		}
	}
	before := base.count(ns + packsDir)
	if err := s.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if after := base.count(ns + packsDir); after >= before {
		t.Errorf("packs %d -> %d: sparse packs were not compacted", before, after)
	}
	s2 := New(base, small(t))
	for i := 0; i < 16; i += 4 {
		if got := mustGet(t, s2, fmt.Sprintf("%scurrent/f%02d", ns, i)); got != noise(i, 900) {
			t.Fatalf("f%02d damaged after compaction", i)
		}
	}
	if mustGet(t, s2, ns+"trash/d/f04") != noise(4, 900) {
		t.Fatal("trash copy damaged after compaction")
	}
}

func TestStartingOverDirectBackupsAndStoppingAgain(t *testing.T) {
	base := newMem()
	ctx := context.Background()
	// Backed up directly, before bundling was turned on.
	put(t, base, ns+"current/old.txt", "old")

	// Not bundling and never bundled: no index, every call goes straight
	// through, and nothing extra is written.
	direct := New(base, Options{TempDir: t.TempDir()})
	put(t, direct, ns+"current/also-direct.txt", "d")
	if err := direct.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if base.has(ns + indexName) {
		t.Fatal("a never-bundled backup got an index")
	}

	// Bundling on: the old objects are adopted, new ones packed, and an
	// overwrite of a direct object removes the stale copy.
	on := New(base, small(t))
	put(t, on, ns+"current/new.txt", "new")
	put(t, on, ns+"current/old.txt", "old v2")
	if err := on.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if base.has(ns + "current/old.txt") {
		t.Error("the replaced direct object was left behind")
	}
	if !base.has(ns + "current/also-direct.txt") {
		t.Error("an untouched direct object was removed")
	}

	// Bundling off again: packed files still read, new writes go direct.
	off := New(base, Options{TempDir: t.TempDir()})
	if mustGet(t, off, ns+"current/new.txt") != "new" || mustGet(t, off, ns+"current/old.txt") != "old v2" {
		t.Fatal("packed files unreadable once bundling is off")
	}
	put(t, off, ns+"current/new.txt", "new v2")
	if err := off.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if !base.has(ns + "current/new.txt") {
		t.Fatal("a write with bundling off did not go up directly")
	}
	again := New(base, Options{TempDir: t.TempDir()})
	if mustGet(t, again, ns+"current/new.txt") != "new v2" || mustGet(t, again, ns+"current/also-direct.txt") != "d" {
		t.Fatal("wrong content after turning bundling off")
	}
	if got := keys(t, again, ns); len(got) != 3 {
		t.Fatalf("listing = %v", got)
	}
}

func TestWiderListingsReadEachBundledBackupsIndex(t *testing.T) {
	base := newMem()
	ctx := context.Background()
	s := New(base, small(t))
	put(t, s, "machines/pc/Docs/current/a", "a")
	put(t, s, "machines/pc/Photos/current/b", "b")
	put(t, base, "machines/laptop/Music/current/c", "c") // someone else's, direct
	if err := s.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	s2 := New(base, small(t))
	got := keys(t, s2, "machines/")
	want := []string{"machines/laptop/Music/current/c", "machines/pc/Docs/current/a", "machines/pc/Photos/current/b"}
	sort.Strings(got)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("listing = %v", got)
	}
	machines, _ := s2.ListPrefixes(ctx, "machines/")
	if strings.Join(machines, ",") != "laptop,pc" {
		t.Fatalf("machines = %v", machines)
	}
}

func TestDeletingAWholeBackupTakesItsPacks(t *testing.T) {
	base := newMem()
	ctx := context.Background()
	s := New(base, small(t))
	put(t, s, ns+"current/a", "a")
	put(t, s, ns+"current/b", "b")
	if err := s.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	res, err := s.DeletePrefix(ctx, ns)
	if err != nil {
		t.Fatal(err)
	}
	if res.Objects != 2 {
		t.Errorf("reported %d deleted, want the 2 files", res.Objects)
	}
	if n := base.count(ns); n != 0 {
		t.Fatalf("%d objects left under the backup", n)
	}
}

func TestOrphanPacksAreSweptOnceOld(t *testing.T) {
	base := newMem()
	ctx := context.Background()
	now := time.Now()
	opts := small(t)
	opts.Now = func() time.Time { return now }
	// A run that sent a pack and died before saving the index.
	dead := New(base, opts)
	defer dead.Close()
	for i := 0; i < 10; i++ {
		put(t, dead, fmt.Sprintf("%scurrent/f%d", ns, i), noise(i, 900))
	}
	orphans := base.count(ns + packsDir)
	if orphans == 0 {
		t.Fatal("setup: no pack was sent")
	}
	// Age the orphans past the grace period.
	base.mu.Lock()
	for k, o := range base.objects {
		o.at = now.Add(-2 * orphanAge)
		base.objects[k] = o
	}
	base.mu.Unlock()

	s := New(base, opts)
	put(t, s, ns+"current/kept", "k")
	if err := s.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if got := base.count(ns + packsDir); got != 1 {
		t.Fatalf("%d packs, want only the live one", got)
	}
	if mustGet(t, New(base, opts), ns+"current/kept") != "k" {
		t.Fatal("live file lost to the sweep")
	}
}

func TestStoresWithoutRangeReadsFetchEachPackOnce(t *testing.T) {
	base := newMem()
	s := New(base, Options{Pack: true, TempDir: t.TempDir()})
	ctx := context.Background()
	for i := 0; i < 30; i++ {
		put(t, s, fmt.Sprintf("%scurrent/f%02d", ns, i), fmt.Sprintf("content %d", i))
	}
	if err := s.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	r := New(base, Options{TempDir: t.TempDir()})
	defer r.Close()
	gets := base.gets
	for i := 0; i < 30; i++ {
		if got := mustGet(t, r, fmt.Sprintf("%scurrent/f%02d", ns, i)); got != fmt.Sprintf("content %d", i) {
			t.Fatalf("f%02d = %q", i, got)
		}
	}
	if base.gets-gets > 2 {
		t.Errorf("30 files from one pack took %d downloads, want the index and the pack", base.gets-gets)
	}
}

func TestDamageIsCaught(t *testing.T) {
	base := newMem()
	s := New(rangeMem{base}, small(t))
	ctx := context.Background()
	put(t, s, ns+"current/a", noise(1, 500))
	if err := s.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	base.mu.Lock()
	for k, o := range base.objects {
		if strings.HasPrefix(k, ns+packsDir) {
			o.data[40] ^= 0xff // inside the file's bytes, past the local header
			base.objects[k] = o
		}
	}
	base.mu.Unlock()
	if _, err := get(t, New(rangeMem{base}, small(t)), ns+"current/a"); err == nil {
		t.Fatal("a damaged file read back without an error")
	}
}

func TestKeysOutsideABackupAreNeverBundled(t *testing.T) {
	base := newMem()
	s := New(base, small(t))
	put(t, s, "r2backup/probe", "x")
	put(t, s, "drime-mirror/laptop/a.txt", "y")
	if !base.has("r2backup/probe") || !base.has("drime-mirror/laptop/a.txt") {
		t.Fatal("a key outside machines/<m>/<set>/ was bundled")
	}
}
