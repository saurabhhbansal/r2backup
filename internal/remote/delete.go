package remote

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// Delete removes a single key. Deleting a key that does not exist is not an
// error: that is S3's own semantics, not something this wrapper adds, so
// callers never need to Head before Delete just to avoid one.
func (c *Client) Delete(ctx context.Context, key string) error {
	_, err := c.s3.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(c.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return fmt.Errorf("remote: delete %q: %w", key, err)
	}
	return nil
}

// deleteBatchLimit is S3's own cap on how many keys one DeleteObjects call
// may carry.
const deleteBatchLimit = 1000

// DeleteBatch removes many keys, batching them into deleteBatchLimit-sized
// DeleteObjects calls so clearing tens of thousands of trashed keys does not
// cost one round trip per key.
func (c *Client) DeleteBatch(ctx context.Context, keys []string) error {
	for start := 0; start < len(keys); start += deleteBatchLimit {
		end := start + deleteBatchLimit
		if end > len(keys) {
			end = len(keys)
		}
		chunk := keys[start:end]

		ids := make([]types.ObjectIdentifier, len(chunk))
		for i, k := range chunk {
			ids[i] = types.ObjectIdentifier{Key: aws.String(k)}
		}

		out, err := c.s3.DeleteObjects(ctx, &s3.DeleteObjectsInput{
			Bucket: aws.String(c.bucket),
			Delete: &types.Delete{
				Objects: ids,
				// Quiet suppresses a per-key success entry in the
				// response; only failures are worth a round trip's worth
				// of payload for a batch this size.
				Quiet: aws.Bool(true),
			},
		})
		if err != nil {
			return fmt.Errorf("remote: delete batch (%d keys): %w", len(chunk), err)
		}
		if len(out.Errors) > 0 {
			first := out.Errors[0]
			return fmt.Errorf("remote: delete batch: %d of %d keys failed, first %q: %s",
				len(out.Errors), len(chunk), aws.ToString(first.Key), aws.ToString(first.Message))
		}
	}
	return nil
}
