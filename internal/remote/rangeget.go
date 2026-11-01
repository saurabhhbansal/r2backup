package remote

import (
	"context"
	"fmt"
	"io"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// RangeReader is a Store that can read part of an object. Bundled backups
// keep many files in one archive, and reading one file back should not mean
// downloading the archive. A store that cannot (Drime) does not implement it,
// and the caller fetches the whole object instead.
type RangeReader interface {
	// GetRange streams n bytes of key starting at off.
	GetRange(ctx context.Context, key string, off, n int64) (io.ReadCloser, error)
}

// GetRange reads n bytes of key from off with an HTTP Range request.
func (c *Client) GetRange(ctx context.Context, key string, off, n int64) (io.ReadCloser, error) {
	if n <= 0 {
		return io.NopCloser(eofReader{}), nil
	}
	out, err := c.s3.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(c.bucket),
		Key:    aws.String(key),
		Range:  aws.String(fmt.Sprintf("bytes=%d-%d", off, off+n-1)),
	})
	if err != nil {
		if isNotFound(err) {
			return nil, fmt.Errorf("remote: get %q: %w", key, ErrNotFound)
		}
		return nil, fmt.Errorf("remote: get %q range %d+%d: %w", key, off, n, err)
	}
	return out.Body, nil
}

type eofReader struct{}

func (eofReader) Read([]byte) (int, error) { return 0, io.EOF }

var _ RangeReader = (*Client)(nil)
