package drime

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/saurabhhbansal/r2backup/internal/remote"
)

type memResume struct {
	mu sync.Mutex
	m  map[string]remote.Upload
}

func (s *memResume) Resumable(key string) (remote.Upload, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.m[key]
	return u, ok, nil
}
func (s *memResume) SaveResumable(u remote.Upload) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[u.Key] = u
	return nil
}
func (s *memResume) ForgetResumable(key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m, key)
	return nil
}
func (s *memResume) AllResumable() ([]remote.Upload, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []remote.Upload
	for _, u := range s.m {
		out = append(out, u)
	}
	return out, nil
}

func newClient(t *testing.T, f *fakeDrime, resume remote.ResumeStore) *Client {
	t.Helper()
	c, err := New(context.Background(), Config{
		Token: f.token, BaseURL: f.srv.URL, Resume: resume,
		Threshold: 6 << 20, PartSize: 5 << 20, MaxAttempts: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func put(t *testing.T, c *Client, key string, data []byte, md remote.Metadata) {
	t.Helper()
	if err := c.Put(context.Background(), remote.PutInput{Key: key, Body: bytes.NewReader(data), Size: int64(len(data)), Metadata: md}); err != nil {
		t.Fatalf("put %s: %v", key, err)
	}
}

func get(t *testing.T, c *Client, key string) ([]byte, remote.Metadata) {
	t.Helper()
	obj, err := c.Get(context.Background(), key)
	if err != nil {
		t.Fatalf("get %s: %v", key, err)
	}
	defer obj.Body.Close()
	b, err := io.ReadAll(obj.Body)
	if err != nil {
		t.Fatal(err)
	}
	return b, obj.Metadata
}

const set = "machines/pc/Docs"

func TestBadTokenSaysSo(t *testing.T) {
	f := newFake(t)
	_, err := New(context.Background(), Config{Token: "wrong", BaseURL: f.srv.URL, MaxAttempts: 1})
	if err == nil || !strings.Contains(err.Error(), "token was refused") {
		t.Fatalf("got %v", err)
	}
}

func TestPutGetRoundTripWithMetadata(t *testing.T) {
	f := newFake(t)
	c := newClient(t, f, nil)
	mt := time.Date(2020, 5, 6, 7, 8, 9, 123456789, time.UTC)
	put(t, c, set+"/current/a/b.txt", []byte("hello"), remote.Metadata{ModTime: mt, Mode: 0o600, Kind: remote.KindFile})
	put(t, c, set+"/current/link", nil, remote.Metadata{ModTime: mt, Kind: remote.KindSymlink, Symlink: "a/b.txt"})

	if _, ok := f.files()["r2backup/machines/pc/Docs/current/a/b.txt"]; !ok {
		t.Fatalf("file not at the browsable path; have %v", keys(f.files()))
	}

	if err := c.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.files()["r2backup/"+set+"/"+ManifestName]; !ok {
		t.Fatal("manifest not written")
	}

	// A fresh client -- another computer -- reads the dates back.
	c2 := newClient(t, f, nil)
	data, md := get(t, c2, set+"/current/a/b.txt")
	if string(data) != "hello" || !md.ModTime.Equal(mt) || md.Mode != 0o600 || md.Size != 5 {
		t.Fatalf("got %q %+v", data, md)
	}
	_, lmd := get(t, c2, set+"/current/link")
	if lmd.Kind != remote.KindSymlink || lmd.Symlink != "a/b.txt" {
		t.Fatalf("symlink metadata lost: %+v", lmd)
	}
}

func TestOverwriteLeavesOneCopy(t *testing.T) {
	f := newFake(t)
	c := newClient(t, f, nil)
	k := set + "/current/x.txt"
	put(t, c, k, []byte("one"), remote.Metadata{})
	put(t, c, k, []byte("two"), remote.Metadata{})
	n := 0
	for p := range f.files() {
		if strings.HasSuffix(p, "/x.txt") || strings.Contains(p, "x.txt (") {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("want one copy, have %v", keys(f.files()))
	}
	if b, _ := get(t, c, k); string(b) != "two" {
		t.Fatalf("got %q", b)
	}
	// And from a client that has to list it fresh.
	if b, _ := get(t, newClient(t, f, nil), k); string(b) != "two" {
		t.Fatalf("fresh client got %q", b)
	}
}

func TestListAndPrefixes(t *testing.T) {
	f := newFake(t)
	f.perPage = 2 // make every listing page
	c := newClient(t, f, nil)
	for _, k := range []string{"current/a.txt", "current/d/b.txt", "current/d/e/c.txt", "trash/2024-01-01/z.txt"} {
		put(t, c, set+"/"+k, []byte(k), remote.Metadata{})
	}
	put(t, c, "machines/pc/Docs2/current/other.txt", []byte("x"), remote.Metadata{})
	if err := c.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}

	c2 := newClient(t, f, nil)
	got, err := c2.List(context.Background(), set+"/")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range got {
		names = append(names, strings.TrimPrefix(e.Key, set+"/"))
	}
	want := "current/a.txt current/d/b.txt current/d/e/c.txt trash/2024-01-01/z.txt"
	if strings.Join(names, " ") != want {
		t.Fatalf("list = %v (manifest must be hidden, Docs2 excluded)", names)
	}

	machines, err := c2.ListPrefixes(context.Background(), "machines/")
	if err != nil || strings.Join(machines, ",") != "pc" {
		t.Fatalf("machines = %v %v", machines, err)
	}
	sets, _ := c2.ListPrefixes(context.Background(), "machines/pc/")
	if strings.Join(sets, ",") != "Docs,Docs2" {
		t.Fatalf("sets = %v", sets)
	}
	if none, err := c2.List(context.Background(), "machines/nobody/"); err != nil || len(none) != 0 {
		t.Fatalf("missing prefix: %v %v", none, err)
	}
}

func TestCopyAndDelete(t *testing.T) {
	f := newFake(t)
	c := newClient(t, f, nil)
	mt := time.Unix(1_600_000_000, 0)
	src, dst := set+"/current/f.txt", set+"/trash/2024-01-01/f~000000-ab.txt"
	put(t, c, src, []byte("data"), remote.Metadata{ModTime: mt, Mode: 0o640})
	if err := c.Copy(context.Background(), src, dst); err != nil {
		t.Fatal(err)
	}
	// Copy within one folder: Drime names the duplicate "x (1)"; it must be
	// renamed back.
	if err := c.Copy(context.Background(), src, set+"/current/g.txt"); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.files()["r2backup/"+set+"/current/g.txt"]; !ok {
		t.Fatalf("copy kept Drime's name: %v", keys(f.files()))
	}
	if err := c.Delete(context.Background(), src); err != nil {
		t.Fatal(err)
	}
	if err := c.Delete(context.Background(), src); err != nil {
		t.Fatalf("deleting a missing key must not fail: %v", err)
	}
	if _, err := c.Head(context.Background(), src); !errors.Is(err, remote.ErrNotFound) {
		t.Fatalf("head after delete: %v", err)
	}
	if err := c.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	b, md := get(t, newClient(t, f, nil), dst)
	if string(b) != "data" || !md.ModTime.Equal(mt) || md.Mode != 0o640 {
		t.Fatalf("copied object: %q %+v", b, md)
	}
}

func TestMoveIsAMoveNotACopy(t *testing.T) {
	f := newFake(t)
	c := newClient(t, f, nil)
	mt := time.Unix(1_650_000_000, 0)
	src, dst := set+"/current/d/f.txt", set+"/trash/2024-01-01/d/f~000000-ab.txt"
	put(t, c, src, []byte("data"), remote.Metadata{ModTime: mt, Mode: 0o600})
	dups := f.callCount("POST /api/v1/file-entries/duplicate")
	if err := c.Move(context.Background(), src, dst); err != nil {
		t.Fatal(err)
	}
	if f.callCount("POST /api/v1/file-entries/duplicate") != dups {
		t.Fatal("Move duplicated the content")
	}
	if _, err := c.Head(context.Background(), src); !errors.Is(err, remote.ErrNotFound) {
		t.Fatalf("source still there: %v", err)
	}
	// Deleting the moved-away live key, as the backup does next, succeeds.
	if err := c.Delete(context.Background(), src); err != nil {
		t.Fatal(err)
	}
	if err := c.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	b, md := get(t, newClient(t, f, nil), dst)
	if string(b) != "data" || !md.ModTime.Equal(mt) || md.Mode != 0o600 {
		t.Fatalf("moved object: %q %+v", b, md)
	}
}

func TestDeletePrefixRemovesFolder(t *testing.T) {
	f := newFake(t)
	c := newClient(t, f, nil)
	put(t, c, set+"/current/a.txt", []byte("12345"), remote.Metadata{})
	put(t, c, set+"/current/b/c.txt", []byte("12"), remote.Metadata{})
	put(t, c, "machines/pc/Docs2/current/keep.txt", []byte("k"), remote.Metadata{})
	if err := c.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	before := f.callCount("GET /api/v1/drive/file-entries")
	res, err := c.DeletePrefix(context.Background(), set+"/")
	if err != nil {
		t.Fatal(err)
	}
	// A whole folder goes in one request, uncounted: no listing first.
	if res.Objects != -1 {
		t.Fatalf("deletion = %+v", res)
	}
	if n := f.callCount("GET /api/v1/drive/file-entries") - before; n > 1 {
		t.Fatalf("deleting a folder listed %d times; it should only resolve the path", n)
	}
	for p := range f.files() {
		if strings.Contains(p, "/Docs/") {
			t.Fatalf("left behind: %s", p)
		}
	}
	if _, ok := f.files()["r2backup/machines/pc/Docs2/current/keep.txt"]; !ok {
		t.Fatal("deleted the neighbouring set")
	}
	// Flushing after must not resurrect the deleted set's manifest.
	if err := c.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.files()["r2backup/"+set+"/"+ManifestName]; ok {
		t.Fatal("manifest came back")
	}
}

// Drime answers a big folder delete only once it is done, which can take
// minutes. Past folderDeleteWait the client stops waiting for the answer and
// looks for the folder instead: gone means deleted.
func TestDeletePrefixDoesNotWaitForeverOnASlowAnswer(t *testing.T) {
	slowDeleteWaits(t)
	f := newFake(t)
	c := newClient(t, f, nil)
	put(t, c, set+"/current/a.txt", []byte("12345"), remote.Metadata{})
	if err := c.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.slowDelete = time.Minute
	start := time.Now()
	res, err := c.DeletePrefix(context.Background(), set+"/")
	if err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Fatalf("waited %v for an answer it no longer needed", d)
	}
	if res.Objects != -1 {
		t.Fatalf("deletion = %+v", res)
	}
	for p := range f.files() {
		if strings.Contains(p, "/Docs/") {
			t.Fatalf("left behind: %s", p)
		}
	}
}

// A delete that never happens is an error, after a bounded wait, not a hang
// and not a false success.
func TestDeletePrefixReportsAFolderThatStays(t *testing.T) {
	slowDeleteWaits(t)
	f := newFake(t)
	c := newClient(t, f, nil)
	put(t, c, set+"/current/a.txt", []byte("12345"), remote.Metadata{})
	if err := c.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.slowDelete, f.stuckDelete, f.noTrash = time.Minute, true, true
	start := time.Now()
	if _, err := c.DeletePrefix(context.Background(), set+"/"); err == nil {
		t.Fatal("reported a folder that is still there as deleted")
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Fatalf("took %v to give up", d)
	}
}

func slowDeleteWaits(t *testing.T) {
	t.Helper()
	w, g, e := folderDeleteWait, folderGoneWait, folderGoneEvery
	folderDeleteWait, folderGoneWait, folderGoneEvery = 200*time.Millisecond, 600*time.Millisecond, 50*time.Millisecond
	t.Cleanup(func() { folderDeleteWait, folderGoneWait, folderGoneEvery = w, g, e })
}

func TestLargeUploadResumes(t *testing.T) {
	f := newFake(t)
	resume := &memResume{m: map[string]remote.Upload{}}
	c := newClient(t, f, resume)

	data := bytes.Repeat([]byte("0123456789abcdef"), (17<<20)/16) // 17 MiB: 4 parts of 5 MiB
	path := filepath.Join(t.TempDir(), "big.bin")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	key := set + "/current/big.bin"
	md := remote.Metadata{ModTime: time.Unix(1_700_000_000, 0)}

	f.failParts[4] = 1 // first attempt dies on the last part
	file, _ := os.Open(path)
	// Parts go up side by side, so progress arrives from several goroutines.
	var sent atomic.Int64
	err := c.Put(context.Background(), remote.PutInput{Key: key, Body: file, Size: int64(len(data)), Metadata: md,
		Progress: func(n int64) { sent.Add(n) }})
	file.Close()
	if err == nil {
		t.Fatal("expected the first attempt to fail")
	}
	rec, ok, _ := resume.Resumable(key)
	if !ok || len(rec.Parts) != 3 {
		t.Fatalf("resume record = %+v %v", rec, ok)
	}

	before := f.calls["PUT /part/"+strings.Split(rec.UploadID, "\n")[0]+"/1"]
	file, _ = os.Open(path)
	sent.Store(0)
	err = c.Put(context.Background(), remote.PutInput{Key: key, Body: file, Size: int64(len(data)), Metadata: md,
		Progress: func(n int64) { sent.Add(n) }})
	file.Close()
	if err != nil {
		t.Fatal(err)
	}
	if after := f.calls["PUT /part/"+strings.Split(rec.UploadID, "\n")[0]+"/1"]; after != before {
		t.Fatal("part 1 was sent again on resume")
	}
	if sent.Load() != int64(len(data)) {
		t.Fatalf("progress reported %d of %d bytes", sent.Load(), len(data))
	}
	if _, ok, _ := resume.Resumable(key); ok {
		t.Fatal("resume record not forgotten after success")
	}
	got, _ := get(t, c, key)
	if !bytes.Equal(got, data) {
		t.Fatal("content differs after resumed upload")
	}
}

func TestNameEncodingRoundTrips(t *testing.T) {
	for _, name := range []string{"plain.txt", " lead", "trail ", `back\slash`, ".", "..", "tab\there", "already＼sub", "‛quote", "␠x"} {
		enc := encodeName(name)
		if strings.ContainsAny(enc, "\\\t") || strings.HasPrefix(enc, " ") || strings.HasSuffix(enc, " ") {
			t.Errorf("%q encoded to unsafe %q", name, enc)
		}
		if dec := decodeName(enc); dec != name {
			t.Errorf("%q -> %q -> %q", name, enc, dec)
		}
	}
}

func TestOddNamesSurviveRoundTrip(t *testing.T) {
	f := newFake(t)
	c := newClient(t, f, nil)
	k := set + "/current/ sp ace /we\\ird.txt"
	put(t, c, k, []byte("x"), remote.Metadata{})
	got, err := newClient(t, f, nil).List(context.Background(), set+"/current/")
	if err != nil || len(got) != 1 || got[0].Key != k {
		t.Fatalf("list = %+v %v", got, err)
	}
}

func keys(m map[string][]byte) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

// Uploads into many new folders at once must create each folder once --
// Drime would happily hold two folders of the same name -- and lose nothing,
// now that listings run outside the tree's lock.
func TestConcurrentPutsCreateEachFolderOnce(t *testing.T) {
	f := newFake(t)
	c := newClient(t, f, nil)
	var wg sync.WaitGroup
	errs := make(chan error, 60)
	for i := 0; i < 60; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			key := fmt.Sprintf("%s/current/d%d/sub/f%d.txt", set, i%10, i)
			errs <- c.Put(context.Background(), remote.PutInput{Key: key, Body: strings.NewReader("x"), Size: 1})
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	f.mu.Lock()
	seen := map[string]int{}
	for _, e := range f.entries {
		if e.Type == itemTypeFolder {
			seen[f.path(e)]++
		}
	}
	f.mu.Unlock()
	for p, n := range seen {
		if n > 1 {
			t.Errorf("folder %s created %d times", p, n)
		}
	}
	got, err := newClient(t, f, nil).List(context.Background(), set+"/current/")
	if err != nil || len(got) != 60 {
		t.Fatalf("listed %d files, err %v", len(got), err)
	}
}

// A folder being made must not hold up uploads into folders that are already
// there. On a first backup every folder is new, and when creation held the
// tree's lock each one stopped every upload in the run for a round trip.
func TestMakingAFolderDoesNotHoldUpOtherUploads(t *testing.T) {
	f := newFake(t)
	c := newClient(t, f, nil)
	put(t, c, set+"/current/a/one.txt", []byte("1"), remote.Metadata{})

	gate := make(chan struct{})
	f.folderGate = gate
	slow := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func(i int) {
			key := fmt.Sprintf("%s/current/b/f%d.txt", set, i)
			slow <- c.Put(context.Background(), remote.PutInput{Key: key, Body: strings.NewReader("x"), Size: 1})
		}(i)
	}
	time.Sleep(50 * time.Millisecond) // let both reach the folder creation

	quick := make(chan error, 1)
	go func() {
		quick <- c.Put(context.Background(), remote.PutInput{Key: set + "/current/a/two.txt", Body: strings.NewReader("2"), Size: 1})
	}()
	select {
	case err := <-quick:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		close(gate)
		t.Fatal("an upload into an existing folder waited for another folder to be made")
	}
	close(gate)
	for i := 0; i < 2; i++ {
		if err := <-slow; err != nil {
			t.Fatal(err)
		}
	}
	f.mu.Lock()
	made := 0
	for _, e := range f.entries {
		if e.Type == itemTypeFolder && e.Name == "b" {
			made++
		}
	}
	f.mu.Unlock()
	if made != 1 {
		t.Fatalf("folder b made %d times, want 1", made)
	}
	got, err := newClient(t, f, nil).List(context.Background(), set+"/current/")
	if err != nil || len(got) != 4 {
		t.Fatalf("listed %d files, err %v", len(got), err)
	}
}

// An upload whose set is due a save does not wait behind a save already
// under way: that one, or the next checkpoint, takes its changes.
func TestDueSaveDoesNotWaitBehindOneUnderWay(t *testing.T) {
	f := newFake(t)
	c := newClient(t, f, nil)
	put(t, c, set+"/current/a.txt", []byte("a"), remote.Metadata{ModTime: time.Unix(1700000000, 0)})

	c.meta.flushing.Lock()
	done := make(chan error, 1)
	go func() { done <- c.meta.flushSoon(context.Background(), set) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("flushSoon waited for the save under way")
	}
	c.meta.flushing.Unlock()
	if !c.Pending() {
		t.Fatal("the skipped save lost the pending change")
	}
	if err := c.meta.flushSoon(context.Background(), set); err != nil {
		t.Fatal(err)
	}
	if c.Pending() {
		t.Fatal("flushSoon with nothing under way did not save")
	}
}

// Deleting a backup moves its folder to Drime's trash, which is a moment
// however big it is, and empties it from there without anyone waiting.
func TestDeletePrefixTrashesAtOnceThenPurges(t *testing.T) {
	slowDeleteWaits(t)
	f := newFake(t)
	c := newClient(t, f, nil)
	put(t, c, set+"/current/a.txt", []byte("12345"), remote.Metadata{})
	put(t, c, set+"/current/b/c.txt", []byte("678"), remote.Metadata{})
	if err := c.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.slowDelete = 300 * time.Millisecond // deleting forever is slow; the trash is not
	start := time.Now()
	if _, err := c.DeletePrefix(context.Background(), set+"/"); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > 200*time.Millisecond {
		t.Fatalf("took %v: waited for the slow delete instead of trashing", d)
	}
	for p := range f.files() {
		if strings.Contains(p, "/Docs/") {
			t.Fatalf("still in the drive: %s", p)
		}
	}
	c.Close() // waits for the purge
	if n := f.trashedFiles(); n != 0 {
		t.Fatalf("%d files left in the trash after the purge", n)
	}
}

// Without a trash, deleting falls back to deleting forever and waiting.
func TestDeletePrefixWithoutTrashStillDeletes(t *testing.T) {
	slowDeleteWaits(t)
	f := newFake(t)
	c := newClient(t, f, nil)
	put(t, c, set+"/current/a.txt", []byte("12345"), remote.Metadata{})
	if err := c.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.noTrash = true
	if _, err := c.DeletePrefix(context.Background(), set+"/"); err != nil {
		t.Fatal(err)
	}
	for p := range f.files() {
		if strings.Contains(p, "/Docs/") {
			t.Fatalf("left behind: %s", p)
		}
	}
}

// EmptyTrash clears Drime's trash.
func TestEmptyTrash(t *testing.T) {
	f := newFake(t)
	c := newClient(t, f, nil)
	put(t, c, set+"/current/a.txt", []byte("12345"), remote.Metadata{})
	if err := c.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	for id, e := range f.entries {
		if e.Name == "a.txt" {
			f.trash(id)
			break
		}
	}
	f.mu.Unlock()
	if f.trashedFiles() != 1 {
		t.Fatal("test bug: nothing in the trash")
	}
	if err := c.EmptyTrash(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := f.trashedFiles(); n != 0 {
		t.Fatalf("%d files left in the trash", n)
	}
}
