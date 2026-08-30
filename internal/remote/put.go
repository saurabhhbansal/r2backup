package remote

import (
	"context"
	"fmt"
	"io"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// PutInput describes an object to upload.
type PutInput struct {
	Key  string
	Body io.Reader
	// Size is the exact byte count of Body. It is required: it decides
	// whether Put uses a single PutObject call or a multipart upload, and
	// it is also stamped into Metadata.
	Size     int64
	Metadata Metadata
	// ContentType is optional; S3 defaults to application/octet-stream
	// when it is left empty.
	ContentType string
	// Progress, if set, is called as bytes are actually written to the
	// connection -- once per read, whether that read belongs to a single
	// PutObject request or to one part of a multipart upload. Each call
	// reports newly-sent bytes, not a running total, so the caller can
	// feed it straight into a progress tracker.
	Progress func(n int64)
}

// Put uploads Body as Key, carrying Metadata as user metadata. Objects
// larger than multipartThreshold go up as a multipart upload with parts
// sized by partSizeFor and uploadConcurrency parts in flight at once;
// everything else goes up as one PutObject call.
func (c *Client) Put(ctx context.Context, in PutInput) error {
	if in.Key == "" {
		return fmt.Errorf("remote: put: key is required")
	}
	if in.Size < 0 {
		return fmt.Errorf("remote: put %q: negative size %d", in.Key, in.Size)
	}

	meta := in.Metadata
	meta.Size = in.Size
	metaMap := meta.ToS3()

	if in.Size > c.threshold() {
		// Resumable when there is somewhere to write the upload id down and
		// the source can be read out of order -- an *os.File, which is what
		// the engine passes. Without both, the old all-or-nothing path,
		// which is correct and merely wasteful after an interruption.
		if c.resume != nil && canSeek(in.Body) {
			return c.putResumable(ctx, in.Key, in.Body, in.Size, in.Metadata.ModTime.UnixNano(),
				metaMap, in.ContentType, in.Progress)
		}
		body := withProgress(in.Body, in.Progress)
		return explain(c.putMultipart(ctx, in.Key, body, in.Size, metaMap, in.ContentType), body, in.Size)
	}

	// Seekability is preserved where the caller had it: the SDK rewinds a
	// body to retry a request, and a wrapper that hides *os.File's Seek
	// turns every retry into a short send against a Content-Length that
	// still describes the whole file. See progressSeeker.
	body := withProgress(in.Body, in.Progress)
	return explain(c.putSingle(ctx, in.Key, body, in.Size, metaMap, in.ContentType), body, in.Size)
}

// canSeek reports whether a body can be read out of order, which is what
// sending part seven without first sending parts one to six requires.
func canSeek(body io.Reader) bool {
	if _, ok := body.(io.ReaderAt); ok {
		return true
	}
	_, ok := body.(io.Seeker)
	return ok
}

// explain adds what the request itself could not say.
//
// A body that stops early is truncated by Go's transport, and an S3 server
// then answers on its own terms -- a 400 IncompleteBody, because it was
// promised a Content-Length it never received. That is what the SDK returns,
// and it describes the symptom from the far end of a wire: it names neither
// the file that would not read nor how far it got. Both are known here.
//
// Size comes from a Stat taken before the file was opened, so the honest
// reading of a short body is usually that the file shrank underneath us --
// which is a thing that happens to a backup tool constantly, and a thing the
// caller can act on. It should not arrive dressed as a protocol error.
func explain(err error, body io.Reader, size int64) error {
	if err == nil {
		return nil
	}
	c, ok := body.(counter)
	if !ok {
		return err
	}
	switch n, readErr := c.sent(); {
	case readErr != nil:
		return fmt.Errorf("%w (the source stopped reading after %d of %d bytes: %v)", err, n, size, readErr)
	case n < size:
		return fmt.Errorf("%w (only %d of the %d bytes promised could be read; the file was most likely "+
			"changed or truncated while it was being uploaded)", err, n, size)
	default:
		return err
	}
}

func (c *Client) putSingle(ctx context.Context, key string, body io.Reader, size int64, meta map[string]string, contentType string) error {
	input := &s3.PutObjectInput{
		Bucket:        aws.String(c.bucket),
		Key:           aws.String(key),
		Body:          body,
		ContentLength: aws.Int64(size),
		Metadata:      meta,
	}
	if contentType != "" {
		input.ContentType = aws.String(contentType)
	}
	if _, err := c.s3.PutObject(ctx, input); err != nil {
		return fmt.Errorf("remote: put %q: %w", key, err)
	}
	return nil
}

func (c *Client) putMultipart(ctx context.Context, key string, body io.Reader, size int64, meta map[string]string, contentType string) error {
	input := &s3.PutObjectInput{
		Bucket:   aws.String(c.bucket),
		Key:      aws.String(key),
		Body:     body,
		Metadata: meta,
	}
	if contentType != "" {
		input.ContentType = aws.String(contentType)
	}

	partSize := partSizeFor(size, defaultPartSize)
	_, err := c.uploader.Upload(ctx, input, func(u *manager.Uploader) {
		u.PartSize = partSize
		u.Concurrency = uploadConcurrency
	})
	if err != nil {
		return fmt.Errorf("remote: multipart put %q: %w", key, err)
	}
	return nil
}

// partSizeFor picks the multipart part size for an object of the given
// size. defaultPartSize (16MiB) parts are used as long as that keeps the
// part count at or under maxParts (R2's 10,000-part cap). Past that point --
// above roughly 156GiB -- 16MiB parts would need more than 10,000 pieces
// and R2 would reject the upload outright, so the part size grows instead
// of failing: bigger parts, same cap, same number of round trips it would
// have taken at the boundary.
func partSizeFor(size, base int64) int64 {
	partSize := base
	if partSize <= 0 {
		partSize = defaultPartSize
	}
	if size <= 0 {
		return partSize
	}
	numParts := (size + partSize - 1) / partSize
	if numParts <= maxParts {
		return partSize
	}

	partSize = (size + maxParts - 1) / maxParts
	// Round up to the next whole MiB for tidy, predictable part boundaries
	// rather than an odd byte count.
	const mib = int64(1 << 20)
	if rem := partSize % mib; rem != 0 {
		partSize += mib - rem
	}
	if partSize < manager.MinUploadPartSize {
		partSize = manager.MinUploadPartSize
	}
	return partSize
}
