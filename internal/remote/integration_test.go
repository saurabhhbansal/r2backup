package remote_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/saurabhhbansal/r2backup/internal/remote"
	minio "github.com/saurabhhbansal/r2backup/test/minio"
)

// newClient starts a real MinIO server for the duration of the test and
// returns a remote.Client pointed at it. Every integration test in this
// file goes through here rather than a mock, so they exercise actual S3
// wire behavior: real pagination, a real multipart upload, a real
// server-side copy.
func newClient(t *testing.T) *remote.Client {
	t.Helper()
	client, cleanup := minio.Start(t)
	t.Cleanup(cleanup)
	return client
}

func TestPutGetRoundTrip(t *testing.T) {
	c := newClient(t)
	ctx := context.Background()

	content := []byte("the quick brown fox jumps over the lazy dog")
	key := "docs/fox.txt"

	err := c.Put(ctx, remote.PutInput{
		Key:      key,
		Body:     bytes.NewReader(content),
		Size:     int64(len(content)),
		Metadata: remote.Metadata{ModTime: time.Now(), Mode: 0o644},
	})
	if err != nil {
		t.Fatalf("Put: %v", err)
	}

	obj, err := c.Get(ctx, key)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer obj.Body.Close()

	got, err := io.ReadAll(obj.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Errorf("Get returned %q, want %q", got, content)
	}
	if obj.Size != int64(len(content)) {
		t.Errorf("Size = %d, want %d", obj.Size, len(content))
	}
}

func TestMetadataSurvivesRoundTrip(t *testing.T) {
	c := newClient(t)
	ctx := context.Background()

	// Nanosecond precision matters: two saves of the same file a few
	// milliseconds apart must not collapse to the same mtime.
	mtime := time.Date(2026, 5, 17, 8, 30, 0, 123456789, time.UTC)
	meta := remote.Metadata{
		ModTime: mtime,
		Mode:    0o640,
		Symlink: "../elsewhere/target.txt",
	}
	key := "symlinks/a-link"

	err := c.Put(ctx, remote.PutInput{
		Key:      key,
		Body:     bytes.NewReader(nil),
		Size:     0,
		Metadata: meta,
	})
	if err != nil {
		t.Fatalf("Put: %v", err)
	}

	head, err := c.Head(ctx, key)
	if err != nil {
		t.Fatalf("Head: %v", err)
	}
	if !head.Metadata.ModTime.Equal(mtime) {
		t.Errorf("ModTime = %v, want %v", head.Metadata.ModTime, mtime)
	}
	if head.Metadata.Mode.Perm() != meta.Mode.Perm() {
		t.Errorf("Mode = %v, want %v", head.Metadata.Mode.Perm(), meta.Mode.Perm())
	}
	if head.Metadata.Symlink != meta.Symlink {
		t.Errorf("Symlink = %q, want %q", head.Metadata.Symlink, meta.Symlink)
	}
}

func TestPutGetLargeFileGoesMultipart(t *testing.T) {
	c := newClient(t)
	ctx := context.Background()

	const size = int64(80 << 20) // 80MiB: over the 64MB multipart threshold
	key := "large/blob.bin"

	// Two independent readers built from the same seed produce
	// byte-identical streams, which lets the test compare 80MB of content
	// without ever holding it all in memory: generate it once into the
	// upload, regenerate it during the download comparison.
	seed := int64(42)
	genA := newSeededReader(seed, size)
	genB := newSeededReader(seed, size)

	err := c.Put(ctx, remote.PutInput{
		Key:      key,
		Body:     genA,
		Size:     size,
		Metadata: remote.Metadata{ModTime: time.Now(), Mode: 0o644},
	})
	if err != nil {
		t.Fatalf("Put: %v", err)
	}

	head, err := c.Head(ctx, key)
	if err != nil {
		t.Fatalf("Head: %v", err)
	}
	if head.Size != size {
		t.Fatalf("Head size = %d, want %d", head.Size, size)
	}

	obj, err := c.Get(ctx, key)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer obj.Body.Close()

	if err := compareStreams(genB, obj.Body); err != nil {
		t.Errorf("uploaded content does not match source: %v", err)
	}
}

// newSeededReader returns a deterministic pseudo-random byte stream of the
// given length, so two independent readers built from the same seed
// produce byte-identical output without either one buffering it.
func newSeededReader(seed int64, size int64) io.Reader {
	return io.LimitReader(&pcgReader{state: uint64(seed) + 1}, size)
}

// pcgReader is a tiny, deterministic, seedable byte generator -- not
// cryptographic, just repeatable, which is all a "does the multipart
// upload preserve bytes" test needs.
type pcgReader struct {
	state uint64
}

func (p *pcgReader) Read(b []byte) (int, error) {
	for i := range b {
		p.state = p.state*6364136223846793005 + 1442695040888963407
		b[i] = byte(p.state >> 56)
	}
	return len(b), nil
}

func compareStreams(a, b io.Reader) error {
	bufA := make([]byte, 32*1024)
	bufB := make([]byte, 32*1024)
	var offset int64
	for {
		nA, errA := io.ReadFull(a, bufA)
		nB, errB := io.ReadFull(b, bufB)
		if nA != nB || !bytes.Equal(bufA[:nA], bufB[:nB]) {
			return fmt.Errorf("mismatch at offset %d", offset)
		}
		offset += int64(nA)
		if errA == io.EOF && errB == io.EOF {
			return nil
		}
		if errA == io.ErrUnexpectedEOF && errB == io.ErrUnexpectedEOF {
			return nil
		}
		if errA != nil && errA != io.EOF && errA != io.ErrUnexpectedEOF {
			return errA
		}
		if errB != nil && errB != io.EOF && errB != io.ErrUnexpectedEOF {
			return errB
		}
		if (errA == io.EOF) != (errB == io.EOF) {
			return fmt.Errorf("stream length mismatch near offset %d", offset)
		}
	}
}

func TestPutProgressCallbackTotalsObjectSize(t *testing.T) {
	c := newClient(t)
	ctx := context.Background()

	const size = int64(70 << 20) // over the multipart threshold
	key := "large/progress.bin"

	var total int64
	var calls int64
	err := c.Put(ctx, remote.PutInput{
		Key:      key,
		Body:     newSeededReader(7, size),
		Size:     size,
		Metadata: remote.Metadata{ModTime: time.Now(), Mode: 0o644},
		Progress: func(n int64) {
			atomic.AddInt64(&total, n)
			atomic.AddInt64(&calls, 1)
		},
	})
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if calls == 0 {
		t.Fatal("Progress callback was never called")
	}
	if total != size {
		t.Errorf("Progress total = %d, want %d", total, size)
	}
}

func TestCopyIsServerSideAndLeavesSourceIntact(t *testing.T) {
	c := newClient(t)
	ctx := context.Background()

	content := []byte("copy me, please")
	srcKey := "orig/file.txt"
	dstKey := "trash/file.txt"

	err := c.Put(ctx, remote.PutInput{
		Key:      srcKey,
		Body:     bytes.NewReader(content),
		Size:     int64(len(content)),
		Metadata: remote.Metadata{ModTime: time.Now(), Mode: 0o644},
	})
	if err != nil {
		t.Fatalf("Put: %v", err)
	}

	if err := c.Copy(ctx, srcKey, dstKey); err != nil {
		t.Fatalf("Copy: %v", err)
	}

	// The source must still be there, byte for byte.
	srcObj, err := c.Get(ctx, srcKey)
	if err != nil {
		t.Fatalf("Get source after copy: %v", err)
	}
	defer srcObj.Body.Close()
	srcGot, _ := io.ReadAll(srcObj.Body)
	if !bytes.Equal(srcGot, content) {
		t.Errorf("source content after copy = %q, want %q", srcGot, content)
	}

	dstObj, err := c.Get(ctx, dstKey)
	if err != nil {
		t.Fatalf("Get destination: %v", err)
	}
	defer dstObj.Body.Close()
	dstGot, _ := io.ReadAll(dstObj.Body)
	if !bytes.Equal(dstGot, content) {
		t.Errorf("destination content = %q, want %q", dstGot, content)
	}
}

func TestListPaginatesOverOneThousandObjects(t *testing.T) {
	c := newClient(t)
	ctx := context.Background()

	const n = 1500
	prefix := "listing/"

	var wg sync.WaitGroup
	sem := make(chan struct{}, 50) // bound concurrency; still much faster than serial
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			key := fmt.Sprintf("%sitem-%05d", prefix, i)
			err := c.Put(ctx, remote.PutInput{
				Key:      key,
				Body:     bytes.NewReader(nil),
				Size:     0,
				Metadata: remote.Metadata{ModTime: time.Now(), Mode: 0o644},
			})
			if err != nil {
				errs <- fmt.Errorf("put %s: %w", key, err)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}

	entries, err := c.List(ctx, prefix)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != n {
		t.Fatalf("List returned %d entries, want %d", len(entries), n)
	}

	seen := make(map[string]bool, n)
	for _, e := range entries {
		if seen[e.Key] {
			t.Errorf("key %q listed more than once", e.Key)
		}
		seen[e.Key] = true
	}
	for i := 0; i < n; i++ {
		key := fmt.Sprintf("%sitem-%05d", prefix, i)
		if !seen[key] {
			t.Errorf("key %q missing from listing", key)
		}
	}
}

func TestDeleteRemovesObject(t *testing.T) {
	c := newClient(t)
	ctx := context.Background()

	key := "to-delete/file.txt"
	err := c.Put(ctx, remote.PutInput{
		Key:      key,
		Body:     bytes.NewReader([]byte("bye")),
		Size:     3,
		Metadata: remote.Metadata{ModTime: time.Now(), Mode: 0o644},
	})
	if err != nil {
		t.Fatalf("Put: %v", err)
	}

	if err := c.Delete(ctx, key); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	_, err = c.Head(ctx, key)
	if !errors.Is(err, remote.ErrNotFound) {
		t.Errorf("Head after Delete: err = %v, want ErrNotFound", err)
	}
}

func TestHeadMissingKeyIsNotFound(t *testing.T) {
	c := newClient(t)
	ctx := context.Background()

	_, err := c.Head(ctx, "does/not/exist.txt")
	if !errors.Is(err, remote.ErrNotFound) {
		t.Errorf("Head: err = %v, want ErrNotFound", err)
	}
}

func TestGetMissingKeyIsNotFound(t *testing.T) {
	c := newClient(t)
	ctx := context.Background()

	_, err := c.Get(ctx, "does/not/exist.txt")
	if !errors.Is(err, remote.ErrNotFound) {
		t.Errorf("Get: err = %v, want ErrNotFound", err)
	}
}

func TestDeleteBatch(t *testing.T) {
	c := newClient(t)
	ctx := context.Background()

	const n = 25
	keys := make([]string, n)
	for i := range keys {
		keys[i] = fmt.Sprintf("batch/item-%d", i)
		err := c.Put(ctx, remote.PutInput{
			Key:      keys[i],
			Body:     bytes.NewReader(nil),
			Size:     0,
			Metadata: remote.Metadata{ModTime: time.Now(), Mode: 0o644},
		})
		if err != nil {
			t.Fatalf("Put %s: %v", keys[i], err)
		}
	}

	if err := c.DeleteBatch(ctx, keys); err != nil {
		t.Fatalf("DeleteBatch: %v", err)
	}

	for _, k := range keys {
		if _, err := c.Head(ctx, k); !errors.Is(err, remote.ErrNotFound) {
			t.Errorf("Head(%s) after DeleteBatch: err = %v, want ErrNotFound", k, err)
		}
	}
}

func TestOddballKeysRoundTrip(t *testing.T) {
	c := newClient(t)
	ctx := context.Background()

	keys := []string{
		"with spaces/file name.txt",
		"hash/name#1.txt",
		"query/name?ok.txt",
		"percent/100%done.txt",
		"emoji/📦-package.txt",
		"deeply/nested/path/that/keeps/going/for/a/while/leaf.txt",
	}

	for _, key := range keys {
		t.Run(key, func(t *testing.T) {
			// MinIO's Windows build stores each object as an NTFS file, so it
			// inherits NTFS's forbidden characters and rejects this key
			// outright. Real R2 is object storage with no such filesystem
			// underneath and accepts it. Skipping here keeps the test honest
			// about what it is measuring -- the test server's limits, not the
			// product's -- rather than weakening the key list for every
			// platform because one test double cannot hold it.
			if runtime.GOOS == "windows" && strings.ContainsAny(key, `?*:"<>|`) {
				t.Skipf("MinIO on Windows cannot store %q: it maps object names onto NTFS filenames. R2 accepts it.", key)
			}
			content := []byte("payload for " + key)
			err := c.Put(ctx, remote.PutInput{
				Key:      key,
				Body:     bytes.NewReader(content),
				Size:     int64(len(content)),
				Metadata: remote.Metadata{ModTime: time.Now(), Mode: 0o644},
			})
			if err != nil {
				t.Fatalf("Put: %v", err)
			}

			obj, err := c.Get(ctx, key)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			defer obj.Body.Close()
			got, err := io.ReadAll(obj.Body)
			if err != nil {
				t.Fatalf("read body: %v", err)
			}
			if !bytes.Equal(got, content) {
				t.Errorf("content = %q, want %q", got, content)
			}

			// Also confirmed via Copy, since CopySource must survive the
			// same characters URL-encoded rather than raw.
			dstKey := key + ".copy"
			if err := c.Copy(ctx, key, dstKey); err != nil {
				t.Fatalf("Copy: %v", err)
			}
			cObj, err := c.Get(ctx, dstKey)
			if err != nil {
				t.Fatalf("Get copy: %v", err)
			}
			defer cObj.Body.Close()
			cGot, _ := io.ReadAll(cObj.Body)
			if !bytes.Equal(cGot, content) {
				t.Errorf("copied content = %q, want %q", cGot, content)
			}
		})
	}
}

// DeletePrefix is what `remove --purge` is built on, so the property that
// matters is not that it deletes -- it is what it leaves alone. Two sets
// whose names make one prefix a byte prefix of the other is not a contrived
// case: "Docs" and "Docs2" are an ordinary pair of names, and S3 matches
// prefixes by bytes with no idea that a "/" means anything. Getting this
// wrong deletes a backup nobody asked to delete.
func TestDeletePrefixStopsAtTheBoundaryItIsGiven(t *testing.T) {
	c := newClient(t)
	ctx := context.Background()

	docs := []string{
		"machines/pc/Docs/current/a.txt",
		"machines/pc/Docs/current/sub/b.txt",
		"machines/pc/Docs/trash/2026-08-29/a~130353-6ae44ae0.txt",
	}
	survivors := []string{
		"machines/pc/Docs2/current/keepme.txt",
		"machines/pc/Docsuments/current/keepme.txt",
		"machines/pc/Other/current/keepme.txt",
	}
	body := []byte("0123456789")
	for _, k := range append(append([]string{}, docs...), survivors...) {
		if err := c.Put(ctx, remote.PutInput{
			Key: k, Body: bytes.NewReader(body), Size: int64(len(body)),
			Metadata: remote.Metadata{ModTime: time.Now(), Mode: 0o644},
		}); err != nil {
			t.Fatalf("Put %q: %v", k, err)
		}
	}

	got, err := c.DeletePrefix(ctx, "machines/pc/Docs/")
	if err != nil {
		t.Fatalf("DeletePrefix: %v", err)
	}
	if got.Objects != len(docs) {
		t.Errorf("deleted %d objects, want %d", got.Objects, len(docs))
	}
	if want := int64(len(docs) * len(body)); got.Bytes != want {
		t.Errorf("reported %d bytes, want %d", got.Bytes, want)
	}

	for _, k := range docs {
		if _, err := c.Head(ctx, k); err == nil {
			t.Errorf("%q survived a purge of its own set", k)
		}
	}
	for _, k := range survivors {
		if _, err := c.Head(ctx, k); err != nil {
			t.Errorf("%q was deleted by a purge of a different set: %v", k, err)
		}
	}
}

// Purging a set that never had anything in the bucket must report nothing
// rather than fail: an empty folder is a legitimate thing to have added, and
// removing it is not an error.
func TestDeletePrefixOfNothingIsNotAnError(t *testing.T) {
	c := newClient(t)
	got, err := c.DeletePrefix(context.Background(), "machines/pc/NeverUsed/")
	if err != nil {
		t.Fatalf("DeletePrefix on an empty prefix: %v", err)
	}
	if got.Objects != 0 || got.Bytes != 0 {
		t.Errorf("DeletePrefix reported %+v, want a zero result", got)
	}
}

// TestListPrefixesReturnsOnlyTheNextLevel is the call a computer that has
// never backed anything up uses to find out what is in the bucket. The point
// of it is the delimiter: without one, learning three set names would mean
// listing every object under machines/, which for a real backup is tens of
// thousands of keys and the operations to match.
func TestListPrefixesReturnsOnlyTheNextLevel(t *testing.T) {
	c := newClient(t)
	ctx := context.Background()

	for _, key := range []string{
		"machines/desktop/Documents/current/a.txt",
		"machines/desktop/Documents/current/deep/b.txt",
		"machines/desktop/Code/current/c.txt",
		"machines/laptop/Documents/current/d.txt",
		"machines/laptop/Photos/current/e.txt",
		// Not under machines/ at all: must not appear.
		"r2backup/state.json",
	} {
		body := []byte("x")
		if err := c.Put(ctx, remote.PutInput{
			Key: key, Body: bytes.NewReader(body), Size: int64(len(body)),
			Metadata: remote.Metadata{ModTime: time.Now(), Mode: 0o644},
		}); err != nil {
			t.Fatalf("Put %s: %v", key, err)
		}
	}

	machines, err := c.ListPrefixes(ctx, "machines/")
	if err != nil {
		t.Fatalf("ListPrefixes: %v", err)
	}
	if got, want := strings.Join(machines, ","), "desktop,laptop"; got != want {
		t.Fatalf("ListPrefixes(machines/) = %q, want %q", got, want)
	}

	names, err := c.ListPrefixes(ctx, "machines/desktop/")
	if err != nil {
		t.Fatalf("ListPrefixes: %v", err)
	}
	// Code and Documents, and specifically not "current" or "deep" -- the
	// delimiter has to stop at the first "/" past the prefix.
	if got, want := strings.Join(names, ","), "Code,Documents"; got != want {
		t.Fatalf("ListPrefixes(machines/desktop/) = %q, want %q", got, want)
	}

	empty, err := c.ListPrefixes(ctx, "machines/nosuchpc/")
	if err != nil {
		t.Fatalf("ListPrefixes of nothing: %v", err)
	}
	if len(empty) != 0 {
		t.Fatalf("ListPrefixes of an unused prefix = %v, want none", empty)
	}
}
