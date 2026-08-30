package remote_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/saurabhhbansal/r2backup/internal/remote"
	minio "github.com/saurabhhbansal/r2backup/test/minio"
)

// Resuming a large upload.
//
// The interruption is real in every test here: the transport is cut off
// partway, exactly as a closed lid or a dropped connection cuts it off, and
// then a second Put is made with a fresh client over the same store. What is
// asserted is that the second one does not send the bytes the first one
// already got there, and that the object it completes is byte-for-byte the
// original.

// memStore is a ResumeStore in a map. The bbolt one is internal/index's and
// is tested there; what matters here is the protocol against a real server.
type memStore struct {
	mu sync.Mutex
	m  map[string]remote.Upload
}

func newMemStore() *memStore { return &memStore{m: map[string]remote.Upload{}} }

func (s *memStore) Resumable(key string) (remote.Upload, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.m[key]
	return u, ok, nil
}

func (s *memStore) SaveResumable(u remote.Upload) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[u.Key] = u
	return nil
}

func (s *memStore) ForgetResumable(key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m, key)
	return nil
}

func (s *memStore) AllResumable() ([]remote.Upload, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]remote.Upload, 0, len(s.m))
	for _, u := range s.m {
		out = append(out, u)
	}
	return out, nil
}

func (s *memStore) len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.m)
}

// cutAfter lets n part uploads through and then refuses every one after,
// standing in for the moment a machine is shut down or a link drops.
type cutAfter struct {
	next  http.RoundTripper
	allow int32
}

var errCut = errors.New("connection cut")

func (c *cutAfter) RoundTrip(r *http.Request) (*http.Response, error) {
	// Only part uploads are cut. Creating and listing the upload must keep
	// working, or the test would be measuring the wrong failure.
	if r.Method == http.MethodPut && r.URL.Query().Get("uploadId") != "" {
		if atomic.AddInt32(&c.allow, -1) < 0 {
			return nil, errCut
		}
	}
	return c.next.RoundTrip(r)
}

// smallParts keeps these tests honest without writing a 64MiB file for each
// one: the thresholds are configuration, and 5MiB is S3's own floor for
// every part but the last, so this is the smallest shape a real multipart
// upload can take.
const (
	testPartSize  = 5 << 20
	testThreshold = 6 << 20
	testFileSize  = 12 << 20 // three parts: 5MiB, 5MiB, 2MiB
)

func resumeClient(t *testing.T, store remote.ResumeStore, transport http.RoundTripper) (*remote.Client, func()) {
	t.Helper()
	return minio.StartWithConfig(t, func(c *remote.Config) {
		c.Resume = store
		c.MultipartThreshold = testThreshold
		c.PartSize = testPartSize
		// One attempt. A cut connection is retryable and rightly so, but
		// waiting out six attempts and thirty seconds of backoff per part is
		// not what these tests are measuring.
		c.MaxRetryAttempts = 1
		if transport != nil {
			c.HTTPClient = &http.Client{Transport: transport}
		}
	})
}

func bigFile(t *testing.T) (path string, content []byte) {
	t.Helper()
	content = make([]byte, testFileSize)
	if _, err := rand.Read(content); err != nil {
		t.Fatal(err)
	}
	path = filepath.Join(t.TempDir(), "big.bin")
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatal(err)
	}
	return path, content
}

func putFile(t *testing.T, c *remote.Client, key, path string, onBytes func(int64)) error {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	return c.Put(context.Background(), remote.PutInput{
		Key:      key,
		Body:     f,
		Size:     info.Size(),
		Metadata: remote.Metadata{ModTime: info.ModTime(), Mode: 0o644},
		Progress: onBytes,
	})
}

// The one that matters: an upload cut off partway carries on from where it
// stopped rather than starting again.
func TestAnInterruptedUploadCarriesOnFromWhereItStopped(t *testing.T) {
	path, _ := bigFile(t)
	store := newMemStore()

	// One part gets through, then the connection dies.
	cut := &cutAfter{next: http.DefaultTransport, allow: 1}
	broken, cleanupBroken := resumeClient(t, store, cut)
	err := putFile(t, broken, "big/file.bin", path, nil)
	if err == nil {
		t.Fatal("the cut connection should have failed the upload")
	}
	cleanupBroken()

	// The part that landed was written down, which is the whole point.
	u, ok, _ := store.Resumable("big/file.bin")
	if !ok {
		t.Fatal("nothing was recorded, so nothing could be resumed")
	}
	if len(u.Parts) != 1 {
		t.Fatalf("recorded %d parts, want the 1 that got through", len(u.Parts))
	}
	// Which part won the race is not decided here -- parts go up
	// concurrently, so the one that got through may be the 2MiB tail as
	// easily as a 5MiB body. What matters is that its bytes were counted.
	if got := u.Done(); got != testPartSize && got != testFileSize-2*testPartSize {
		t.Errorf("recorded %d bytes done, want one whole part", got)
	}
	if u.UploadID == "" {
		t.Error("no upload id was recorded, so the server's parts are unreachable")
	}
}

// End to end, against one server: interrupt, then finish, and check both the
// bytes and that the second attempt did not re-send the first attempt's work.
func TestAResumedUploadDoesNotSendTheSameBytesTwice(t *testing.T) {
	path, content := bigFile(t)
	store := newMemStore()

	counting := &countingTransport{next: http.DefaultTransport}
	cut := &cutAfter{next: counting, allow: 1}
	client, cleanup := resumeClient(t, store, cut)
	t.Cleanup(cleanup)

	if err := putFile(t, client, "big/file.bin", path, nil); err == nil {
		t.Fatal("the cut connection should have failed the upload")
	}
	firstAttemptParts := counting.parts()
	if firstAttemptParts == 0 {
		t.Fatal("no part reached the server on the first attempt")
	}

	// Let everything through now, on the same server and the same store.
	atomic.StoreInt32(&cut.allow, 1<<30)
	counting.reset()

	var reported int64
	if err := putFile(t, client, "big/file.bin", path, func(n int64) {
		atomic.AddInt64(&reported, n)
	}); err != nil {
		t.Fatalf("the resumed upload failed: %v", err)
	}

	// Three parts in total; one landed before the cut, so the second attempt
	// should have sent two. If resume were not working it would send three.
	if got := counting.parts(); got != 2 {
		t.Errorf("the resumed attempt sent %d parts, want the 2 that were missing", got)
	}
	// Progress still adds up to the whole file: the part already on the
	// server is reported once, up front, or the bar would finish short.
	if n := atomic.LoadInt64(&reported); n != int64(len(content)) {
		t.Errorf("progress reported %d bytes, want the file's %d", n, len(content))
	}
	if store.len() != 0 {
		t.Error("a completed upload should leave no record behind")
	}

	obj, err := client.Get(context.Background(), "big/file.bin")
	if err != nil {
		t.Fatal(err)
	}
	defer obj.Body.Close()
	got, err := io.ReadAll(obj.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("the finished object is %d bytes and does not match the original %d", len(got), len(content))
	}
}

// countingTransport counts part uploads that actually reach the server.
type countingTransport struct {
	next http.RoundTripper
	n    int32
}

func (c *countingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Method == http.MethodPut && r.URL.Query().Get("uploadId") != "" {
		atomic.AddInt32(&c.n, 1)
	}
	return c.next.RoundTrip(r)
}

func (c *countingTransport) parts() int { return int(atomic.LoadInt32(&c.n)) }
func (c *countingTransport) reset()     { atomic.StoreInt32(&c.n, 0) }

// Parts already on the server describe the bytes the file held when they were
// cut. If the file has changed since, they describe a version nobody is
// asking for -- so they must be thrown away, not completed into an object
// that is half one file and half another.
func TestAChangedFileIsNotResumedIntoAFrankenstein(t *testing.T) {
	path, _ := bigFile(t)
	store := newMemStore()

	cut := &cutAfter{next: http.DefaultTransport, allow: 1}
	client, cleanup := resumeClient(t, store, cut)
	t.Cleanup(cleanup)

	if err := putFile(t, client, "big/file.bin", path, nil); err == nil {
		t.Fatal("the cut connection should have failed the upload")
	}
	if _, ok, _ := store.Resumable("big/file.bin"); !ok {
		t.Fatal("nothing recorded to resume from")
	}

	// Rewrite the file with different content, and make sure the modification
	// time really moved: a same-size rewrite inside the filesystem's
	// timestamp granularity is exactly the case this check must not miss.
	replacement := make([]byte, testFileSize)
	if _, err := rand.Read(replacement); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, replacement, 0o644); err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(2 * time.Hour)
	if err := os.Chtimes(path, later, later); err != nil {
		t.Fatal(err)
	}

	atomic.StoreInt32(&cut.allow, 1<<30)
	if err := putFile(t, client, "big/file.bin", path, nil); err != nil {
		t.Fatalf("the restarted upload failed: %v", err)
	}

	obj, err := client.Get(context.Background(), "big/file.bin")
	if err != nil {
		t.Fatal(err)
	}
	defer obj.Body.Close()
	got, err := io.ReadAll(obj.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, replacement) {
		t.Fatal("the stored object is not the file that was on disk; a stale part was completed into it")
	}
}

// An upload nobody is going to finish is still billed, and its parts do not
// appear in any object listing -- so it is a charge with nothing on screen to
// explain it. The sweep abandons them.
func TestStaleUploadsAreAbandonedRatherThanBilledForever(t *testing.T) {
	path, _ := bigFile(t)
	store := newMemStore()

	cut := &cutAfter{next: http.DefaultTransport, allow: 1}
	client, cleanup := resumeClient(t, store, cut)
	t.Cleanup(cleanup)

	if err := putFile(t, client, "big/file.bin", path, nil); err == nil {
		t.Fatal("the cut connection should have failed the upload")
	}

	// Not old enough yet: a sweep must not throw away work someone is coming
	// back to in an hour.
	n, err := client.AbandonStaleUploads(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("abandoned %d fresh uploads, want none", n)
	}
	if store.len() != 1 {
		t.Fatal("the fresh record should still be there")
	}

	// Age it past the limit.
	u, _, _ := store.Resumable("big/file.bin")
	u.StartedAt = time.Now().Add(-2 * remote.ResumeMaxAge).UnixNano()
	if err := store.SaveResumable(u); err != nil {
		t.Fatal(err)
	}

	n, err = client.AbandonStaleUploads(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("abandoned %d, want the 1 stale upload", n)
	}
	if store.len() != 0 {
		t.Error("the swept record should be gone")
	}
}

// Without a store the old behaviour stands: correct, and merely wasteful
// after an interruption. Nothing should depend on a store being configured.
func TestALargeUploadStillWorksWithNowhereToRecordIt(t *testing.T) {
	path, content := bigFile(t)
	client, cleanup := minio.StartWithConfig(t, func(c *remote.Config) {
		c.MultipartThreshold = testThreshold
		c.PartSize = testPartSize
	})
	t.Cleanup(cleanup)

	if err := putFile(t, client, "big/plain.bin", path, nil); err != nil {
		t.Fatalf("Put: %v", err)
	}
	obj, err := client.Get(context.Background(), "big/plain.bin")
	if err != nil {
		t.Fatal(err)
	}
	defer obj.Body.Close()
	got, err := io.ReadAll(obj.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, content) {
		t.Fatal("the object does not match the file")
	}
}
