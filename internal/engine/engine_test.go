package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/saurabhhbansal/r2backup/internal/plan"
	"github.com/saurabhhbansal/r2backup/internal/scan"
)

// ---- fakes -----------------------------------------------------------

// fakeUploader records every call it receives, in order, under a mutex, and
// lets a test script per-key behaviour (error, blocking, byte reporting).
type fakeUploader struct {
	mu     sync.Mutex
	calls  []string // "put:key", "copy:from>to", "deletemany:k1,k2"
	puts   map[string]int
	copies map[string]int

	// putErr, if set for a key, is returned by Put instead of success.
	putErr map[string]error
	// copyErr, if set for a key (the "to" key), is returned by Copy.
	copyErr map[string]error
	// deleteErr, if non-nil, is returned by every DeleteMany call.
	deleteErr error

	// block, if set for a key, makes Put wait on the channel (or ctx.Done)
	// before proceeding, after signalling onStart.
	block   map[string]chan struct{}
	onStart func(key string)
}

func newFakeUploader() *fakeUploader {
	return &fakeUploader{
		puts:   map[string]int{},
		copies: map[string]int{},
	}
}

func (f *fakeUploader) Put(ctx context.Context, key string, r io.Reader, size int64, meta map[string]string, onBytes func(int64)) (string, error) {
	f.mu.Lock()
	f.calls = append(f.calls, "put:"+key)
	f.puts[key]++
	blockCh := f.block[key]
	onStart := f.onStart
	f.mu.Unlock()

	if onStart != nil {
		onStart(key)
	}
	if blockCh != nil {
		select {
		case <-blockCh:
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}

	n, err := io.Copy(io.Discard, r)
	if err != nil {
		return "", err
	}
	if onBytes != nil {
		onBytes(n)
	}

	f.mu.Lock()
	kerr := f.putErr[key]
	f.mu.Unlock()
	if kerr != nil {
		return "", kerr
	}
	return "etag-" + key, nil
}

func (f *fakeUploader) Copy(ctx context.Context, from, to string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fmt.Sprintf("copy:%s>%s", from, to))
	f.copies[to]++
	if err := f.copyErr[to]; err != nil {
		return err
	}
	return nil
}

func (f *fakeUploader) DeleteMany(ctx context.Context, keys []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := append([]string(nil), keys...)
	f.calls = append(f.calls, "deletemany:"+fmt.Sprint(cp))
	return f.deleteErr
}

func (f *fakeUploader) callSequence() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *fakeUploader) putCount(key string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.puts[key]
}

// fakeReporter records reported bytes and completed files.
type fakeReporter struct {
	mu       sync.Mutex
	bytes    int64
	complete int64
}

func (r *fakeReporter) AddBytes(n int64) {
	atomic.AddInt64(&r.bytes, n)
}
func (r *fakeReporter) CompleteFile(size int64) {
	atomic.AddInt64(&r.complete, 1)
}

// fakeFileInfo implements fs.FileInfo with fully controllable fields.
type fakeFileInfo struct {
	name    string
	size    int64
	modTime time.Time
}

func (f fakeFileInfo) Name() string       { return f.name }
func (f fakeFileInfo) Size() int64        { return f.size }
func (f fakeFileInfo) Mode() fs.FileMode  { return 0o644 }
func (f fakeFileInfo) ModTime() time.Time { return f.modTime }
func (f fakeFileInfo) IsDir() bool        { return false }
func (f fakeFileInfo) Sys() any           { return nil }

// fakeFS is an in-memory FileSystem. Each key maps to file content and a
// stat sequence: successive calls to Stat pop the next entry (or repeat the
// last one once the sequence is exhausted), which is what lets a test
// simulate a file whose mtime/size changes between the pre- and post-upload
// stat.
type fakeFS struct {
	mu      sync.Mutex
	content map[string][]byte
	stats   map[string][]fakeFileInfo
	statPos map[string]int
	openErr map[string]error
	statErr map[string]error
}

func newFakeFS() *fakeFS {
	return &fakeFS{
		content: map[string][]byte{},
		stats:   map[string][]fakeFileInfo{},
		statPos: map[string]int{},
		openErr: map[string]error{},
		statErr: map[string]error{},
	}
}

func (f *fakeFS) addFile(path string, data []byte, mod time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.content[path] = data
	f.stats[path] = []fakeFileInfo{{name: path, size: int64(len(data)), modTime: mod}}
}

// addStatSequence overrides the stat responses returned for path, one per
// call, holding the last entry once exhausted.
func (f *fakeFS) addStatSequence(path string, infos ...fakeFileInfo) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stats[path] = infos
	f.statPos[path] = 0
}

func (f *fakeFS) Open(path string) (io.ReadCloser, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.openErr[path]; err != nil {
		return nil, err
	}
	data, ok := f.content[path]
	if !ok {
		return nil, fmt.Errorf("fakeFS: no such file %q", path)
	}
	return io.NopCloser(bytesReader(data)), nil
}

func (f *fakeFS) Stat(path string) (fs.FileInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.statErr[path]; err != nil {
		return nil, err
	}
	seq, ok := f.stats[path]
	if !ok || len(seq) == 0 {
		return nil, fmt.Errorf("fakeFS: no stat for %q", path)
	}
	pos := f.statPos[path]
	if pos >= len(seq) {
		pos = len(seq) - 1
	}
	info := seq[pos]
	if pos < len(seq)-1 {
		f.statPos[path] = pos + 1
	}
	return info, nil
}

func bytesReader(b []byte) io.Reader { return &sliceReader{b: b} }

type sliceReader struct {
	b   []byte
	pos int
}

func (r *sliceReader) Read(p []byte) (int, error) {
	if r.pos >= len(r.b) {
		return 0, io.EOF
	}
	n := copy(p, r.b[r.pos:])
	r.pos += n
	return n, nil
}

// ---- helpers -----------------------------------------------------------

func mkEntry(key string, size int64, mod time.Time) scan.Entry {
	return scan.Entry{Key: key, Size: size, ModTime: mod, Kind: scan.KindFile}
}

func setupPlanWithFiles(t *testing.T, fsys *fakeFS, n int) *plan.Plan {
	t.Helper()
	mod := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	p := &plan.Plan{}
	for i := 0; i < n; i++ {
		key := fmt.Sprintf("file%d.txt", i)
		data := []byte(fmt.Sprintf("content-%d", i))
		fsys.addFile(key, data, mod)
		p.Uploads = append(p.Uploads, mkEntry(key, int64(len(data)), mod))
		p.UploadBytes += int64(len(data))
	}
	return p
}

// ---- tests -----------------------------------------------------------

func TestRun_AllFilesUploadedExactlyOnce(t *testing.T) {
	fsys := newFakeFS()
	p := setupPlanWithFiles(t, fsys, 20)
	up := newFakeUploader()
	eng := New(Options{Root: "", Uploader: up, FS: fsys})

	res, err := eng.Run(context.Background(), p)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Uploaded != 20 {
		t.Fatalf("Uploaded = %d, want 20", res.Uploaded)
	}
	if len(res.Failed) != 0 {
		t.Fatalf("Failed = %v, want none", res.Failed)
	}
	for _, u := range p.Uploads {
		if got := up.putCount(u.Key); got != 1 {
			t.Errorf("putCount(%s) = %d, want 1", u.Key, got)
		}
	}
}

func TestRun_ReportedBytesMatchPlan(t *testing.T) {
	fsys := newFakeFS()
	p := setupPlanWithFiles(t, fsys, 10)
	up := newFakeUploader()
	eng := New(Options{Uploader: up, FS: fsys})

	res, err := eng.Run(context.Background(), p)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.UploadedBytes != p.UploadBytes {
		t.Fatalf("UploadedBytes = %d, want %d", res.UploadedBytes, p.UploadBytes)
	}
}

func TestRun_OpenFailureDoesNotStopOtherFiles(t *testing.T) {
	fsys := newFakeFS()
	p := setupPlanWithFiles(t, fsys, 5)
	// file2.txt cannot be opened -- e.g. locked by another process.
	fsys.mu.Lock()
	fsys.openErr["file2.txt"] = errors.New("locked by another process")
	fsys.mu.Unlock()

	up := newFakeUploader()
	eng := New(Options{Uploader: up, FS: fsys})

	res, err := eng.Run(context.Background(), p)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Uploaded != 4 {
		t.Fatalf("Uploaded = %d, want 4 (one file failed to open)", res.Uploaded)
	}
	if len(res.Failed) != 1 {
		t.Fatalf("Failed = %v, want exactly one failure", res.Failed)
	}
	if res.Failed[0].Key != "file2.txt" {
		t.Fatalf("Failed[0].Key = %q, want file2.txt", res.Failed[0].Key)
	}
	if up.putCount("file2.txt") != 0 {
		t.Fatalf("file2.txt should never have reached Put")
	}
}

func TestRun_UploadErrorRecordedNotSucceeded(t *testing.T) {
	fsys := newFakeFS()
	p := setupPlanWithFiles(t, fsys, 5)
	up := newFakeUploader()
	up.putErr = map[string]error{"file3.txt": errors.New("500 internal error")}
	eng := New(Options{Uploader: up, FS: fsys})

	res, err := eng.Run(context.Background(), p)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Uploaded != 4 {
		t.Fatalf("Uploaded = %d, want 4", res.Uploaded)
	}
	if len(res.Failed) != 1 || res.Failed[0].Key != "file3.txt" {
		t.Fatalf("Failed = %v, want exactly file3.txt", res.Failed)
	}
	for _, f := range res.Failed {
		if f.Key == "file3.txt" && f.Err == nil {
			t.Fatalf("failure recorded with nil error")
		}
	}
}

func TestRun_DeletesHappenAfterUploadsAndMoves(t *testing.T) {
	fsys := newFakeFS()
	p := setupPlanWithFiles(t, fsys, 10)
	p.Deletes = []string{"gone1.txt", "gone2.txt"}

	up := newFakeUploader()
	eng := New(Options{Uploader: up, FS: fsys})

	res, err := eng.Run(context.Background(), p)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Deleted != 2 {
		t.Fatalf("Deleted = %d, want 2", res.Deleted)
	}

	seq := up.callSequence()
	deleteIdx := -1
	for i, c := range seq {
		if len(c) >= 11 && c[:11] == "deletemany:" {
			deleteIdx = i
			break
		}
	}
	if deleteIdx == -1 {
		t.Fatalf("no deletemany call recorded: %v", seq)
	}
	for i, c := range seq {
		if i == deleteIdx {
			continue
		}
		if len(c) >= 4 && c[:4] == "put:" && i > deleteIdx {
			t.Fatalf("put %q happened after deletemany (index %d > %d)", c, i, deleteIdx)
		}
	}
	// Every put must precede the single deletemany call.
	for i := 0; i < deleteIdx; i++ {
		if len(seq[i]) >= 11 && seq[i][:11] == "deletemany:" {
			t.Fatalf("found an earlier deletemany at %d before %d", i, deleteIdx)
		}
	}
}

func TestRun_ConcurrencyIsReal(t *testing.T) {
	const n = 8
	const barrier = 4

	fsys := newFakeFS()
	p := setupPlanWithFiles(t, fsys, n)

	up := newFakeUploader()
	up.block = map[string]chan struct{}{}
	release := make(chan struct{})
	for i := 0; i < n; i++ {
		up.block[fmt.Sprintf("file%d.txt", i)] = release
	}

	var inFlight int32
	reached := make(chan struct{})
	var once sync.Once
	up.onStart = func(key string) {
		if atomic.AddInt32(&inFlight, 1) >= barrier {
			once.Do(func() { close(reached) })
		}
	}

	eng := New(Options{Uploader: up, FS: fsys, Workers: 8, MaxWorkers: 8})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	done := make(chan *Result, 1)
	go func() {
		res, err := eng.Run(ctx, p)
		if err != nil {
			t.Errorf("Run: %v", err)
		}
		done <- res
	}()

	select {
	case <-reached:
		// proven: at least `barrier` Put calls were in flight concurrently.
	case <-ctx.Done():
		t.Fatalf("timed out waiting for %d concurrent Put calls; only one worker may be running", barrier)
	}
	close(release)

	select {
	case res := <-done:
		if res.Uploaded != n {
			t.Fatalf("Uploaded = %d, want %d", res.Uploaded, n)
		}
	case <-ctx.Done():
		t.Fatal("Run did not finish after release")
	}
}

func TestRun_CancellationReturnsPromptlyWithOnlyCompletedKeys(t *testing.T) {
	const n = 20
	fsys := newFakeFS()
	p := setupPlanWithFiles(t, fsys, n)

	up := newFakeUploader()
	// Every file blocks until released or ctx is cancelled; Put honours ctx
	// cancellation exactly like a real network call would.
	up.block = map[string]chan struct{}{}
	never := make(chan struct{})
	for _, u := range p.Uploads {
		up.block[u.Key] = never
	}

	var started int32
	allowCancel := make(chan struct{})
	var once sync.Once
	up.onStart = func(key string) {
		if atomic.AddInt32(&started, 1) == 4 {
			once.Do(func() { close(allowCancel) })
		}
	}

	eng := New(Options{Uploader: up, FS: fsys, Workers: 4, MaxWorkers: 4})

	ctx, cancel := context.WithCancel(context.Background())
	type outcome struct {
		res *Result
		err error
	}
	resultCh := make(chan outcome, 1)
	go func() {
		res, err := eng.Run(ctx, p)
		resultCh <- outcome{res, err}
	}()

	<-allowCancel
	start := time.Now()
	cancel()

	select {
	case out := <-resultCh:
		if time.Since(start) > 2*time.Second {
			t.Fatalf("cancellation was not prompt: took %s", time.Since(start))
		}
		// A cancelled run must say so -- see the package doc on Run -- or a
		// caller has no honest way to tell it apart from a run that quietly
		// finished with nothing to do.
		if !errors.Is(out.err, context.Canceled) {
			t.Fatalf("Run err = %v, want context.Canceled", out.err)
		}
		if out.res.Uploaded != 0 {
			t.Fatalf("Uploaded = %d, want 0 -- no Put ever returned success", out.res.Uploaded)
		}
		if out.res.Deleted != 0 {
			t.Fatalf("Deleted = %d, want 0 -- deletes must be skipped on a cancelled run", out.res.Deleted)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return promptly after cancellation")
	}
}

func TestRun_FileChangedOnceIsRetriedAndSucceeds(t *testing.T) {
	fsys := newFakeFS()
	mod1 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	mod2 := time.Date(2026, 1, 1, 0, 0, 5, 0, time.UTC)
	fsys.addFile("f.txt", []byte("hello"), mod1)
	// First upload attempt: pre-stat says mod1/size5, post-stat (after Put)
	// says mod2 -- torn. Second attempt: pre and post both say mod2 -- clean.
	fsys.addStatSequence("f.txt",
		fakeFileInfo{name: "f.txt", size: 5, modTime: mod1}, // pre-stat, attempt 1
		fakeFileInfo{name: "f.txt", size: 5, modTime: mod2}, // post-stat, attempt 1 (changed!)
		fakeFileInfo{name: "f.txt", size: 5, modTime: mod2}, // pre-stat, attempt 2
		fakeFileInfo{name: "f.txt", size: 5, modTime: mod2}, // post-stat, attempt 2 (clean)
	)

	p := &plan.Plan{Uploads: []scan.Entry{mkEntry("f.txt", 5, mod1)}, UploadBytes: 5}
	up := newFakeUploader()
	eng := New(Options{Uploader: up, FS: fsys})

	res, err := eng.Run(context.Background(), p)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Uploaded != 1 {
		t.Fatalf("Uploaded = %d, want 1 after one retry", res.Uploaded)
	}
	if len(res.Failed) != 0 {
		t.Fatalf("Failed = %v, want none", res.Failed)
	}
	if up.putCount("f.txt") != 2 {
		t.Fatalf("putCount = %d, want 2 (original + one retry)", up.putCount("f.txt"))
	}
}

func TestRun_FileChangedTwiceIsRecordedAsFailed(t *testing.T) {
	fsys := newFakeFS()
	mod1 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	mod2 := time.Date(2026, 1, 1, 0, 0, 5, 0, time.UTC)
	mod3 := time.Date(2026, 1, 1, 0, 0, 9, 0, time.UTC)
	fsys.addFile("f.txt", []byte("hello"), mod1)
	fsys.addStatSequence("f.txt",
		fakeFileInfo{name: "f.txt", size: 5, modTime: mod1}, // pre, attempt 1
		fakeFileInfo{name: "f.txt", size: 5, modTime: mod2}, // post, attempt 1 (changed)
		fakeFileInfo{name: "f.txt", size: 5, modTime: mod2}, // pre, attempt 2
		fakeFileInfo{name: "f.txt", size: 5, modTime: mod3}, // post, attempt 2 (changed again)
	)

	p := &plan.Plan{Uploads: []scan.Entry{mkEntry("f.txt", 5, mod1)}, UploadBytes: 5}
	up := newFakeUploader()
	eng := New(Options{Uploader: up, FS: fsys})

	res, err := eng.Run(context.Background(), p)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Uploaded != 0 {
		t.Fatalf("Uploaded = %d, want 0", res.Uploaded)
	}
	if len(res.Failed) != 1 || res.Failed[0].Key != "f.txt" {
		t.Fatalf("Failed = %v, want exactly f.txt", res.Failed)
	}
	if up.putCount("f.txt") != 2 {
		t.Fatalf("putCount = %d, want exactly 2 (one retry, then give up)", up.putCount("f.txt"))
	}
}

func TestRun_EmptyPlanDoesNothing(t *testing.T) {
	up := newFakeUploader()
	eng := New(Options{Uploader: up, FS: newFakeFS()})

	res, err := eng.Run(context.Background(), &plan.Plan{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Uploaded != 0 || res.Moved != 0 || res.Deleted != 0 || len(res.Failed) != 0 {
		t.Fatalf("Result = %+v, want all zero", res)
	}
	if len(up.callSequence()) != 0 {
		t.Fatalf("Uploader received calls on an empty plan: %v", up.callSequence())
	}
}

func TestRun_ReporterReceivesProgressAndCompletions(t *testing.T) {
	fsys := newFakeFS()
	p := setupPlanWithFiles(t, fsys, 6)
	up := newFakeUploader()
	rep := &fakeReporter{}
	eng := New(Options{Uploader: up, FS: fsys, Reporter: rep})

	res, err := eng.Run(context.Background(), p)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if atomic.LoadInt64(&rep.bytes) != p.UploadBytes {
		t.Fatalf("Reporter saw %d bytes, want %d", rep.bytes, p.UploadBytes)
	}
	if atomic.LoadInt64(&rep.complete) != int64(res.Uploaded) {
		t.Fatalf("Reporter saw %d completions, want %d", rep.complete, res.Uploaded)
	}
}

func TestRun_MovesCallCopyNeverPut(t *testing.T) {
	up := newFakeUploader()
	eng := New(Options{Uploader: up, FS: newFakeFS()})

	p := &plan.Plan{
		Moves: []plan.Move{
			{From: "old/a.txt", To: "new/a.txt", Size: 100},
			{From: "old/b.txt", To: "new/b.txt", Size: 200},
		},
	}
	res, err := eng.Run(context.Background(), p)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Moved != 2 {
		t.Fatalf("Moved = %d, want 2", res.Moved)
	}
	if up.copies["new/a.txt"] != 1 || up.copies["new/b.txt"] != 1 {
		t.Fatalf("copies = %v, want one each", up.copies)
	}
	if len(up.puts) != 0 {
		t.Fatalf("Put was called for a move: %v", up.puts)
	}
	// Move sources must be deleted, since the copy succeeded.
	if res.Deleted != 2 {
		t.Fatalf("Deleted = %d, want 2 (both move sources)", res.Deleted)
	}
	seq := up.callSequence()
	if len(seq) != 3 { // 2 copies + 1 deletemany
		t.Fatalf("call sequence = %v, want 2 copies then 1 deletemany", seq)
	}
}

func TestRun_FailedMoveDoesNotDeleteSource(t *testing.T) {
	up := newFakeUploader()
	up.copyErr = map[string]error{"new/a.txt": errors.New("copy failed")}
	eng := New(Options{Uploader: up, FS: newFakeFS()})

	p := &plan.Plan{
		Moves: []plan.Move{{From: "old/a.txt", To: "new/a.txt", Size: 100}},
	}
	res, err := eng.Run(context.Background(), p)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Moved != 0 {
		t.Fatalf("Moved = %d, want 0", res.Moved)
	}
	if len(res.Failed) != 1 || res.Failed[0].Key != "new/a.txt" {
		t.Fatalf("Failed = %v, want exactly new/a.txt", res.Failed)
	}
	// The source must never be deleted when its copy failed -- otherwise the
	// data ends up in neither the old key nor the new one.
	if res.Deleted != 0 {
		t.Fatalf("Deleted = %d, want 0 -- a failed copy's source must survive", res.Deleted)
	}
	for _, c := range up.callSequence() {
		if len(c) >= 11 && c[:11] == "deletemany:" {
			t.Fatalf("DeleteMany was called after a failed move: %v", up.callSequence())
		}
	}
}

func TestRun_NoGoroutineLeak(t *testing.T) {
	fsys := newFakeFS()
	p := setupPlanWithFiles(t, fsys, 50)
	up := newFakeUploader()
	eng := New(Options{Uploader: up, FS: fsys, Workers: 16, MaxWorkers: 16})

	runtime.GC()
	before := runtime.NumGoroutine()

	res, err := eng.Run(context.Background(), p)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Uploaded != 50 {
		t.Fatalf("Uploaded = %d, want 50", res.Uploaded)
	}

	var after int
	deadline := time.Now().Add(2 * time.Second)
	for {
		runtime.GC()
		after = runtime.NumGoroutine()
		if after <= before+1 { // small slack for the Go test runtime itself
			break
		}
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if after > before+1 {
		t.Fatalf("goroutines leaked: before=%d after=%d", before, after)
	}
}

func TestRun_CancelledBeforeStartLeavesNothingUploaded(t *testing.T) {
	fsys := newFakeFS()
	p := setupPlanWithFiles(t, fsys, 5)
	up := newFakeUploader()
	eng := New(Options{Uploader: up, FS: fsys})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	res, err := eng.Run(ctx, p)
	// This used to assert err == nil, which was the M2 bug in miniature: an
	// already-cancelled run did nothing at all and still reported success.
	// A caller has no way to tell that apart from a legitimate empty plan
	// without this error, so backup.Run would record it as a clean run
	// rather than the cancelled one it actually was. See
	// TestACancelledRunReportsAsCancelledNotClean in backup_test.go for that
	// half of the finding.
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run err = %v, want context.Canceled", err)
	}
	if res.Uploaded != 0 {
		t.Fatalf("Uploaded = %d, want 0 on an already-cancelled run", res.Uploaded)
	}
}

// ---- adaptive concurrency ------------------------------------------------

func TestAdjustLimit_ThrottleHalvesDownToFloor(t *testing.T) {
	got := adjustLimit(32, 1000, 1000, true, false, 4, 128)
	if got != 16 {
		t.Fatalf("adjustLimit throttled from 32 = %d, want 16", got)
	}
	got = adjustLimit(6, 1000, 1000, true, false, 4, 128)
	if got != 4 {
		t.Fatalf("adjustLimit throttled from 6 = %d, want floor 4", got)
	}
	got = adjustLimit(4, 1000, 1000, true, false, 4, 128)
	if got != 4 {
		t.Fatalf("adjustLimit throttled at floor = %d, want to stay at 4", got)
	}
}

func TestAdjustLimit_SustainedGrowthRaisesLimit(t *testing.T) {
	current := 32
	// Each window's bytes strictly exceed the previous window, simulating
	// sustained improving throughput.
	windows := []int64{1000, 2000, 3000, 4000}
	prev := int64(0)
	grew := false
	for i, bytes := range windows {
		next := adjustLimit(current, bytes, prev, false, i == 0, 4, 128)
		if i > 0 && next > current {
			grew = true
		}
		current = next
		prev = bytes
	}
	if !grew {
		t.Fatalf("limit never grew across improving windows: ended at %d", current)
	}
	if current > 128 {
		t.Fatalf("limit exceeded max: %d", current)
	}
}

func TestAdjustLimit_GrowthCappedAtMax(t *testing.T) {
	got := adjustLimit(128, 2000, 1000, false, false, 4, 128)
	if got != 128 {
		t.Fatalf("adjustLimit at max = %d, want to stay at 128", got)
	}
}

func TestAdjustLimit_FirstWindowNeverGrows(t *testing.T) {
	// prevWindow (0) is meaningless on the very first sample; growth must
	// wait for a real baseline.
	got := adjustLimit(32, 5000, 0, false, true, 4, 128)
	if got != 32 {
		t.Fatalf("adjustLimit on first window = %d, want unchanged 32", got)
	}
}

func TestAdjustLimit_FlatThroughputHoldsSteady(t *testing.T) {
	got := adjustLimit(32, 1000, 1000, false, false, 4, 128)
	if got != 32 {
		t.Fatalf("adjustLimit on flat throughput = %d, want unchanged 32", got)
	}
	got = adjustLimit(32, 500, 1000, false, false, 4, 128)
	if got != 32 {
		t.Fatalf("adjustLimit on degraded throughput (no throttle) = %d, want unchanged 32", got)
	}
}

func TestGovernor_DrivenByManualTicker(t *testing.T) {
	lim := newLimiter(32, 4, 128)
	r := &runner{opts: Options{MinWorkers: 4, MaxWorkers: 128}}
	ticker := NewManualTicker()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		r.governor(ctx, lim, ticker)
	}()

	// Window 1: throttled -> halves.
	atomic.StoreInt64(&r.throttleHits, 1)
	if !ticker.Tick(ctx) {
		t.Fatal("tick 1 not consumed")
	}
	waitForLimit(t, lim, 16)

	// Window 2: sustained growth, no throttle.
	atomic.StoreInt64(&r.windowBytes, 5000)
	if !ticker.Tick(ctx) {
		t.Fatal("tick 2 not consumed")
	}
	// bytes(5000) compared against prevBytes from window 1 (0, since nothing
	// was added) -- this alone should grow the limit.
	waitForGreater(t, lim, 16)

	cancel()
	wg.Wait() // governor must exit once its context ends
}

func waitForLimit(t *testing.T, lim *limiter, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if lim.currentLimit() == want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("limiter never reached %d, stuck at %d", want, lim.currentLimit())
}

func waitForGreater(t *testing.T, lim *limiter, than int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if lim.currentLimit() > than {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("limiter never grew past %d, stuck at %d", than, lim.currentLimit())
}

func TestLimiter_AcquireUnblocksOnContextCancel(t *testing.T) {
	lim := newLimiter(1, 1, 1)
	ctx, cancel := context.WithCancel(context.Background())

	if !lim.acquire(ctx) {
		t.Fatal("first acquire should succeed")
	}
	// Second acquire blocks since limit is 1 and the slot is held.
	done := make(chan bool, 1)
	go func() {
		done <- lim.acquire(ctx)
	}()

	select {
	case <-done:
		t.Fatal("acquire returned before it should have blocked")
	case <-time.After(50 * time.Millisecond):
	}

	cancel()
	select {
	case ok := <-done:
		if ok {
			t.Fatal("acquire should have returned false after cancellation")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("acquire did not unblock after ctx cancellation")
	}
}
