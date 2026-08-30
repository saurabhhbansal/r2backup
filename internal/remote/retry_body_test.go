package remote_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/saurabhhbansal/r2backup/internal/remote"
	minio "github.com/saurabhhbansal/r2backup/test/minio"
)

// failFirstPuts fails the first n PutObject attempts with a 500, so the SDK
// retries them. Everything else is passed through to the real server.
//
// 500 rather than a dropped connection because that is the failure the
// retryer treats as retryable without any doubt, and the point here is the
// retry, not the classification.
type failFirstPuts struct {
	next http.RoundTripper
	left int32
}

func (f *failFirstPuts) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Method == http.MethodPut && atomic.AddInt32(&f.left, -1) >= 0 {
		// The body is drained first, as a real server that answers 500
		// mid-upload would have consumed some of it. Leaving it unread
		// would let a rewind succeed for the wrong reason.
		_, _ = io.Copy(io.Discard, r.Body)
		_ = r.Body.Close()
		return &http.Response{
			StatusCode: http.StatusInternalServerError,
			Status:     "500 Internal Server Error",
			Body:       io.NopCloser(strings.NewReader("")),
			Header:     make(http.Header),
			Request:    r,
		}, nil
	}
	return f.next.RoundTrip(r)
}

// An upload that has to be retried must send the whole file the second time.
//
// It did not. Put wrapped the body in a progress counter that implemented
// only io.Reader, which hid *os.File's Seek -- and the SDK can only rewind a
// body that is an io.Seeker. The retry therefore sent what was left of an
// already-consumed stream against a Content-Length describing the whole
// file, and the server answered:
//
//	IncompleteBody: You did not provide the number of bytes specified by
//	the Content-Length HTTP header
//
// which is what a twenty-thousand-file backup hit in bursts on a loaded CI
// runner: dozens of files reported as failed uploads, and a clean re-run
// afterwards, because it takes a transient failure to trigger at all.
func TestARetriedUploadSendsTheWholeFile(t *testing.T) {
	transport := &failFirstPuts{next: http.DefaultTransport, left: 1}
	client, cleanup := minio.StartWithConfig(t, func(c *remote.Config) {
		c.HTTPClient = &http.Client{Transport: transport}
	})
	t.Cleanup(cleanup)
	ctx := context.Background()

	// Big enough that a truncated resend cannot coincidentally match, and
	// random so a partial write cannot compare equal to the whole.
	content := make([]byte, 512*1024)
	if _, err := rand.Read(content); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "payload.bin")
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	var reported int64
	err = client.Put(ctx, remote.PutInput{
		Key:      "retried/payload.bin",
		Body:     f,
		Size:     int64(len(content)),
		Progress: func(n int64) { atomic.AddInt64(&reported, n) },
	})
	if err != nil {
		t.Fatalf("Put after one retryable failure: %v", err)
	}
	if atomic.LoadInt32(&transport.left) >= 0 {
		t.Fatal("the injected failure never fired, so nothing was retried")
	}

	obj, err := client.Get(ctx, "retried/payload.bin")
	if err != nil {
		t.Fatal(err)
	}
	defer obj.Body.Close()
	got, err := io.ReadAll(obj.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("stored %d bytes, want the original %d", len(got), len(content))
	}

	// A progress bar must not claim more bytes went up than the file has.
	// The first attempt's bytes were counted and then thrown away by the
	// rewind; counting them again would report 150% of a 100% upload.
	if n := atomic.LoadInt64(&reported); n != int64(len(content)) {
		t.Errorf("progress reported %d bytes for a %d-byte file", n, len(content))
	}
}
