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

// shortReader gives up partway, the way a file that is being rewritten
// underneath a backup does.
type shortReader struct {
	data []byte
	pos  int
	stop int
	err  error
}

func (s *shortReader) Read(b []byte) (int, error) {
	if s.pos >= s.stop {
		return 0, s.err
	}
	n := copy(b, s.data[s.pos:s.stop])
	s.pos += n
	return n, nil
}

// A body that stops early has to say so in the words of the thing that went
// wrong, not in the words of the server's complaint about it.
//
// Go's transport truncates a request whose body stops, and an S3 server then
// answers 400 IncompleteBody -- it was promised a Content-Length it never
// received. That is a true sentence about the wire and a useless one to the
// person whose backup just failed: it names neither the file nor how far it
// got, and it reads like a protocol fault rather than "this file changed
// while it was being copied", which is what it nearly always is.
//
// This is also the diagnostic that was missing when a twenty-thousand-file
// upload produced sixty-two of these at once and nothing could say why.
func TestAShortBodySaysWhatActuallyStopped(t *testing.T) {
	c := newClient(t)
	ctx := context.Background()

	content := bytes.Repeat([]byte("abcdefgh"), 4096) // 32 KiB
	body := &shortReader{data: content, stop: 4096, err: io.ErrUnexpectedEOF}

	err := c.Put(ctx, remote.PutInput{
		Key:  "short/body.bin",
		Body: body,
		Size: int64(len(content)), // promised more than the reader will give
	})
	if err == nil {
		t.Fatal("a body that stopped short must not report success")
	}
	got := err.Error()
	for _, want := range []string{"4096", "32768", "unexpected EOF"} {
		if !strings.Contains(got, want) {
			t.Errorf("error should mention %q, got: %v", want, err)
		}
	}
}

// The same, for a source that simply ends early with no error of its own --
// a file that was truncated between the stat that sized it and the read.
func TestAFileThatShrankIsNamedAsSuch(t *testing.T) {
	c := newClient(t)
	ctx := context.Background()

	content := bytes.Repeat([]byte("x"), 32*1024)
	body := &shortReader{data: content, stop: 8192, err: io.EOF}

	err := c.Put(ctx, remote.PutInput{
		Key:  "short/shrank.bin",
		Body: body,
		Size: int64(len(content)),
	})
	if err == nil {
		t.Fatal("a file that shrank must not report success")
	}
	if !strings.Contains(err.Error(), "changed or truncated") {
		t.Errorf("error should say the file changed under it, got: %v", err)
	}
}

// A successful upload must not gain an explanation it does not need.
func TestAnUploadThatWorkedIsNotAnnotated(t *testing.T) {
	c := newClient(t)
	ctx := context.Background()
	content := []byte("all of it")
	if err := c.Put(ctx, remote.PutInput{
		Key:  "short/whole.bin",
		Body: bytes.NewReader(content),
		Size: int64(len(content)),
	}); err != nil {
		t.Fatalf("Put: %v", err)
	}
}

// incompleteBodyOnce answers the first PutObject with the 400 an S3 server
// sends when it was promised more bytes than it received.
//
// A synthetic response rather than a real truncation: a body genuinely cut
// short on the way out makes Go's transport fail the round trip with an error
// of its own, which the SDK already retries for other reasons. That would
// pass this test without the classifier under test ever being consulted --
// which is exactly what the first version of it did.
type incompleteBodyOnce struct {
	next http.RoundTripper
	left int32
}

const incompleteBodyXML = `<?xml version="1.0" encoding="UTF-8"?>` +
	`<Error><Code>IncompleteBody</Code>` +
	`<Message>You did not provide the number of bytes specified by the Content-Length HTTP header</Message>` +
	`</Error>`

func (f *incompleteBodyOnce) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Method == http.MethodPut && atomic.AddInt32(&f.left, -1) >= 0 {
		if r.Body != nil {
			_, _ = io.Copy(io.Discard, r.Body)
			_ = r.Body.Close()
		}
		h := make(http.Header)
		h.Set("Content-Type", "application/xml")
		return &http.Response{
			StatusCode: http.StatusBadRequest,
			Status:     "400 Bad Request",
			Body:       io.NopCloser(strings.NewReader(incompleteBodyXML)),
			Header:     h,
			Request:    r,
		}, nil
	}
	return f.next.RoundTrip(r)
}

// A body the server found short is worth sending again.
//
// IncompleteBody is a 400, and the standard retryer calls every 4xx final --
// right for a malformed request, wrong for this one. It means the server was
// promised a Content-Length it did not receive, which is what happens when
// the writer stalls long enough for the server to stop waiting. Nothing about
// the request is wrong and sending it again is the answer.
//
// It only became a safe thing to say once the body was rewindable, but this
// test does not prove that half: it passes no Progress callback, and the
// unfixed code only wrapped -- and so only hid the file's Seek -- when there
// was one to report to. TestARetriedUploadSendsTheWholeFile is the one that
// covers the rewind, and it passes a callback for exactly that reason.
// Production always does: internal/backup hands Put the engine's byte
// counter on every upload.
//
// Verified against the unfixed code: without the classifier this fails with
// the same sentence the twenty-thousand-file run produced -- "api error
// IncompleteBody: You did not provide the number of bytes specified by the
// Content-Length HTTP header".
func TestAShortSendIsRetriedRatherThanFailed(t *testing.T) {
	transport := &incompleteBodyOnce{next: http.DefaultTransport, left: 1}
	client, cleanup := minio.StartWithConfig(t, func(c *remote.Config) {
		c.HTTPClient = &http.Client{Transport: transport}
	})
	t.Cleanup(cleanup)
	ctx := context.Background()

	content := bytes.Repeat([]byte("payload!"), 2048) // 16 KiB
	path := filepath.Join(t.TempDir(), "stalled.bin")
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	if err := client.Put(ctx, remote.PutInput{
		Key:  "stalled/payload.bin",
		Body: f,
		Size: int64(len(content)),
	}); err != nil {
		t.Fatalf("Put after one IncompleteBody: %v", err)
	}
	if atomic.LoadInt32(&transport.left) >= 0 {
		t.Fatal("the injected 400 never fired")
	}

	obj, err := client.Get(ctx, "stalled/payload.bin")
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
}
