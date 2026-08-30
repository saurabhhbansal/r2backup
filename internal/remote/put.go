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

	// Seekability is preserved where the caller had it: the SDK rewinds a
	// body to retry a request, and a wrapper that hides *os.File's Seek
	// turns every retry into a short send against a Content-Length that
	// still describes the whole file. See progressSeeker.
	body := withProgress(in.Body, in.Progress)

	meta := in.Metadata
	meta.Size = in.Size
	metaMap := meta.ToS3()

	if in.Size > multipartThreshold {
		return c.putMultipart(ctx, in.Key, body, in.Size, metaMap, in.ContentType)
	}
	return c.putSingle(ctx, in.Key, body, in.Size, metaMap, in.ContentType)
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

	partSize := partSizeFor(size)
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
func partSizeFor(size int64) int64 {
	partSize := int64(defaultPartSize)
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
